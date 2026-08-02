// Package main: agent.go is the agent loop (spec §2.5).
//
// The loop: validate tool arguments against their schema before dispatch
// (model-visible retry on failure, bounded and logged); dispatch via the
// allow-listed shell; record intent + result to the write-ahead event log;
// enforce max_turns / max_total_tokens / max_tool_invocations / max_wall_clock
// in-loop; terminate on a validated `finish` call.
//
// Latigo is stateless between turns (spec §2.6). Resume means: load the
// transcript from the event log, mount the workspace, continue. There is no
// in-memory state to snapshot — the log and the workspace are the state.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Agent is the agent loop wired to an LLM client, an allow-listed shell, and a
// write-ahead event log.
type Agent struct {
	cfg          Config
	llm          *LLMClient
	shell        *Shell
	allow        *Allowlist
	log          *EventLog
	tools        []*Tool
	outputSchema *Schema

	messages []Message
	runID    string
	start    time.Time
	tb       timebomb

	// usage counters
	turns           int
	totalTokens     int
	toolInvocations int

	// finish signalling (set by the finish tool)
	finishSet     bool
	finishOutput  json.RawMessage
	finishInvalid [][]string

	// DeltaSink, when non-nil, receives text deltas during a streamed chat
	// completion. Set it to stream the model's output to a live consumer. It is
	// an ephemeral output path; the assembled message is still the record.
	DeltaSink DeltaSink

	// Strategy points (overridable), see compaction.go.
	SystemPrompt   string
	ShouldCompact  func(a *Agent, msgs []Message) bool
	Compact        func(a *Agent, msgs []Message) []Message
	EstimateTokens func(msgs []Message) int
}

// NewAgent constructs the agent from its wired dependencies.
func NewAgent(cfg Config, llm *LLMClient, shell *Shell, allow *Allowlist, log *EventLog, outputSchema *Schema) *Agent {
	a := &Agent{
		cfg:          cfg,
		llm:          llm,
		shell:        shell,
		allow:        allow,
		log:          log,
		tools:        builtinTools(outputSchema),
		outputSchema: outputSchema,
		tb:           newTimebomb(time.Duration(cfg.MaxWallClockSeconds) * time.Second),
	}
	a.SystemPrompt = defaultSystemPrompt
	a.EstimateTokens = estimateTokens
	a.ShouldCompact = func(ag *Agent, msgs []Message) bool {
		// Trigger near the token budget when one is set; else on a message
		// count, so the transcript never grows without bound.
		if budget := ag.cfg.MaxTotalTokens; budget > 0 {
			return ag.EstimateTokens(msgs) > budget*8/10
		}
		return len(msgs) > 40
	}
	a.Compact = defaultCompact
	if cfg.Compaction == "llm" {
		// Model-driven summarisation: a real llm.call summarises the elided
		// middle. Because that call is recorded in the log, resume stays
		// consistent (the summary is part of the transcript).
		a.Compact = func(ag *Agent, msgs []Message) []Message {
			return llmCompact(ag, msgs)
		}
	}
	return a
}

const defaultSystemPrompt = `You are Latigo, a WASIX agent. You have three tools:

- shell: run an allow-listed command line in the workspace. Pipes and redirection work; command substitution and eval are rejected. Work in the mounted workspace directory.
- tool_list: list the available commands on the allow-list.
- finish: end the task with your final output.

To learn what commands are available, call tool_list. To learn a command's full usage, run "<name> --help" through the shell. Context cost scales with tools used, not tools available, so only fetch usage you need.

Work toward the user's goal. When the goal is complete, call finish with the result.`

