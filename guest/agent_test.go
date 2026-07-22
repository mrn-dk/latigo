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
	cp := agent.Checkpoint()
	var snap map[string]any
	if err := json.Unmarshal(cp, &snap); err != nil {
		t.Fatalf("checkpoint not valid JSON: %v", err)
	}
	if _, ok := snap["messages"]; !ok {
		t.Error("checkpoint missing messages")
	}
}
