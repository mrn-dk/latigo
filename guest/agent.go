package guest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
	// plan is the durable task list maintained by the "plan" built-in
	// (guest/plan.go). It is included in the checkpoint snapshot and pinned
	// into the LLM request fresh every turn (see messagesForLLM) rather than
	// living in messages, so guest/compaction.go's transcript elision never
	// touches it.
	plan []PlanItem

	// Strategy points (overridable).
	SystemPrompt string
	// ShouldCompact decides whether to compact the transcript before a turn.
	ShouldCompact func(msgs []abi.LLMMessage) bool
	// Compact rewrites the transcript (e.g. summarising older turns).
	Compact func(a *Agent, msgs []abi.LLMMessage) []abi.LLMMessage
	// EstimateTokens approximates the token footprint of a transcript, used by
	// the default budget-based ShouldCompact.
	EstimateTokens func(msgs []abi.LLMMessage) int
	// Summarize condenses the elided middle of the transcript into one message.
	// The default is a deterministic placeholder; llmSummarize (selected via the
	// "llm" compaction strategy) asks the model instead.
	Summarize func(a *Agent, old []abi.LLMMessage) abi.LLMMessage
	// ShouldStop decides whether to terminate after a turn.
	ShouldStop func(a *Agent, turn int) bool
	// ShouldCheckpoint decides whether to snapshot durable state at the top of a
	// turn. Only consulted when the host grants the Checkpoint capability.
	ShouldCheckpoint func(a *Agent, turn int) bool
	// OnLLMError decides how to handle an llm.call that failed after the
	// host's own internal retries were exhausted (see host.LLMRetry) — this is
	// a terminal failure as far as the guest is concerned. Return retry=true
	// to re-issue the call (rare; the host already retried internally with
	// backoff), a non-nil fallback assistant message to inject in place of the
	// model's turn (letting the run terminate gracefully with that message as
	// the summary), or (nil, false) to abort. The default returns (nil,
	// false), which preserves the pre-existing behaviour of aborting the run
	// with a wrapped "llm.call: ..." error — nothing changes unless a host
	// opts in by overriding this field.
	//
	// Deliberately excluded: guest-side sleeping. Backoff must live entirely
	// in the host handler (wall-clock waiting driven by the guest would be
	// non-deterministic and break replay); this policy is for graceful
	// termination, not for waiting out a rate limit.
	OnLLMError func(a *Agent, err error, turn int) (fallback *abi.LLMMessage, retry bool)
	// ApprovalGate decides whether a tool call needs host approval before it
	// runs, returning the action name and details to present plus whether
	// approval is required at all (need=false means "no approval needed").
	// Only consulted when the host grants the Approval capability; see
	// Client.ApprovalAwait for the degrade-to-approved behaviour when it is
	// absent.
	ApprovalGate func(a *Agent, name string, args json.RawMessage) (action string, details json.RawMessage, need bool)
	// Steer decides whether to pull a pending host steering message and how to
	// inject it into the transcript, consulted at the top of each turn (subject
	// to SteerEvery) when the host grants the Steer capability. Returning
	// inject != nil appends it as a message before the turn proceeds; stop=true
	// requests graceful termination (e.g. the default's "/stop" sentinel).
	Steer func(a *Agent) (inject *abi.LLMMessage, stop bool)
	// SteerEvery throttles how often Steer is consulted when the Steer
	// capability is granted: every SteerEvery-th turn (default 1, i.e. every
	// turn). Raise it to shrink the msg.recv hostcall footprint on hosts that
	// wire a real steering source but don't need per-turn granularity.
	SteerEvery int
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
		tools:  NewRegistry(client, cfg.Capabilities.Multimodal),
		vfs:    vfs,
		bash:   NewBash(vfs, fetch),
		skills: NewSkills(vfs),
		script: NewScriptRunner(ScriptBudget{}),
	}
	a.SystemPrompt = defaultSystemPrompt
	a.EstimateTokens = estimateTokens
	a.Summarize = selectSummarizer(cfg.Compaction)
	// Trigger on the token budget when the host advertises one (like Claude
	// Code's auto-compact near the context limit), else fall back to a simple
	// message-count threshold.
	a.ShouldCompact = func(msgs []abi.LLMMessage) bool {
		if budget := a.cfg.Capabilities.MaxLLMTokens; budget > 0 {
			return a.EstimateTokens(msgs) > budget*8/10 // ~80% of the window
		}
		return len(msgs) > 40
	}
	a.Compact = defaultCompact
	a.ShouldStop = func(ag *Agent, turn int) bool { return ag.done || turn >= ag.cfg.MaxTurns }
	// Snapshot state every few turns so the host can compact the log to a
	// bounded tail and resume interrupted runs.
	a.ShouldCheckpoint = func(ag *Agent, turn int) bool { return turn > 0 && turn%4 == 0 }
	a.OnLLMError = func(_ *Agent, _ error, _ int) (*abi.LLMMessage, bool) { return nil, false }
	a.ApprovalGate = defaultApprovalGate
	a.Steer = defaultSteer
	a.SteerEvery = 1
	a.registerBuiltins()
	a.registerEditTools()
	a.registerPlanTools()
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

	// Resume from a checkpoint when the host offers one; otherwise start fresh.
	// state.restore is always the guest's second startup hostcall so compaction
	// can rely on its position. Two resume flavours:
	//   - reactivation: a *new activation* of a parked (completed) agent — clear
	//     the terminal state, append the new input, run with a fresh budget;
	//   - resume: a bounded-replay/crash continuation of an in-progress run.
	startTurn := 0
	skipCheckpoint := false
	restored := false
	if a.cfg.Capabilities.Checkpoint {
		if st, err := a.client.StateRestore(); err == nil && st.Found {
			if resumeTurn, ok := a.restore(st.State); ok {
				restored = true
				if st.Reactivate {
					a.done = false
					a.summary = ""
					if st.Input != "" {
						a.messages = append(a.messages, abi.LLMMessage{Role: "user", Content: st.Input})
					}
					// Fresh turn budget for the new task; keep the transcript.
					startTurn = 0
				} else {
					startTurn = resumeTurn
					skipCheckpoint = true // the boundary checkpoint is not re-emitted
				}
			}
		}
	}
	if !restored {
		a.messages = []abi.LLMMessage{
			{Role: "system", Content: a.SystemPrompt},
			a.initialUserMessage(),
		}
	}

	// didWork records whether the loop executed at least one turn this
	// activation. It gates the terminal checkpoint so a pure reconstruction
	// (restore a completed agent, do nothing) does not re-emit a checkpoint that
	// would diverge from the recorded/compacted journal.
	didWork := false
	turn := startTurn
	for ; ; turn++ {
		if a.ShouldStop(a, turn) {
			break
		}
		if a.cfg.Capabilities.Steer && a.steerDue(turn) {
			if inject, stop := a.Steer(a); stop {
				break
			} else if inject != nil {
				a.messages = append(a.messages, *inject)
			}
		}
		didWork = true
		if a.cfg.Capabilities.Checkpoint && !skipCheckpoint && a.ShouldCheckpoint(a, turn) {
			_ = a.client.StateCheckpoint(a.checkpointState(turn))
		}
		skipCheckpoint = false
		if a.ShouldCompact(a.messages) {
			a.messages = a.Compact(a, a.messages)
		}

		callLLM := func() (abi.LLMCallResponse, error) {
			return a.client.LLMCall(abi.LLMCallRequest{
				Model:     a.cfg.Model,
				Messages:  a.messagesForLLM(),
				Tools:     a.tools.Specs(),
				MaxTokens: a.cfg.Capabilities.MaxLLMTokens,
			})
		}
		resp, err := callLLM()
		for err != nil {
			// The host already retried internally (host.LLMRetry); this is a
			// terminal failure. OnLLMError decides whether to abort (default),
			// re-issue, or degrade to a fallback message.
			fallback, retry := a.OnLLMError(a, err, turn)
			if retry {
				resp, err = callLLM()
				continue
			}
			if fallback == nil {
				return "", fmt.Errorf("llm.call: %w", err)
			}
			resp = abi.LLMCallResponse{Message: *fallback, FinishReason: "stop"}
			err = nil
		}
		a.messages = append(a.messages, resp.Message)

		if len(resp.Message.ToolCalls) == 0 {
			// Model produced a final answer: the activation is complete.
			if a.summary == "" {
				a.summary = resp.Message.Content
			}
			a.done = true
			break
		}

		for _, tc := range resp.Message.ToolCalls {
			args := json.RawMessage(tc.Arguments)
			if a.cfg.Capabilities.Approval {
				if action, details, need := a.ApprovalGate(a, tc.Name, args); need {
					dec, _ := a.client.ApprovalAwait(action, details)
					if !dec.Approved {
						a.messages = append(a.messages, abi.LLMMessage{
							Role:       "tool",
							ToolCallID: tc.ID,
							Name:       tc.Name,
							Content:    "denied by host: " + dec.Reason,
						})
						_ = a.client.LogAppend("info", "tool call denied", mustJSON(map[string]string{"tool": tc.Name}))
						continue
					}
				}
			}
			out, parts, isErr := a.tools.Invoke(ctx, tc.Name, args)
			_ = isErr
			toolMsg := abi.LLMMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Name,
				Content:    out,
			}
			if len(parts) > 0 {
				toolMsg.Parts = parts
			}
			a.messages = append(a.messages, toolMsg)
			_ = a.client.LogAppend("info", "tool call", mustJSON(map[string]string{"tool": tc.Name}))
		}
		if a.done {
			break
		}
	}

	// Checkpoint on terminate: snapshot the completed state so the host can park
	// the agent with an up-to-date blob and later reactivate it. Skipped when no
	// work was done this activation (see didWork).
	if a.cfg.Capabilities.Checkpoint && didWork {
		_ = a.client.StateCheckpoint(a.checkpointState(turn))
	}
	return a.summary, nil
}

