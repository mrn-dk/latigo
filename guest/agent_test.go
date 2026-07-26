package guest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrn-dk/latigo/abi"
)

// fakeTransport implements the ABI in-process for testing the agent loop
// without a WASM host.
type fakeTransport struct {
	llmTurns []abi.LLMMessage
	llmIdx   int
	log      []string
}

func (f *fakeTransport) Hostcall(req abi.Request) (abi.Response, error) {
	switch req.Op {
	case abi.OpLLMCall:
		var msg abi.LLMMessage
		reason := "stop"
		if f.llmIdx < len(f.llmTurns) {
			msg = f.llmTurns[f.llmIdx]
			f.llmIdx++
			if len(msg.ToolCalls) > 0 {
				reason = "tool_calls"
			}
		} else {
			msg = abi.LLMMessage{Role: "assistant", Content: "done"}
		}
		return result(abi.LLMCallResponse{Message: msg, FinishReason: reason})
	case abi.OpToolList:
		return result(abi.ToolListResponse{Epoch: 1})
	case abi.OpLogAppend:
		var r abi.LogAppendRequest
		_ = json.Unmarshal(req.Args, &r)
		f.log = append(f.log, r.Message)
		return result(abi.LogAppendResponse{})
	case abi.OpClockNow:
		return result(abi.ClockNowResponse{UnixNano: 1})
	default:
		return abi.Response{Error: "unsupported", Code: abi.ErrUnsupported}, nil
	}
}

func result(v any) (abi.Response, error) {
	b, _ := json.Marshal(v)
	return abi.Response{Result: b}, nil
}

func TestAgentLoop(t *testing.T) {
	bashArgs, _ := json.Marshal(map[string]string{"script": "echo hi > /work/out.txt; cat /work/out.txt"})
	doneArgs, _ := json.Marshal(map[string]string{"summary": "all done"})
	ft := &fakeTransport{llmTurns: []abi.LLMMessage{
		{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "1", Name: "bash", Arguments: string(bashArgs)}}},
		{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "2", Name: "done", Arguments: string(doneArgs)}}},
	}}

	client := NewClient(ft)
	agent := NewAgent(Config{Goal: "test goal", MaxTurns: 8}, client)

	summary, err := agent.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary != "all done" {
		t.Errorf("summary = %q, want %q", summary, "all done")
	}
	// The bash tool should have written to the VFS.
	data, err := agent.VFS().ReadFile("/work/out.txt")
	if err != nil || strings.TrimSpace(string(data)) != "hi" {
		t.Errorf("vfs file = %q err=%v", data, err)
	}
}

func TestCheckpointRoundTrip(t *testing.T) {
	client := NewClient(&fakeTransport{})
	agent := NewAgent(Config{Goal: "g", MaxTurns: 1}, client)
	_, _ = agent.Run(context.Background())
	// Snapshot the agent, then restore it into a fresh agent and confirm the
	// transcript survives the round-trip.
	agent.messages = append(agent.messages, abi.LLMMessage{Role: "user", Content: "marker"})
	_ = agent.vfs.WriteFile("/work/keep.txt", []byte("data"))
	cp := agent.checkpointState(3)

	restored := NewAgent(Config{Goal: "g", MaxTurns: 1}, client)
	turn, ok := restored.restore(cp)
	if !ok || turn != 3 {
		t.Fatalf("restore returned turn=%d ok=%v, want 3,true", turn, ok)
	}
	if len(restored.messages) != len(agent.messages) {
		t.Errorf("restored %d messages, want %d", len(restored.messages), len(agent.messages))
	}
	if data, err := restored.vfs.ReadFile("/work/keep.txt"); err != nil || string(data) != "data" {
		t.Errorf("restored vfs file = %q err=%v", data, err)
	}
}

// erroringLLMTransport always fails llm.call with a classified host error
// (mirroring what host.LLMClient surfaces once its internal retries are
// exhausted), and answers everything else like fakeTransport's zero-turn path.
type erroringLLMTransport struct {
	code    string
	message string
	calls   int
}

