// Package main: events.go is the durable event log (spec §2.6).
//
// Append-only JSONL, fsync'd before any result is acted upon (write-ahead).
// It carries the conversation *plus* a thin operational layer: turn
// boundaries, tool/exec intent with an idempotency key (recorded before
// dispatch), results/exit codes/token counts/model/latency, the workspace
// checkpoint ID produced by each turn, and egress destinations.
//
// There is no replay engine here. Latigo is stateless between turns: resume
// means load the transcript, mount the workspace, continue. Loading the
// transcript is just reading the recorded conversation back out of the log —
// not re-execution.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// HarnessVersion stamps every event Latigo emits.
const HarnessVersion = "latigo/0.2.0"

// Event kinds recorded in the log.
const (
	KindRunStart = "run_start"
	KindTurn     = "turn"
	KindLLM      = "llm"
	KindTool     = "tool"
	KindFinish   = "finish"
	KindTurnEnd  = "turn_end"
	KindRunEnd   = "run_end"
	KindLog      = "log" // operational notes (validation failures, etc.)
)

// Event is one JSONL record.
type Event struct {
	Seq     uint64          `json:"seq"`
	Kind    string          `json:"kind"`
	Time    time.Time       `json:"time"`
	Harness string          `json:"harness"`
	Payload json.RawMessage `json:"payload"`
}

// EventLog is an append-only, fsync'd JSONL writer.
type EventLog struct {
	mu   sync.Mutex
	f    *os.File
	w    *bufio.Writer
	seq  uint64
	path string

	// tail caches the high-water marks read from the log as it stood when it
	// was opened, so the resume path derives both counters from one scan.
	tail *logTail
}

// logTail is what an existing log says about where to continue: the highest
// sequence number and the highest turn number it records. Both are needed on
// the same path, so one pass produces both.
type logTail struct {
	seq  uint64
	turn int
}

// scanTail reads the existing log once and returns its high-water marks,
// caching the result: the file does not change under us between opening it and
// our first append. Returns zeroes for a fresh or unreadable log.
func (l *EventLog) scanTail() (logTail, error) {
	if l.tail != nil {
		return *l.tail, nil
	}
	var t logTail
	in, err := os.Open(l.path)
	if err != nil {
		l.tail = &t
		return t, nil
	}
	defer in.Close()
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.Seq > t.seq {
			t.seq = e.Seq
		}
		switch e.Kind {
		case KindTurn, KindTurnEnd, KindLLM:
			// All three payloads carry the turn number under "turn".
			var p TurnPayload
			if json.Unmarshal(e.Payload, &p) == nil && p.Turn > t.turn {
				t.turn = p.Turn
			}
		}
	}
	if err := sc.Err(); err != nil {
		return t, err
	}
	l.tail = &t
	return t, nil
}

// OpenEventLog opens (or creates) an event log for appending.
func OpenEventLog(path string) (*EventLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &EventLog{f: f, w: bufio.NewWriter(f), path: path}, nil
}

// lastSeq reads the existing log to find the highest sequence number, so a
// resumed run continues numbering monotonically. Returns 0 for a fresh log.
func (l *EventLog) lastSeq() (uint64, error) {
	t, err := l.scanTail()
	return t.seq, err
}

// lastTurn reads the existing log to find the highest turn number it records,
// so a resumed run continues turn numbering rather than restarting it. Returns
// 0 for a log with no recorded turns, whose first turn is therefore 1.
func (l *EventLog) lastTurn() (int, error) {
	t, err := l.scanTail()
	return t.turn, err
}

// ResumeSeq sets the sequence counter to continue after an existing log.
func (l *EventLog) ResumeSeq() error {
	n, err := l.lastSeq()
	if err != nil {
		return err
	}
	l.seq = n
	return nil
}

// ResumeTurn returns the turn number a resumed run continues from: the highest
// turn the log records, so the run's first turn is one greater. The counter
// itself lives on the agent (the log records turn numbers but does not assign
// them), which is why this returns a value where ResumeSeq sets one.
func (l *EventLog) ResumeTurn() (int, error) {
	return l.lastTurn()
}

// Append writes one event, flushes, and fsyncs. Write-ahead: a caller must
// have appended (and Synced) a result before acting on it.
func (l *EventLog) Append(kind string, payload any) (Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	body, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}
	ev := Event{
		Seq:     l.seq,
		Kind:    kind,
		Time:    time.Now().UTC(),
		Harness: HarnessVersion,
		Payload: body,
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return Event{}, err
	}
	if _, err := l.w.Write(line); err != nil {
		return Event{}, err
	}
	if err := l.w.WriteByte('\n'); err != nil {
		return Event{}, err
	}
	if err := l.w.Flush(); err != nil {
		return Event{}, err
	}
	if err := l.f.Sync(); err != nil {
		return Event{}, err
	}
	return ev, nil
}

