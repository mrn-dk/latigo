package guest

import (
	"fmt"

	"github.com/mrn-dk/latigo/abi"
)

// This file holds the overridable compaction strategy points and the built-in
// implementations. Compaction manages the in-context transcript (distinct from
// state.checkpoint, which snapshots durable state for the event log).
//
// The trigger (ShouldCompact), the token estimate (EstimateTokens), and the
// summariser (Summarize) are all fields on Agent, so an embedder can mix and
// match: cheap deterministic windowing, or model-driven summarisation, or their
// own policy.

// estimateTokens is a fast, dependency-free token estimate: roughly one token
// per four bytes of message content plus tool-call arguments, with a small
// per-message overhead. It is intentionally an approximation — good enough to
// drive a budget-based compaction trigger without a real tokenizer.
func estimateTokens(msgs []abi.LLMMessage) int {
	const bytesPerToken = 4
	total := 0
	for _, m := range msgs {
		n := len(m.Content) + len(m.Role) + len(m.Name)
		for _, tc := range m.ToolCalls {
			n += len(tc.Name) + len(tc.Arguments)
		}
		total += n + 16 // framing/role overhead per message
	}
	return total / bytesPerToken
}

// placeholderSummarize is the default summariser: it replaces the elided middle
// of the transcript with a single deterministic marker. No hostcall, so it is
// free and trivially replay-safe.
func placeholderSummarize(_ *Agent, old []abi.LLMMessage) abi.LLMMessage {
	return abi.LLMMessage{
		Role:    "user",
		Content: fmt.Sprintf("[Earlier %d messages compacted for brevity.]", len(old)),
	}
}

const compactionSystemPrompt = `You compress an AI agent's working transcript so it can keep going with far less context.
Produce a dense briefing that preserves everything needed to continue the task:
- the goal and current task state / plan,
- files created or modified and their key contents,
- decisions made and why,
- important command/tool outputs and results,
- open TODOs and next steps.
Be terse and factual. Omit chit-chat. Do not invent information.`

const compactionInstruction = `Summarize the conversation so far into a compact briefing per your instructions.`

// llmSummarize asks the model itself to summarise the elided turns (the strategy
// Claude Code uses). It issues an llm.call, which is a recorded hostcall — so
// the summary is captured once and returned verbatim on replay, keeping the run
// deterministic. On any error or empty result it degrades to the deterministic
// placeholder so compaction never blocks progress.
func llmSummarize(a *Agent, old []abi.LLMMessage) abi.LLMMessage {
	msgs := make([]abi.LLMMessage, 0, len(old)+2)
	msgs = append(msgs, abi.LLMMessage{Role: "system", Content: compactionSystemPrompt})
	msgs = append(msgs, old...)
	msgs = append(msgs, abi.LLMMessage{Role: "user", Content: compactionInstruction})

	resp, err := a.client.LLMCall(abi.LLMCallRequest{
		Model:     a.cfg.Model,
		Messages:  msgs,
		MaxTokens: 512,
	})
	if err != nil || resp.Message.Content == "" {
		return placeholderSummarize(a, old)
	}
	return abi.LLMMessage{
		Role:    "user",
		Content: "[Summary of earlier conversation]\n" + resp.Message.Content,
	}
}

// Compaction strategy names selectable via configuration.
const (
	CompactionWindow = "window" // deterministic sliding window (default)
	CompactionLLM    = "llm"    // model-driven summarisation
)

// selectSummarizer returns the summariser for the named strategy.
func selectSummarizer(name string) func(*Agent, []abi.LLMMessage) abi.LLMMessage {
	switch name {
	case CompactionLLM:
		return llmSummarize
	default:
		return placeholderSummarize
	}
}
