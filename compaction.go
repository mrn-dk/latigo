// Package main: compaction.go manages the agent's own context window (spec
// §2.5 — max_total_tokens implies the harness must keep the transcript
// bounded). These are plain overridable strategy points on *Agent; the loop
// consults ShouldCompact before each turn and applies Compact when it fires.
package main

import (
	"context"
	"fmt"
)

// estimateTokens is a cheap proxy for token count: 4 chars per token, rounded
// up, summed over message content + tool-call arguments. It over-counts
// slightly, which keeps compaction conservative. The real count comes back
// from the endpoint in the llm event and drives the hard max_total_tokens limit.
func estimateTokens(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += (len(m.Content) + 3) / 4
		for _, p := range m.Parts {
			if p.Type == "text" {
				n += (len(p.Text) + 3) / 4
			} else if p.ImageURL != nil {
				n += 256 // rough image placeholder cost
			}
		}
		for _, tc := range m.ToolCalls {
			n += (len(tc.Function.Name) + len(tc.Function.Arguments) + 3) / 4
		}
	}
	return n
}

// defaultCompact keeps the system prompt and the first user turn (pinned
// context), replaces the middle with a deterministic placeholder summary, and
// keeps the most recent turns verbatim. No LLM call, so it's free and
// trivially consistent with the log.
func defaultCompact(a *Agent, msgs []Message) []Message {
	if len(msgs) <= 6 {
		return msgs
	}
	head := msgs[:2]
	mid := msgs[2 : len(msgs)-4]
	tail := msgs[len(msgs)-4:]
	summary := windowSummary(mid)
	out := make([]Message, 0, len(head)+1+len(tail))
	out = append(out, head...)
	out = append(out, summary)
	out = append(out, tail...)
	return out
}

// windowSummary produces a deterministic placeholder for the elided turns. It
// notes how many turns were folded and counts tool calls, so the model knows
// what happened without a summary costing tokens.
func windowSummary(mid []Message) Message {
	tools := 0
	for _, m := range mid {
		tools += len(m.ToolCalls)
	}
	text := fmt.Sprintf("[transcript compacted: %d prior messages elided, %d tool calls; see the event log for full history]", len(mid), tools)
	return Message{Role: "system", Content: text}
}

// llmCompact is the model-driven summarisation strategy: ask the model to
// write a structured briefing of the elided middle. Because that llm call is
// recorded in the event log, resume stays consistent — the summary becomes
// part of the transcript.
func llmCompact(a *Agent, msgs []Message) []Message {
	if len(msgs) <= 6 {
		return msgs
	}
	head := msgs[:2]
	mid := msgs[2 : len(msgs)-4]
	tail := msgs[len(msgs)-4:]
	summary, err := summarizeWithLLM(a, mid)
	if err != nil {
		// Degrade to the deterministic placeholder on any error.
		summary = windowSummary(mid)
	}
	out := make([]Message, 0, len(head)+1+len(tail))
	out = append(out, head...)
	out = append(out, summary)
	out = append(out, tail...)
	return out
}

// summarizeWithLLM asks the endpoint for a structured briefing of the elided turns.
func summarizeWithLLM(a *Agent, mid []Message) (Message, error) {
	ctx := context.Background()
	prompt := []Message{
		{Role: "system", Content: "Summarise the following agent turns in a concise briefing: files touched, decisions made, task state, and any TODOs. Plain text only."},
		{Role: "user", Content: transcriptText(mid)},
	}
	resp, err := a.llm.Call(ctx, chatRequest{Model: a.cfg.Model, Messages: wireMessages(prompt)})
	if err != nil {
		return Message{}, err
	}
	return Message{Role: "system", Content: "[compacted summary]\n" + resp.Message.Content}, nil
}

// transcriptText flattens a message slice into readable text for summarisation.
func transcriptText(msgs []Message) string {
	var b []byte
	for _, m := range msgs {
		b = append(b, m.Role...)
		b = append(b, ": "...)
		if m.Content != "" {
			b = append(b, m.Content...)
		}
		for _, tc := range m.ToolCalls {
			b = append(b, fmt.Sprintf(" [tool %s(%s)]", tc.Function.Name, tc.Function.Arguments)...)
		}
		b = append(b, '\n')
	}
	return string(b)
}