// Run executes the agent loop until termination and returns the final output
// (the finish tool's output, or the last assistant text if the model answered
// without finishing) and the termination reason.
func (a *Agent) Run(ctx context.Context) (string, string, error) {
	a.start = time.Now()
	if err := a.bootstrap(); err != nil {
		return "", "bootstrap_error", err
	}
	if _, err := a.log.Append(KindRunStart, RunStartPayload{
		RunID:      a.runID,
		Goal:       a.cfg.Goal,
		Model:      a.cfg.Model,
		LLMBaseURL: a.cfg.LLMBaseURL,
		Grants: GrantsSummary{
			Workspace: a.cfg.Workspace,
			Net:       []string{a.cfg.LLMBaseURL},
			Commands:  a.allow.sortedNames(),
		},
		Config: a.cfg.summary(),
	}); err != nil {
		return "", "log_error", err
	}

	for a.turns < a.cfg.MaxTurns {
		if a.tb.expired() {
			return a.terminate("max_wall_clock", "")
		}
		if a.cfg.MaxTotalTokens > 0 && a.totalTokens > a.cfg.MaxTotalTokens {
			return a.terminate("max_total_tokens", "")
		}
		if a.cfg.MaxToolInvocations > 0 && a.toolInvocations > a.cfg.MaxToolInvocations {
			return a.terminate("max_tool_invocations", "")
		}
		if _, err := a.log.Append(KindTurn, TurnPayload{Turn: a.turns}); err != nil {
			return "", "log_error", err
		}
		if a.ShouldCompact(a, a.messages) {
			a.messages = a.Compact(a, a.messages)
		}

		resp, err := a.callLLM(ctx)
		if err != nil {
			// A terminal LLM failure after internal retries (none here —
			// bounded model-visible retry is for tool validation, not LLM
			// transport). Record and abort.
			_, _ = a.log.Append(KindLog, LogPayload{Level: "error", Message: "llm call failed: " + err.Error()})
			_, _, _ = a.terminate("llm_error", err.Error())
			return "", "llm_error", err
		}
		a.messages = append(a.messages, resp.Message)
		if _, err := a.log.Append(KindLLM, LLMPayload{
			Turn:         a.turns,
			Model:        resp.Model,
			LatencyMS:    resp.Latency.Milliseconds(),
			InputTokens:  resp.InputTokens,
			OutputTokens: resp.OutputTokens,
			TotalTokens:  resp.TotalTokens,
			FinishReason: resp.FinishReason,
			Message:      resp.Message,
		}); err != nil {
			return "", "log_error", err
		}
		a.totalTokens += resp.TotalTokens
		a.turns++

		if len(resp.Message.ToolCalls) == 0 {
			// The model produced a final answer without calling finish.
			// Treat its text content as the output.
			out := resp.Message.Content
			if _, err := a.log.Append(KindFinish, FinishPayload{
				Output: json.RawMessage(fmt.Sprintf("%q", out)),
				Valid:  true,
			}); err != nil {
				return "", "log_error", err
			}
			return a.terminate("answered", out)
		}

		if stopped, err := a.dispatchToolCalls(ctx, resp.Message.ToolCalls); stopped || err != nil {
			if err != nil {
				return "", "dispatch_error", err
			}
			// finish was called and validated.
			return a.terminate("finished", string(a.finishOutput))
		}
		// Emit a turn-end marker. The checkpoint ID is assigned by the
		// orchestrator (Stonewall); on the single-host path it is empty and
		// the log + workspace remain the recoverable state.
		if _, err := a.log.Append(KindTurnEnd, TurnEndPayload{
			Turn:   a.turns - 1,
			Egress: []string{a.cfg.LLMBaseURL},
		}); err != nil {
			return "", "log_error", err
		}
	}
	return a.terminate("max_turns", "")
}

// bootstrap loads the transcript (resume) or seeds the initial system + user
// messages (fresh run).
func (a *Agent) bootstrap() error {
	a.runID = newRunID()
	if a.cfg.Resume {
		lt, err := LoadTranscript(a.cfg.EventLog)
		if err != nil {
			return fmt.Errorf("resume: load transcript: %w", err)
		}
		a.messages = append(a.messages, Message{Role: "system", Content: a.SystemPrompt})
		if lt.Goal != "" {
			a.messages = append(a.messages, Message{Role: "user", Content: lt.Goal})
		} else {
			a.messages = append(a.messages, Message{Role: "user", Content: a.cfg.Goal})
		}
		a.messages = append(a.messages, lt.Messages...)
		if lt.Model != "" {
			a.llm.Model = lt.Model
		}
		if lt.RunID != "" {
			a.runID = lt.RunID
		}
		if err := a.log.ResumeSeq(); err != nil {
			return err
		}
		return nil
	}
	a.messages = []Message{
		{Role: "system", Content: a.SystemPrompt},
		{Role: "user", Content: a.cfg.Goal},
	}
	return nil
}

// callLLM performs one chat completion against the endpoint. When streaming is
// enabled it uses the streaming path and forwards text deltas to a.DeltaSink;
// otherwise it uses the plain non-streaming path. Both return the same
// LLMResult, so the loop and event log are unchanged by how the bytes arrive.
func (a *Agent) callLLM(ctx context.Context) (LLMResult, error) {
	req := chatRequest{
		Model:    a.cfg.Model,
		Messages: wireMessages(a.messages),
		Tools:    toolSpecs(a.tools),
	}
	if a.cfg.Stream {
		return a.llm.CallStream(ctx, req, a.DeltaSink)
	}
	return a.llm.Call(ctx, req)
}

