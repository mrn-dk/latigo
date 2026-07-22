package host

import (
	"context"
	"encoding/json"

	"github.com/mrn-dk/latigo/abi"
)

// MockLLM is a deterministic, offline llm.call implementation. It is driven by
// a scripted sequence of assistant turns, which makes end-to-end runs and the
// conformance suite reproducible without a network. When the script is
// exhausted it emits a final "done" tool call echoing the goal.
type MockLLM struct {
	Turns []abi.LLMMessage
	i     int
}

// Scripted returns a MockLLM that performs a bash echo and then finishes.
func ScriptedMockLLM(goal string) *MockLLM {
	echoArgs, _ := json.Marshal(map[string]string{"script": "echo hello from latigo; ls /work"})
	doneArgs, _ := json.Marshal(map[string]string{"summary": "completed: " + goal})
	return &MockLLM{Turns: []abi.LLMMessage{
		{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "c1", Name: "bash", Arguments: string(echoArgs)}}},
		{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "c2", Name: "done", Arguments: string(doneArgs)}}},
	}}
}

// LLM registers this mock as the llm.call handler.
func (m *MockLLM) Register(h *Host) {
	h.Handle(abi.OpLLMCall, handler(func(_ context.Context, _ abi.LLMCallRequest) (abi.LLMCallResponse, error) {
		if m.i < len(m.Turns) {
			msg := m.Turns[m.i]
			m.i++
			reason := "tool_calls"
			if len(msg.ToolCalls) == 0 {
				reason = "stop"
			}
			return abi.LLMCallResponse{Message: msg, FinishReason: reason}, nil
		}
		return abi.LLMCallResponse{
			Message:      abi.LLMMessage{Role: "assistant", Content: "done"},
			FinishReason: "stop",
		}, nil
	}))
}