// initialUserMessage builds the first user turn from the configured goal and
// any images the host attached to it (e.g. via latigo-local's -image flag,
// delivered through Config.Images — see LoadConfig). When no images are
// attached this is just {Role:"user", Content: goal}, identical to
// pre-multimodal behaviour. When images are present, Content stays populated
// as the text shorthand (harmless/ignored once Parts is set) and Parts
// becomes authoritative: a leading text part for the goal (when non-empty)
// followed by one image part per attachment, degraded per the negotiated
// Multimodal capability so a text-only host never sees an image part.
func (a *Agent) initialUserMessage() abi.LLMMessage {
	msg := abi.LLMMessage{Role: "user", Content: a.cfg.Goal}
	if len(a.cfg.Images) == 0 {
		return msg
	}
	parts := make([]abi.ContentPart, 0, len(a.cfg.Images)+1)
	if a.cfg.Goal != "" {
		parts = append(parts, abi.ContentPart{Type: "text", Text: a.cfg.Goal})
	}
	for i := range a.cfg.Images {
		img := a.cfg.Images[i]
		parts = append(parts, abi.ContentPart{Type: "image", Image: &img})
	}
	msg.Parts = sanitizeContentParts(parts, a.cfg.Capabilities.Multimodal)
	return msg
}

// messagesForLLM returns the transcript to send the model this turn: the
// persisted transcript (a.messages — what checkpointing and Compact operate
// on) plus a pinned plan reminder appended fresh whenever a plan is set. The
// reminder is synthesized from a.plan on every call and never stored in
// a.messages itself, so defaultCompact (guest/compaction.go) has nothing to
// elide it from — the current plan survives compaction by construction, not
// by special-casing the compactor.
func (a *Agent) messagesForLLM() []abi.LLMMessage {
	if len(a.plan) == 0 {
		return a.messages
	}
	out := make([]abi.LLMMessage, len(a.messages), len(a.messages)+1)
	copy(out, a.messages)
	out = append(out, abi.LLMMessage{
		Role:    "system",
		Content: "Current plan (pinned; use the plan tool to update it):\n" + renderPlan(a.plan),
	})
	return out
}

