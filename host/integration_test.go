package host_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrn-dk/latigo/abi"
	"github.com/mrn-dk/latigo/events"
	"github.com/mrn-dk/latigo/host"
)

// buildGuest compiles the guest to wasm into a temp file, or skips.
func buildGuest(t *testing.T) []byte {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	out := filepath.Join(t.TempDir(), "latigo.wasm")
	cmd := exec.Command("go", "build", "-o", out, "../cmd/latigo-guest")
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build guest wasm: %v\n%s", err, b)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestGuestRunAndReplay runs the real WASM guest end-to-end against a mock LLM,
// then replays the recorded event log and asserts the reconstruction matches.
func TestGuestRunAndReplay(t *testing.T) {
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
	var sent []string
	h.Messaging(host.Messenger{Out: func(_, c string) { sent = append(sent, c) }})
	h.Tools(host.NewStaticTools())
	host.ScriptedMockLLM("integration").Register(h)

	var stdout bytes.Buffer
	if err := h.Run(context.Background(), host.RunConfig{
		Wasm: wasm, Goal: "integration", MaxTurns: 8, Stdout: &stdout, Stderr: &stdout,
	}); err != nil {
		t.Fatalf("run: %v\n%s", err, stdout.String())
	}
	log.Close()

	if len(sent) == 0 || sent[len(sent)-1] != "completed: integration" {
		t.Fatalf("unexpected result message: %v", sent)
	}

	// The log must contain run_start, hostcalls, and run_end.
	evs, err := host.ReadEvents(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var haveStart, haveEnd, haveHostcall bool
	for _, ev := range evs {
		switch ev.Kind {
		case events.KindRunStart:
			haveStart = true
		case events.KindRunEnd:
			haveEnd = true
		case events.KindHostcall:
			haveHostcall = true
		}
	}
	if !haveStart || !haveEnd || !haveHostcall {
		t.Fatalf("log missing required events: start=%v end=%v hostcall=%v", haveStart, haveEnd, haveHostcall)
	}

	// --- replay: no real handlers, results come from the log ---
	rh := host.New(abi.Capabilities{FSWrite: true}, nil)
	if err := rh.LoadReplay(evs); err != nil {
		t.Fatal(err)
	}
	// Replay returns recorded results without invoking handlers, so the guest
	// reconstructs the same state and prints the same summary to stdout.
	var rout bytes.Buffer
	if err := rh.Run(context.Background(), host.RunConfig{
		Wasm: wasm, Goal: "integration", MaxTurns: 8, Stdout: &rout, Stderr: &rout,
	}); err != nil {
		t.Fatalf("replay: %v\n%s", err, rout.String())
	}
	if !bytes.Contains(rout.Bytes(), []byte("completed: integration")) {
		t.Fatalf("replay produced different result: %q", rout.String())
	}
}

// oaiToolCallBody builds an OpenAI-shaped chat-completions response body
// carrying a single tool call, matching what host.LLMClient's wire decoder
// expects.
func oaiToolCallBody(id, name string, args map[string]string) []byte {
	argsJSON, _ := json.Marshal(args)
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{
			{
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{
						{
							"id":   id,
							"type": "function",
							"function": map[string]any{
								"name":      name,
								"arguments": string(argsJSON),
							},
						},
					},
				},
				"finish_reason": "tool_calls",
			},
		},
	})
	return b
}

// TestGuestRunAndReplay_RateLimitedThenSucceeded runs the real WASM guest
// against a real host.LLMClient (not the mock) whose first HTTP round trip
// returns 429, then succeeds. Because host.LLMClient retries internally,
// below the durability boundary, a single llm.call hostcall must still
// produce exactly one recorded event — retries are invisible to the log — and
// replay must reconstruct the same run without any network access.
func TestGuestRunAndReplay_RateLimitedThenSucceeded(t *testing.T) {
	wasm := buildGuest(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	goal := "retry-integration"

	var httpCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls++
		switch httpCalls {
		case 1:
			// First attempt: rate-limited. No Retry-After, so the client falls
			// back to its (fast, jittered) exponential base delay.
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
		case 2:
			// Retry succeeds: first guest turn, a bash tool call.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(oaiToolCallBody("c1", "bash", map[string]string{"script": "echo hello from latigo"}))
		default:
			// Second guest turn: finish.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(oaiToolCallBody("c2", "done", map[string]string{"summary": "completed: " + goal}))
		}
	}))
	defer srv.Close()

	llm := host.NewLLMClient(srv.URL, "", "test-model")
	llm.Retry.BaseDelay = 1 * time.Millisecond
	llm.Retry.MaxDelay = 5 * time.Millisecond

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
	var sent []string
	h.Messaging(host.Messenger{Out: func(_, c string) { sent = append(sent, c) }})
	h.Tools(host.NewStaticTools())
	h.LLM(llm)

	var stdout bytes.Buffer
	if err := h.Run(context.Background(), host.RunConfig{
		Wasm: wasm, Goal: goal, MaxTurns: 8, Stdout: &stdout, Stderr: &stdout,
	}); err != nil {
		t.Fatalf("run: %v\n%s", err, stdout.String())
	}
	log.Close()

	if len(sent) == 0 || sent[len(sent)-1] != "completed: "+goal {
		t.Fatalf("unexpected result message: %v", sent)
	}
	if httpCalls != 3 {
		t.Fatalf("stub server saw %d HTTP round trips, want 3 (1 rate-limited + 2 successful turns)", httpCalls)
	}

	evs, err := host.ReadEvents(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var llmHostcalls int
	for _, ev := range evs {
		if ev.Kind != events.KindHostcall {
			continue
		}
		var hc events.Hostcall
		if err := json.Unmarshal(ev.Payload, &hc); err != nil {
			t.Fatal(err)
		}
		if hc.Op == abi.OpLLMCall {
			llmHostcalls++
		}
	}
	// Two guest turns (bash, then done) -> exactly two recorded llm.call
	// events, even though the underlying HTTP transport made three round
	// trips. The 429-then-200 retry collapses into a single recorded result.
	if llmHostcalls != 2 {
		t.Fatalf("recorded %d llm.call hostcalls, want 2 (retries must not be individually recorded)", llmHostcalls)
	}

	// --- replay: no real handlers (and no network), results come from the log ---
	rh := host.New(abi.Capabilities{FSWrite: true}, nil)
	if err := rh.LoadReplay(evs); err != nil {
		t.Fatal(err)
	}
	var rout bytes.Buffer
	if err := rh.Run(context.Background(), host.RunConfig{
		Wasm: wasm, Goal: goal, MaxTurns: 8, Stdout: &rout, Stderr: &rout,
	}); err != nil {
		t.Fatalf("replay: %v\n%s", err, rout.String())
	}
	if !bytes.Contains(rout.Bytes(), []byte("completed: "+goal)) {
		t.Fatalf("replay produced different result: %q", rout.String())
	}
}