func (f *erroringLLMTransport) Hostcall(req abi.Request) (abi.Response, error) {
	switch req.Op {
	case abi.OpLLMCall:
		f.calls++
		return abi.Response{Error: f.message, Code: f.code}, nil
	case abi.OpToolList:
		return result(abi.ToolListResponse{Epoch: 1})
	case abi.OpLogAppend:
		return result(abi.LogAppendResponse{})
	case abi.OpClockNow:
		return result(abi.ClockNowResponse{UnixNano: 1})
	default:
		return abi.Response{Error: "unsupported", Code: abi.ErrUnsupported}, nil
	}
}

// TestOnLLMErrorDefaultAborts verifies the default strategy preserves
// today's behaviour: an llm.call failure aborts the run with a wrapped error,
// with no fallback message and no additional retry issued.
func TestOnLLMErrorDefaultAborts(t *testing.T) {
	ft := &erroringLLMTransport{code: abi.ErrRateLimited, message: "provider is rate limiting"}
	agent := NewAgent(Config{Goal: "test goal", MaxTurns: 8}, NewClient(ft))

	summary, err := agent.Run(context.Background())
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "llm.call:") {
		t.Errorf("error = %v, want it to be wrapped as llm.call: ...", err)
	}
	if !strings.Contains(err.Error(), "provider is rate limiting") {
		t.Errorf("error = %v, want the underlying host error preserved", err)
	}
	if summary != "" {
		t.Errorf("summary = %q, want empty on abort", summary)
	}
	if ft.calls != 1 {
		t.Errorf("llm.call issued %d times, want 1 (no guest-side retry by default)", ft.calls)
	}
}

// TestOnLLMErrorFallbackTerminatesCleanly verifies an opt-in OnLLMError
// override can degrade to a fallback assistant message instead of aborting,
// and that the run terminates cleanly with that message as the summary.
func TestOnLLMErrorFallbackTerminatesCleanly(t *testing.T) {
	ft := &erroringLLMTransport{code: abi.ErrOverloaded, message: "provider unavailable"}
	agent := NewAgent(Config{Goal: "test goal", MaxTurns: 8}, NewClient(ft))

	var gotErr error
	var gotTurn int
	agent.OnLLMError = func(a *Agent, err error, turn int) (*abi.LLMMessage, bool) {
		gotErr = err
		gotTurn = turn
		return &abi.LLMMessage{Role: "assistant", Content: "stopping: provider unavailable"}, false
	}

	summary, err := agent.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary != "stopping: provider unavailable" {
		t.Errorf("summary = %q, want %q", summary, "stopping: provider unavailable")
	}
	if gotErr == nil {
		t.Error("OnLLMError was not invoked with the underlying error")
	}
	if gotTurn != 0 {
		t.Errorf("OnLLMError turn = %d, want 0", gotTurn)
	}
	if ft.calls != 1 {
		t.Errorf("llm.call issued %d times, want 1", ft.calls)
	}
}

// TestOnLLMErrorRetryReissues verifies retry=true causes the guest to
// re-issue the call exactly once per invocation, and that the loop terminates
// once the policy stops asking for a retry.
func TestOnLLMErrorRetryReissues(t *testing.T) {
	ft := &erroringLLMTransport{code: abi.ErrTimeout, message: "timed out"}
	agent := NewAgent(Config{Goal: "test goal", MaxTurns: 8}, NewClient(ft))

	attempts := 0
	agent.OnLLMError = func(a *Agent, err error, turn int) (*abi.LLMMessage, bool) {
		attempts++
		if attempts < 3 {
			return nil, true // ask the guest to re-issue
		}
		return &abi.LLMMessage{Role: "assistant", Content: "gave up after retries"}, false
	}

	summary, err := agent.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary != "gave up after retries" {
		t.Errorf("summary = %q, want %q", summary, "gave up after retries")
	}
	if ft.calls != 3 {
		t.Errorf("llm.call issued %d times, want 3", ft.calls)
	}
	if attempts != 3 {
		t.Errorf("OnLLMError invoked %d times, want 3", attempts)
	}
}
