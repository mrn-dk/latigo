package host_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/mrn-dk/latigo/abi"
	"github.com/mrn-dk/latigo/events"
	"github.com/mrn-dk/latigo/host"
)

// scriptedApprovalSteerLLM returns a MockLLM that: calls a gated tool
// (http_fetch, expected to be denied), then an ungated tool (bash, expected
// to run), then finishes. It exercises both the approval-gating and
// steering-injection paths in the same run.
func scriptedApprovalSteerLLM() *host.MockLLM {
	fetchArgs, _ := json.Marshal(map[string]string{"url": "http://example.com/secret"})
	bashArgs, _ := json.Marshal(map[string]string{"script": "echo steered > /work/note.txt"})
	doneArgs, _ := json.Marshal(map[string]string{"summary": "finished"})
	return &host.MockLLM{Turns: []abi.LLMMessage{
		{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "c1", Name: "http_fetch", Arguments: string(fetchArgs)}}},
		{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "c2", Name: "bash", Arguments: string(bashArgs)}}},
		{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "c3", Name: "done", Arguments: string(doneArgs)}}},
	}}
}

// TestApprovalAndSteerReplay runs the real WASM guest through a scripted turn
// sequence that includes a denied tool call (approval gating) and an injected
// steering message, then replays the recorded event log from scratch and
// asserts the reconstruction reproduces the same transcript byte-for-byte —
// proving the human/host decisions become part of the durable, replayable
// record exactly as the spec requires.
func TestApprovalAndSteerReplay(t *testing.T) {
	wasm := buildGuest(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")

	// --- live run ---
	log, err := host.OpenEventLog(logPath, "test")
	if err != nil {
		t.Fatal(err)
	}
	h := host.New(abi.Capabilities{FSWrite: true}, log)
	if err := h.FS(filepath.Join(dir, "ws"), true); err != nil {
		t.Fatal(err)
	}
	h.Clock(nil)
	h.Rand(nil)
	h.Log(discard{})
	// Deny every gated action.
	h.Approval(func(action string, details json.RawMessage) (bool, string) {
		return false, "policy: network access denied"
	})
	// Serve exactly one steering message, on the first msg.recv("steer") poll.
	steered := false
	var sent []string
	h.Messaging(host.Messenger{
		Out: func(_, c string) { sent = append(sent, c) },
		In: func(channel string, blocking bool) (string, bool) {
			if channel == "steer" && !steered {
				steered = true
				return "focus on the workspace", true
			}
			return "", false
		},
	})
	h.Tools(host.NewStaticTools())
	scriptedApprovalSteerLLM().Register(h)

	var stdout bytes.Buffer
	if err := h.Run(context.Background(), host.RunConfig{
		Wasm: wasm, Goal: "steer-approval", MaxTurns: 8, Stdout: &stdout, Stderr: &stdout,
	}); err != nil {
		t.Fatalf("run: %v\n%s", err, stdout.String())
	}
	log.Close()

	if len(sent) == 0 || sent[len(sent)-1] != "finished" {
		t.Fatalf("unexpected result message: %v", sent)
	}

	evs, err := host.ReadEvents(logPath)
	if err != nil {
		t.Fatal(err)
	}

	// The recorded log must show both an approval.await and a msg.recv
	// hostcall (the denial and the steering message actually happened and
	// were durably recorded, not just observed in-process).
	var haveApproval, haveMsgRecv int
	for _, ev := range evs {
		if ev.Kind != events.KindHostcall {
			continue
		}
		var hc struct {
			Op abi.Op `json:"op"`
		}
		_ = json.Unmarshal(ev.Payload, &hc)
		switch hc.Op {
		case abi.OpApprovalAwait:
			haveApproval++
		case abi.OpMsgRecv:
			haveMsgRecv++
		}
	}
	if haveApproval == 0 {
		t.Error("expected at least one recorded approval.await hostcall")
	}
	if haveMsgRecv == 0 {
		t.Error("expected at least one recorded msg.recv hostcall")
	}

	// --- replay: no real handlers are registered, results come from the log.
	// The replay host must advertise the same capability set the live run
	// negotiated (Approval + Steer), or the guest will skip the
	// approval.await/msg.recv hostcalls the log expects next and replay will
	// diverge.
	rh := host.New(abi.Capabilities{FSWrite: true, Approval: true, Steer: true}, nil)
	if err := rh.LoadReplay(evs); err != nil {
		t.Fatal(err)
	}
	var rout bytes.Buffer
	if err := rh.Run(context.Background(), host.RunConfig{
		Wasm: wasm, Goal: "steer-approval", MaxTurns: 8, Stdout: &rout, Stderr: &rout,
	}); err != nil {
		t.Fatalf("replay: %v\n%s", err, rout.String())
	}

	if !bytes.Equal(stdout.Bytes(), rout.Bytes()) {
		t.Fatalf("replay transcript diverged:\nlive:   %q\nreplay: %q", stdout.String(), rout.String())
	}
}
