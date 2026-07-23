package host_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrn-dk/latigo/abi"
	"github.com/mrn-dk/latigo/events"
	"github.com/mrn-dk/latigo/host"
)

// TestCheckpointOnTerminateAndReactivate exercises the durable-actor lifecycle:
//  1. an agent runs to completion and, on terminate, emits a checkpoint that
//     captures its finished state (Feature 1);
//  2. the host parks it (stores the blob) and later reactivates it with a new
//     task; the agent clears its terminal state, carries its transcript forward,
//     and produces a new result (Feature 2).
func TestCheckpointOnTerminateAndReactivate(t *testing.T) {
	wasm := buildGuest(t)
	dir := t.TempDir()

	// --- activation 1: run to completion ---
	logPath := filepath.Join(dir, "a1.jsonl")
	log, err := host.OpenEventLog(logPath, "test")
	if err != nil {
		t.Fatal(err)
	}
	h := newActorHost(t, dir, "ws1", log)
	bashArgs, _ := json.Marshal(map[string]string{"script": "echo hi"})
	(&host.MockLLM{Turns: []abi.LLMMessage{
		{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "c1", Name: "bash", Arguments: string(bashArgs)}}},
	}}).Register(h)
	h.Checkpoints(nil)

	var out bytes.Buffer
	if err := h.Run(context.Background(), host.RunConfig{
		Wasm: wasm, Goal: "act1 goal", MaxTurns: 8, Stdout: &out, Stderr: &out,
	}); err != nil {
		t.Fatalf("activation 1: %v\n%s", err, out.String())
	}
	log.Close()

	// Feature 1: a terminal checkpoint capturing the completed state must exist.
	blob := lastCheckpointState(t, logPath)
	var snap struct {
		Done    bool   `json:"done"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(blob, &snap); err != nil {
		t.Fatalf("checkpoint blob: %v", err)
	}
	if !snap.Done {
		t.Fatalf("terminal checkpoint should capture done=true, got %s", blob)
	}

	// --- activation 2: reactivate the parked agent with a new task ---
	h2 := newActorHost(t, dir, "ws2", nil)
	var seenMessages []abi.LLMMessage
	h2.Handle(abi.OpLLMCall, func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
		var req abi.LLMCallRequest
		_ = json.Unmarshal(args, &req)
		seenMessages = req.Messages
		resp := abi.LLMCallResponse{
			Message:      abi.LLMMessage{Role: "assistant", Content: "second result"},
			FinishReason: "stop",
		}
		b, _ := json.Marshal(resp)
		return b, nil
	})
	var sent []string
	h2.Messaging(host.Messenger{Out: func(_, c string) { sent = append(sent, c) }})
	h2.Reactivate(blob, "now write a report")

	var out2 bytes.Buffer
	if err := h2.Run(context.Background(), host.RunConfig{
		Wasm: wasm, Goal: "ignored on reactivation", MaxTurns: 8, Stdout: &out2, Stderr: &out2,
	}); err != nil {
		t.Fatalf("activation 2: %v\n%s", err, out2.String())
	}

	// Feature 2: the reactivated agent produced a new result...
	if len(sent) == 0 || sent[len(sent)-1] != "second result" {
		t.Fatalf("reactivation result = %v, want 'second result'", sent)
	}
	// ...and its transcript carried the prior activation forward plus the new task.
	joined := ""
	for _, m := range seenMessages {
		joined += m.Content + "\n"
	}
	if !strings.Contains(joined, "act1 goal") {
		t.Errorf("reactivation transcript lost prior context:\n%s", joined)
	}
	if !strings.Contains(joined, "now write a report") {
		t.Errorf("reactivation transcript missing the new task:\n%s", joined)
	}
}

// --- small host builder for the durable-actor tests ---

func newActorHost(t *testing.T, dir, ws string, log *host.EventLog) *host.Host {
	t.Helper()
	h := host.New(abi.Capabilities{FSWrite: true, HostVersion: "test"}, log)
	if err := h.FS(filepath.Join(dir, ws), true); err != nil {
		t.Fatal(err)
	}
	h.Clock(nil)
	h.Rand(nil)
	h.Log(discard{})
	h.Messaging(host.Messenger{})
	h.Tools(host.NewStaticTools())
	return h
}

func lastCheckpointState(t *testing.T, path string) json.RawMessage {
	t.Helper()
	evs, err := host.ReadEvents(path)
	if err != nil {
		t.Fatal(err)
	}
	var state json.RawMessage
	found := false
	for _, ev := range evs {
		if ev.Kind == events.KindCheckpoint {
			var cp events.Checkpoint
			if err := json.Unmarshal(ev.Payload, &cp); err != nil {
				t.Fatal(err)
			}
			state, found = cp.State, true
		}
	}
	if !found {
		t.Fatal("no checkpoint event found (Feature 1: checkpoint-on-terminate missing)")
	}
	return state
}