// Close flushes and closes the underlying file.
func (l *EventLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.w.Flush(); err != nil {
		return err
	}
	return l.f.Close()
}

// ----- payloads -----

type RunStartPayload struct {
	RunID      string          `json:"run_id"`
	Goal       string          `json:"goal"`
	Model      string          `json:"model"`
	LLMBaseURL string          `json:"llm_base_url"`
	Grants     GrantsSummary   `json:"grants"`
	Config     json.RawMessage `json:"config,omitempty"`
}

type GrantsSummary struct {
	Workspace string   `json:"workspace,omitempty"`
	Net       []string `json:"net,omitempty"`
	Commands  []string `json:"commands,omitempty"`
}

type TurnPayload struct {
	Turn int `json:"turn"`
}

type LLMPayload struct {
	Turn         int    `json:"turn"`
	Model        string `json:"model"`
	LatencyMS    int64  `json:"latency_ms"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	TotalTokens  int    `json:"total_tokens"`
	FinishReason string `json:"finish_reason"`
	// Message is the assistant turn, recorded verbatim so the transcript can
	// be rebuilt by reading the log. It includes any tool_calls.
	Message Message `json:"message"`
	// RetryAfterMS is the advisory backoff hint, if any, on a retried call.
	RetryAfterMS int64 `json:"retry_after_ms,omitempty"`
}

type ToolPayload struct {
	CallID         string          `json:"call_id"`
	IdempotencyKey string          `json:"idempotency_key"`
	Name           string          `json:"name"`
	Args           json.RawMessage `json:"args,omitempty"`
	// Status: "intent" (before dispatch), "ok", "error", "invalid", "denied".
	Status    string `json:"status"`
	ExitCode  int    `json:"exit_code,omitempty"`
	Stdout    string `json:"stdout,omitempty"`
	Stderr    string `json:"stderr,omitempty"`
	Error     string `json:"error,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
}

type FinishPayload struct {
	Output json.RawMessage `json:"output"`
	Valid  bool            `json:"valid"`
	Errors []string        `json:"errors,omitempty"`
}

type TurnEndPayload struct {
	Turn         int      `json:"turn"`
	CheckpointID string   `json:"checkpoint_id,omitempty"`
	Egress       []string `json:"egress,omitempty"`
}

type RunEndPayload struct {
	Reason string `json:"reason"`
	Error  string `json:"error,omitempty"`
}

type LogPayload struct {
	Level   string          `json:"level"`
	Message string          `json:"message"`
	Fields  json.RawMessage `json:"fields,omitempty"`
}

// ----- transcript rebuild -----

// loadedTranscript is the conversation reconstructed from an event log.
type loadedTranscript struct {
	RunID    string
	Goal     string
	Model    string
	Messages []Message
}

// LoadTranscript reads an event log and rebuilds the conversation: the system
// prompt is supplied by the caller; the goal comes from run_start; assistant
// turns come from llm events; tool results come from tool events with a
// terminal status. intent-only tool events and operational events are
// skipped. This is reading the recorded conversation, not re-execution.
func LoadTranscript(path string) (loadedTranscript, error) {
	var lt loadedTranscript
	in, err := os.Open(path)
	if err != nil {
		return lt, err
	}
	defer in.Close()
	// Track which call_ids already produced a tool message so a later
	// "intent" record that follows a "ok" record (out of order, shouldn't
	// happen but is defensive) doesn't duplicate.
	seen := map[string]bool{}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch e.Kind {
		case KindRunStart:
			var p RunStartPayload
			if json.Unmarshal(e.Payload, &p) == nil {
				lt.RunID = p.RunID
				lt.Goal = p.Goal
				lt.Model = p.Model
			}
		case KindLLM:
			var p LLMPayload
			if json.Unmarshal(e.Payload, &p) == nil {
				lt.Messages = append(lt.Messages, p.Message)
			}
		case KindTool:
			var p ToolPayload
			if json.Unmarshal(e.Payload, &p) == nil {
				if p.Status == "ok" || p.Status == "error" || p.Status == "invalid" || p.Status == "denied" {
					if seen[p.CallID] {
						continue
					}
					seen[p.CallID] = true
					content := p.Stdout
					if content == "" {
						content = p.Stderr
					}
					if p.Status != "ok" && p.Error != "" {
						if content != "" {
							content += "\n"
						}
						content += p.Error
					}
					lt.Messages = append(lt.Messages, Message{
						Role:       "tool",
						ToolCallID: p.CallID,
						Name:       p.Name,
						Content:    content,
					})
				}
			}
		}
	}
	return lt, sc.Err()
}

// newRunID returns a short, monotonic-ish run id from the wall clock. It is a
// label for the log, not a security value.
func newRunID() string {
	return fmt.Sprintf("run-%d", time.Now().UnixNano())
}