// agentSnapshot is the guest-defined checkpoint blob. It is opaque to the host.
type agentSnapshot struct {
	Turn     int               `json:"turn"`
	Messages []abi.LLMMessage  `json:"messages"`
	Files    map[string][]byte `json:"files"`
	Done     bool              `json:"done"`
	Summary  string            `json:"summary"`
	// Plan is the durable task list (see guest/plan.go). omitempty so
	// checkpoint blobs written before this field existed still decode: an
	// absent "plan" key unmarshals to a nil slice, identical to a run that
	// never touched the plan tool.
	Plan []PlanItem `json:"plan,omitempty"`
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
		Plan:     a.plan,
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
	a.plan = snap.Plan
	return snap.Turn, true
}

func defaultCompact(a *Agent, msgs []abi.LLMMessage) []abi.LLMMessage {
	if len(msgs) <= 6 {
		return msgs
	}
	// Keep system + first user message (pinned context), summarise the middle
	// via the Summarize strategy, and keep the most recent turns verbatim.
	head := msgs[:2]
	mid := msgs[2 : len(msgs)-4]
	tail := msgs[len(msgs)-4:]
	summary := a.Summarize(a, mid)
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

// steerDue reports whether turn is a multiple of SteerEvery (treating <= 0 as
// "every turn"). It is a pure function of the (deterministic, checkpointed)
// turn counter, so it replays identically.
func (a *Agent) steerDue(turn int) bool {
	n := a.SteerEvery
	if n <= 0 {
		n = 1
	}
	return turn%n == 0
}

// defaultApprovalGate requires approval for the ambient/dangerous surface:
// exec.run-backed tools (by naming convention, "exec*"), http_fetch, VFS
// writes that escape /work, and fs-removal tools. edit_file and multi_edit
// (guest/edit.go) are VFS writes just like write_file — same path field, same
// /work sandbox check — so they are gated identically: an edit that stays
// under /work runs unattended, one that would touch a path outside /work
// requires approval exactly as write_file does. Everything else (bash running
// inside the sandboxed VFS, reads, in-/work writes, skills, scripts, plan)
// runs unattended.
func defaultApprovalGate(a *Agent, name string, args json.RawMessage) (string, json.RawMessage, bool) {
	switch {
	case name == "http_fetch":
		return name, args, true
	case strings.HasPrefix(name, "exec"):
		return name, args, true
	case name == "fs_remove", name == "fs.remove", name == "remove_file":
		return name, args, true
	case name == "write_file", name == "edit_file", name == "multi_edit":
		var in struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(args, &in) == nil && !underWork(in.Path) {
			return name, args, true
		}
	}
	return "", nil, false
}

// underWork reports whether p, resolved relative to /work when not already
// absolute, stays within the /work sandbox root.
func underWork(p string) bool {
	if p == "" {
		return true
	}
	cp := resolve("/work", p)
	return cp == "/work" || strings.HasPrefix(cp, "/work/")
}

// defaultSteer non-blockingly polls the "steer" channel and, if a message is
// pending, either injects it as a user message (so the model sees it next
// turn) or, for the "/stop" sentinel, requests graceful termination. Absence
// of a message (or a host that errors/degrades the call) is a no-op.
func defaultSteer(a *Agent) (*abi.LLMMessage, bool) {
	resp, err := a.client.MsgRecv("steer", false)
	if err != nil || !resp.HasMessage {
		return nil, false
	}
	if strings.TrimSpace(resp.Content) == "/stop" {
		return nil, true
	}
	msg := abi.LLMMessage{Role: "user", Content: resp.Content}
	return &msg, false
}