// dispatchToolCalls runs the model's requested tool calls for this turn. It
// returns stopped=true when the `finish` tool was called and validated (the
// loop should terminate). Each tool call's arguments are schema-validated
// before dispatch (model-visible retry): on a validation failure the model is
// told via the tool-role message and the call is logged, bounded by the
// overall tool-invocation limit.
func (a *Agent) dispatchToolCalls(ctx context.Context, calls []ToolCall) (bool, error) {
	for _, tc := range calls {
		args := json.RawMessage(tc.Function.Arguments)
		tool := findTool(a.tools, tc.Function.Name)
		if tool == nil {
			a.toolInvocations++
			msg := fmt.Sprintf("error: unknown tool %q", tc.Function.Name)
			a.recordTool(ctx, tc, args, "error", 0, "", "", msg)
			a.messages = append(a.messages, Message{
				Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name, Content: msg,
			})
			continue
		}
		// Schema-validate arguments before dispatch (spec §2.5).
		schema, _ := parseSchema(tool.Parameters)
		if errs := validateValue(schema, args); len(errs) > 0 {
			a.toolInvocations++
			msg := "invalid arguments: " + strings.Join(errs, "; ")
			_, _ = a.log.Append(KindLog, LogPayload{
				Level: "warn", Message: "tool args validation failed",
				Fields: mustJSON(map[string]any{"tool": tc.Function.Name, "errors": errs}),
			})
			a.recordTool(ctx, tc, args, "invalid", 0, "", "", msg)
			a.messages = append(a.messages, Message{
				Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name, Content: msg,
			})
			continue
		}

		// Record intent (with idempotency key) before dispatch, write-ahead.
		key := idempotencyKey(a.runID, tc.ID, a.toolInvocations)
		if _, err := a.log.Append(KindTool, ToolPayload{
			CallID: tc.ID, IdempotencyKey: key, Name: tc.Function.Name, Args: args, Status: "intent",
		}); err != nil {
			return false, err
		}

		a.toolInvocations++
		tStart := time.Now()
		content, isErr := tool.Invoke(ctx, a, args)
		latency := time.Since(tStart).Milliseconds()

		status := "ok"
		if isErr {
			status = "error"
		}
		// If finish was called and validated, record it and stop.
		if a.finishSet {
			if _, err := a.log.Append(KindFinish, FinishPayload{
				Output: a.finishOutput, Valid: true,
			}); err != nil {
				return false, err
			}
			return true, nil
		}
		a.recordTool(ctx, tc, args, status, 0, content, "", "")
		_ = latency
		a.messages = append(a.messages, Message{
			Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name, Content: content,
		})
	}
	return false, nil
}

// recordTool writes a terminal tool event (ok/error/invalid/denied).
func (a *Agent) recordTool(ctx context.Context, tc ToolCall, args json.RawMessage, status string, exit int, stdout, stderr, errMsg string) {
	_, _ = a.log.Append(KindTool, ToolPayload{
		CallID:         tc.ID,
		IdempotencyKey: idempotencyKey(a.runID, tc.ID, a.toolInvocations),
		Name:           tc.Function.Name,
		Args:           args,
		Status:         status,
		ExitCode:       exit,
		Stdout:         stdout,
		Stderr:         stderr,
		Error:          errMsg,
	})
}

// terminate records the run_end event and returns the final output + reason.
func (a *Agent) terminate(reason, output string) (string, string, error) {
	_, _ = a.log.Append(KindRunEnd, RunEndPayload{Reason: reason})
	if output == "" {
		// For finish, the output is the finish tool's payload; for limit
		// terminations, surface a short status.
		switch reason {
		case "finished":
			output = string(a.finishOutput)
		case "answered":
			// already set
		default:
			output = fmt.Sprintf("terminated: %s", reason)
		}
	}
	return output, reason, nil
}

// idempotencyKey derives a stable key for a tool intent: the run, the tool
// call id, and the invocation index. On resume after a crash mid-tool, the
// recorded intent lets the orchestrator dedup.
func idempotencyKey(runID, callID string, attempt int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", runID, callID, attempt)))
	return hex.EncodeToString(h[:8])
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
