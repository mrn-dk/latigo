package guest

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mrn-dk/latigo/abi"
)

// Agent is the in-guest agent loop. It wires the LLM, the tool registry, the
// virtual bash + VFS, skills, and Starlark scripts together, with configurable
// strategy points for compaction and termination.
type Agent struct {
	cfg    Config
	client *Client
	tools  *Registry
	vfs    *VFS
	bash   *Bash
	skills *Skills
	script *ScriptRunner

	messages []abi.LLMMessage
	done     bool
	summary  string

	// Strategy points (overridable).
	SystemPrompt string
	// ShouldCompact decides whether to compact the transcript before a turn.
	ShouldCompact func(msgs []abi.LLMMessage) bool
	// Compact rewrites the transcript (e.g. summarising older turns).
	Compact func(a *Agent, msgs []abi.LLMMessage) []abi.LLMMessage
	// ShouldStop decides whether to terminate after a turn.
	ShouldStop func(a *Agent, turn int) bool
}

// NewAgent constructs an agent from config and a client.
func NewAgent(cfg Config, client *Client) *Agent {
	vfs := NewVFS()
	// Only hand the shell a network fetcher when the host granted the HTTP
	// capability; otherwise curl/wget report no network rather than issuing a
	// hostcall that would just fail.
	var fetch Fetcher
	if cfg.Capabilities.HTTP {
		fetch = client
	}
	a := &Agent{
		cfg:    cfg,
		client: client,
		tools:  NewRegistry(client),
		vfs:    vfs,
		bash:   NewBash(vfs, fetch),
		skills: NewSkills(vfs),
		script: NewScriptRunner(ScriptBudget{}),
	}
	a.SystemPrompt = defaultSystemPrompt
	a.ShouldCompact = func(msgs []abi.LLMMessage) bool { return len(msgs) > 40 }
	a.Compact = defaultCompact
	a.ShouldStop = func(ag *Agent, turn int) bool { return ag.done || turn >= ag.cfg.MaxTurns }
	a.registerBuiltins()
	return a
}

// VFS exposes the agent's virtual filesystem for seeding.
func (a *Agent) VFS() *VFS { return a.vfs }

// Skills exposes the skills provider for seeding.
func (a *Agent) Skills() *Skills { return a.skills }

// Tools exposes the tool registry.
func (a *Agent) Tools() *Registry { return a.tools }

const defaultSystemPrompt = `You are Latigo, a durable agent running in a WebAssembly sandbox.
You have a virtual filesystem and a bash-like shell, on-demand skills, and a set of tools.
Work toward the user's goal using tools. When finished, call the "done" tool with a summary.
Be concise. Prefer the bash tool for file manipulation and inspection.`

// Run executes the agent loop until termination and returns a final summary.
func (a *Agent) Run(ctx context.Context) (string, error) {
	if _, err := a.tools.RefreshHostCatalog(); err != nil {
		return "", err
	}

	a.messages = []abi.LLMMessage{
		{Role: "system", Content: a.SystemPrompt},
		{Role: "user", Content: a.cfg.Goal},
	}

	for turn := 0; ; turn++ {
		if a.ShouldStop(a, turn) {
			break
		}
		if a.ShouldCompact(a.messages) {
			a.messages = a.Compact(a, a.messages)
		}

		resp, err := a.client.LLMCall(abi.LLMCallRequest{
			Model:     a.cfg.Model,
			Messages:  a.messages,
			Tools:     a.tools.Specs(),
			MaxTokens: a.cfg.Capabilities.MaxLLMTokens,
		})
		if err != nil {
			return "", fmt.Errorf("llm.call: %w", err)
		}
		a.messages = append(a.messages, resp.Message)

		if len(resp.Message.ToolCalls) == 0 {
			// Model produced a final answer: terminate.
			if a.summary == "" {
				a.summary = resp.Message.Content
			}
			break
		}

		for _, tc := range resp.Message.ToolCalls {
			out, isErr := a.tools.Invoke(ctx, tc.Name, json.RawMessage(tc.Arguments))
			_ = isErr
			a.messages = append(a.messages, abi.LLMMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Name,
				Content:    out,
			})
			_ = a.client.LogAppend("info", "tool call", mustJSON(map[string]string{"tool": tc.Name}))
		}
		if a.done {
			break
		}
	}
	return a.summary, nil
}

// Checkpoint returns an opaque guest-state snapshot for the durable log.
func (a *Agent) Checkpoint() json.RawMessage {
	snap := map[string]any{
		"messages": a.messages,
		"files":    a.vfs.Snapshot(),
		"done":     a.done,
		"summary":  a.summary,
	}
	b, _ := json.Marshal(snap)
	return b
}

func defaultCompact(a *Agent, msgs []abi.LLMMessage) []abi.LLMMessage {
	if len(msgs) <= 6 {
		return msgs
	}
	// Keep system + first user message, summarise the middle, keep the tail.
	head := msgs[:2]
	tail := msgs[len(msgs)-4:]
	summary := abi.LLMMessage{
		Role:    "user",
		Content: fmt.Sprintf("[Earlier %d messages compacted for brevity.]", len(msgs)-6),
	}
	out := make([]abi.LLMMessage, 0, len(head)+1+len(tail))
	out = append(out, head...)
	out = append(out, summary)
	out = append(out, tail...)
	return out
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
