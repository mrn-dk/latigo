package guest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrn-dk/latigo/abi"
)

// emptyLLMTransport returns an empty llm.call result, forcing llmSummarize to
// fall back to the deterministic placeholder.
type emptyLLMTransport struct{}

func (emptyLLMTransport) Hostcall(req abi.Request) (abi.Response, error) {
	if req.Op == abi.OpLLMCall {
		b, _ := json.Marshal(abi.LLMCallResponse{Message: abi.LLMMessage{Role: "assistant", Content: ""}})
		return abi.Response{Result: b}, nil
	}
	return abi.Response{Error: "unsupported", Code: abi.ErrUnsupported}, nil
}

func longTranscript(n int) []abi.LLMMessage {
	msgs := []abi.LLMMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "goal"},
	}
	for i := 0; i < n; i++ {
		msgs = append(msgs,
			abi.LLMMessage{Role: "assistant", Content: "step"},
			abi.LLMMessage{Role: "tool", Content: "output"},
		)
	}
	return msgs
}

func TestEstimateTokens(t *testing.T) {
	msgs := []abi.LLMMessage{{Role: "user", Content: strings.Repeat("x", 400)}}
	got := estimateTokens(msgs)
	// ~ (400 + overhead)/4; assert it's in a sane ballpark, not zero/huge.
	if got < 90 || got > 120 {
		t.Fatalf("estimateTokens = %d, want ~100", got)
	}
}

func TestBudgetTriggerCompaction(t *testing.T) {
	client := NewClient(&fakeTransport{})
	// A small token budget should trigger compaction well before the 40-message
	// fallback threshold.
	a := NewAgent(Config{MaxTurns: 1, Capabilities: abi.Capabilities{MaxLLMTokens: 100}}, client)
	if !a.ShouldCompact(longTranscript(20)) {
		t.Fatal("expected budget-based ShouldCompact to fire")
	}
	// With no budget advertised, it falls back to the message-count threshold.
	b := NewAgent(Config{MaxTurns: 1}, client)
	if b.ShouldCompact(longTranscript(3)) {
		t.Fatal("small transcript should not compact under the count fallback")
	}
}

func TestDefaultCompactWindow(t *testing.T) {
	client := NewClient(&fakeTransport{})
	a := NewAgent(Config{MaxTurns: 1}, client) // window strategy (default)
	in := longTranscript(10)                   // 22 messages
	out := a.Compact(a, in)
	// head(2) + 1 summary + tail(4)
	if len(out) != 7 {
		t.Fatalf("compacted to %d messages, want 7", len(out))
	}
	if !strings.Contains(out[2].Content, "compacted for brevity") {
		t.Errorf("expected deterministic placeholder summary, got %q", out[2].Content)
	}
}

func TestLLMCompactionUsesModel(t *testing.T) {
	// A fake transport whose llm.call returns a canned summary.
	ft := &fakeTransport{llmTurns: []abi.LLMMessage{
		{Role: "assistant", Content: "STRUCTURED SUMMARY: goal X, file Y written"},
	}}
	client := NewClient(ft)
	a := NewAgent(Config{MaxTurns: 1, Compaction: CompactionLLM}, client)

	out := a.Compact(a, longTranscript(10))
	if len(out) != 7 {
		t.Fatalf("compacted to %d messages, want 7", len(out))
	}
	if !strings.Contains(out[2].Content, "STRUCTURED SUMMARY") {
		t.Errorf("expected model-produced summary, got %q", out[2].Content)
	}
	if ft.llmIdx == 0 {
		t.Error("llm compaction did not issue an llm.call")
	}
}

func TestLLMCompactionFallsBack(t *testing.T) {
	// Transport that returns empty content for llm.call -> deterministic fallback.
	client := NewClient(emptyLLMTransport{})
	a := NewAgent(Config{MaxTurns: 1, Compaction: CompactionLLM}, client)
	out := a.Compact(a, longTranscript(10))
	if !strings.Contains(out[2].Content, "compacted for brevity") {
		t.Errorf("expected placeholder fallback, got %q", out[2].Content)
	}
}
