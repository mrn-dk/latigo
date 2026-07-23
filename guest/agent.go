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
	// ShouldCheckpoint decides whether to snapshot durable state at the top of a
	// turn. Only consulted when the host grants the Checkpoint capability.
	ShouldCheckpoint func(a *Agent, turn int) bool
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
	// Snapshot state every few turns so the host can compact the log to a
	// bounded tail and resume interrupted runs.
	a.ShouldCheckpoint = func(ag *Agent, turn int) bool { return turn > 0 && turn%4 == 0 }
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

	// Resume from a checkpoint when the host offers one (a compacted or
	// interrupted run); otherwise start fresh. state.restore is always the
	// guest's second startup hostcall so compaction can rely on its position.
	startTurn := 0
	skipCheckpoint := false
	restored := false
	if a.cfg.Capabilities.Checkpoint {
		if st, err := a.client.StateRestore(); err == nil && st.Found {
			if resumeTurn, ok := a.restore(st.State); ok {
				startTurn = resumeTurn
				skipCheckpoint = true // the boundary checkpoint is not re-emitted
				restored = true
			}
		}
	}
	if !restored {
		a.messages = []abi.LLMMessage{
			{Role: "system", Content: a.SystemPrompt},
			{Role: "user", Content: a.cfg.Goal},
		}
	}

	for turn := startTurn; ; turn++ {
		if a.ShouldStop(a, turn) {
			break
		}
		if a.cfg.Capabilities.Checkpoint && !skipCheckpoint && a.ShouldCheckpoint(a, turn) {
			_ = a.client.StateCheckpoint(a.checkpointState(turn))
		}
		skipCheckpoint = false
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

// agentSnapshot is the guest-defined checkpoint blob. It is opaque to the host.
type agentSnapshot struct {
	Turn     int               `json:"turn"`
	Messages []abi.LLMMessage  `json:"messages"`
	Files    map[string][]byte `json:"files"`
	Done     bool              `json:"done"`
	Summary  string            `json:"summary"`
}

// checkpointState returns an opaque, restorable snapshot of the guest for the
// durable log, taken at the top of the given turn.
func (a *Agent) checkpointState(turn int) json.RawMessage {
	b, _ := json.Marshal(agentSnapshot{
		Turn:     turn,
		Messages: a.messages,
		Files:    a.vfs.SnapshotFull(),
		Done:     a.done,
		Summary:  a.summary,
	})
	return b
}

// restore rehydrates the agent from a snapshot and returns the turn to resume
// at. ok is false if the blob cannot be decoded (the caller then starts fresh).
func (a *Agent) restore(state json.RawMessage) (int, bool) {
	var snap agentSnapshot
	if err := json.Unmarshal(state, &snap); err != nil {
		return 0, false
	}
	a.messages = snap.Messages
	a.vfs.RestoreFull(snap.Files)
	a.done = snap.Done
	a.summary = snap.Summary
	return snap.Turn, true
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
