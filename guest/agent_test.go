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

	// approval.await: approveAll approves every request when true; otherwise
	// every request is denied with denyReason (default "denied"). approvalCalls
	// records what was asked, in order.
	approveAll    bool
	denyReason    string
	approvalCalls []approvalCall

	// msg.recv: steerMsgs is served in order, one per call, "" meaning
	// HasMessage=false for that call; calls beyond len(steerMsgs) also report
	// HasMessage=false. msgRecvCalls counts every call regardless of channel.
	steerMsgs    []string
	steerIdx     int
	msgRecvCalls int
}

// approvalCall records one approval.await request observed by fakeTransport.
type approvalCall struct {
	Action  string
	Details json.RawMessage
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
	case abi.OpApprovalAwait:
		var r abi.ApprovalAwaitRequest
		_ = json.Unmarshal(req.Args, &r)
		f.approvalCalls = append(f.approvalCalls, approvalCall{Action: r.Action, Details: r.Details})
		if f.approveAll {
			return result(abi.ApprovalAwaitResponse{Approved: true, Reason: "ok"})
		}
		reason := f.denyReason
		if reason == "" {
			reason = "denied"
		}
		return result(abi.ApprovalAwaitResponse{Approved: false, Reason: reason})
	case abi.OpMsgRecv:
		f.msgRecvCalls++
		if f.steerIdx < len(f.steerMsgs) {
			msg := f.steerMsgs[f.steerIdx]
			f.steerIdx++
			if msg == "" {
				return result(abi.MsgRecvResponse{HasMessage: false})
			}
			return result(abi.MsgRecvResponse{HasMessage: true, Content: msg, Channel: "steer"})
		}
		return result(abi.MsgRecvResponse{HasMessage: false})
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
	// Snapshot the agent, then restore it into a fresh agent and confirm the
	// transcript survives the round-trip.
	agent.messages = append(agent.messages, abi.LLMMessage{Role: "user", Content: "marker"})
	_ = agent.vfs.WriteFile("/work/keep.txt", []byte("data"))
	cp := agent.checkpointState(3)

	restored := NewAgent(Config{Goal: "g", MaxTurns: 1}, client)
	turn, ok := restored.restore(cp)
	if !ok || turn != 3 {
		t.Fatalf("restore returned turn=%d ok=%v, want 3,true", turn, ok)
	}
	if len(restored.messages) != len(agent.messages) {
		t.Errorf("restored %d messages, want %d", len(restored.messages), len(agent.messages))
	}
	if data, err := restored.vfs.ReadFile("/work/keep.txt"); err != nil || string(data) != "data" {
		t.Errorf("restored vfs file = %q err=%v", data, err)
	}
}

// erroringLLMTransport always fails llm.call with a classified host error
// (mirroring what host.LLMClient surfaces once its internal retries are
// exhausted), and answers everything else like fakeTransport's zero-turn path.
type erroringLLMTransport struct {
	code    string
	message string
	calls   int
}

func (f *erroringLLMTransport) Hostcall(req abi.Request) (abi.Response, error) {
	switch req.Op {
	case abi.OpLLMCall:
		f.calls++
		return abi.Response{Error: f.message, Code: f.code}, nil
	case abi.OpToolList:
		return result(abi.ToolListResponse{Epoch: 1})
	case abi.OpLogAppend:
		return result(abi.LogAppendResponse{})
	case abi.OpClockNow:
		return result(abi.ClockNowResponse{UnixNano: 1})
	default:
		return abi.Response{Error: "unsupported", Code: abi.ErrUnsupported}, nil
	}
}

// TestOnLLMErrorDefaultAborts verifies the default strategy preserves
// today's behaviour: an llm.call failure aborts the run with a wrapped error,
// with no fallback message and no additional retry issued.
func TestOnLLMErrorDefaultAborts(t *testing.T) {
	ft := &erroringLLMTransport{code: abi.ErrRateLimited, message: "provider is rate limiting"}
	agent := NewAgent(Config{Goal: "test goal", MaxTurns: 8}, NewClient(ft))

	summary, err := agent.Run(context.Background())
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "llm.call:") {
		t.Errorf("error = %v, want it to be wrapped as llm.call: ...", err)
	}
	if !strings.Contains(err.Error(), "provider is rate limiting") {
		t.Errorf("error = %v, want the underlying host error preserved", err)
	}
	if summary != "" {
		t.Errorf("summary = %q, want empty on abort", summary)
	}
	if ft.calls != 1 {
		t.Errorf("llm.call issued %d times, want 1 (no guest-side retry by default)", ft.calls)
	}
}

// TestOnLLMErrorFallbackTerminatesCleanly verifies an opt-in OnLLMError
// override can degrade to a fallback assistant message instead of aborting,
// and that the run terminates cleanly with that message as the summary.
func TestOnLLMErrorFallbackTerminatesCleanly(t *testing.T) {
	ft := &erroringLLMTransport{code: abi.ErrOverloaded, message: "provider unavailable"}
	agent := NewAgent(Config{Goal: "test goal", MaxTurns: 8}, NewClient(ft))

	var gotErr error
	var gotTurn int
	agent.OnLLMError = func(a *Agent, err error, turn int) (*abi.LLMMessage, bool) {
		gotErr = err
		gotTurn = turn
		return &abi.LLMMessage{Role: "assistant", Content: "stopping: provider unavailable"}, false
	}

	summary, err := agent.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary != "stopping: provider unavailable" {
		t.Errorf("summary = %q, want %q", summary, "stopping: provider unavailable")
	}
	if gotErr == nil {
		t.Error("OnLLMError was not invoked with the underlying error")
	}
	if gotTurn != 0 {
		t.Errorf("OnLLMError turn = %d, want 0", gotTurn)
	}
	if ft.calls != 1 {
		t.Errorf("llm.call issued %d times, want 1", ft.calls)
	}
}

// TestOnLLMErrorRetryReissues verifies retry=true causes the guest to
// re-issue the call exactly once per invocation, and that the loop terminates
// once the policy stops asking for a retry.
func TestOnLLMErrorRetryReissues(t *testing.T) {
	ft := &erroringLLMTransport{code: abi.ErrTimeout, message: "timed out"}
	agent := NewAgent(Config{Goal: "test goal", MaxTurns: 8}, NewClient(ft))

	attempts := 0
	agent.OnLLMError = func(a *Agent, err error, turn int) (*abi.LLMMessage, bool) {
		attempts++
		if attempts < 3 {
			return nil, true // ask the guest to re-issue
		}
		return &abi.LLMMessage{Role: "assistant", Content: "gave up after retries"}, false
	}

	summary, err := agent.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary != "gave up after retries" {
		t.Errorf("summary = %q, want %q", summary, "gave up after retries")
	}
	if ft.calls != 3 {
		t.Errorf("llm.call issued %d times, want 3", ft.calls)
	}
	if attempts != 3 {
		t.Errorf("OnLLMError invoked %d times, want 3", attempts)
	}
}

// ----- approval gating -----

func TestApprovalGateGatedVsUngated(t *testing.T) {
	fetchArgs, _ := json.Marshal(map[string]string{"url": "http://example.com"})
	readArgs, _ := json.Marshal(map[string]string{"path": "/work/x.txt"})
	doneArgs, _ := json.Marshal(map[string]string{"summary": "ok"})
	ft := &fakeTransport{
		approveAll: true,
		llmTurns: []abi.LLMMessage{
			{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "1", Name: "http_fetch", Arguments: string(fetchArgs)}}},
			{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "2", Name: "read_file", Arguments: string(readArgs)}}},
			{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "3", Name: "done", Arguments: string(doneArgs)}}},
		},
	}
	client := NewClient(ft)
	agent := NewAgent(Config{Goal: "g", MaxTurns: 8, Capabilities: abi.Capabilities{Approval: true}}, client)
	_ = agent.vfs.WriteFile("/work/x.txt", []byte("hi"))

	if _, err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ft.approvalCalls) != 1 || ft.approvalCalls[0].Action != "http_fetch" {
		t.Fatalf("approval calls = %+v, want exactly one for http_fetch", ft.approvalCalls)
	}
}

func TestApprovalDenialFeedsBackAndContinues(t *testing.T) {
	fetchArgs, _ := json.Marshal(map[string]string{"url": "http://example.com"})
	doneArgs, _ := json.Marshal(map[string]string{"summary": "ok"})
	ft := &fakeTransport{
		denyReason: "not allowed",
		llmTurns: []abi.LLMMessage{
			{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "1", Name: "http_fetch", Arguments: string(fetchArgs)}}},
			{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "2", Name: "done", Arguments: string(doneArgs)}}},
		},
	}
	client := NewClient(ft)
	agent := NewAgent(Config{Goal: "g", MaxTurns: 8, Capabilities: abi.Capabilities{Approval: true}}, client)

	summary, err := agent.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary != "ok" {
		t.Fatalf("summary = %q, want the run to continue past the denial to completion", summary)
	}
	var found bool
	for _, m := range agent.messages {
		if m.ToolCallID == "1" {
			found = true
			if !strings.Contains(m.Content, "denied by host: not allowed") {
				t.Errorf("tool result = %q, want a denial message", m.Content)
			}
		}
	}
	if !found {
		t.Fatal("no tool result recorded for the denied call")
	}
}

func TestApprovalGracefulDegradationNoCapability(t *testing.T) {
	fetchArgs, _ := json.Marshal(map[string]string{"url": "http://example.com"})
	doneArgs, _ := json.Marshal(map[string]string{"summary": "ok"})
	ft := &fakeTransport{
		llmTurns: []abi.LLMMessage{
			{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "1", Name: "http_fetch", Arguments: string(fetchArgs)}}},
			{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "2", Name: "done", Arguments: string(doneArgs)}}},
		},
	}
	client := NewClient(ft)
	// No Approval capability granted: even a gated tool name runs unprompted.
	agent := NewAgent(Config{Goal: "g", MaxTurns: 8}, client)

	if _, err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ft.approvalCalls) != 0 {
		t.Fatalf("approval calls = %d, want 0 (no approval capability)", len(ft.approvalCalls))
	}
}

func TestApprovalGateWriteOutsideWork(t *testing.T) {
	inside, _ := json.Marshal(map[string]string{"path": "/work/a.txt", "content": "x"})
	outside, _ := json.Marshal(map[string]string{"path": "/etc/passwd", "content": "x"})
	if _, _, need := defaultApprovalGate(nil, "write_file", inside); need {
		t.Error("write_file under /work should not require approval")
	}
	if _, _, need := defaultApprovalGate(nil, "write_file", outside); !need {
		t.Error("write_file outside /work should require approval")
	}
}

// TestApprovalGateEditToolsMatchWriteFile verifies edit_file and multi_edit
// are gated identically to write_file: same /work-escape check on the same
// "path" field, for both the in-sandbox (unattended) and escaping (gated)
// cases.
func TestApprovalGateEditToolsMatchWriteFile(t *testing.T) {
	editInside, _ := json.Marshal(map[string]any{"path": "/work/a.txt", "old": "x", "new": "y"})
	editOutside, _ := json.Marshal(map[string]any{"path": "/etc/passwd", "old": "x", "new": "y"})
	multiInside, _ := json.Marshal(map[string]any{"path": "/work/a.txt", "edits": []map[string]string{{"old": "x", "new": "y"}}})
	multiOutside, _ := json.Marshal(map[string]any{"path": "/etc/passwd", "edits": []map[string]string{{"old": "x", "new": "y"}}})

	for _, tc := range []struct {
		name string
		args json.RawMessage
		tool string
		want bool
	}{
		{"edit_file under /work", editInside, "edit_file", false},
		{"edit_file outside /work", editOutside, "edit_file", true},
		{"multi_edit under /work", multiInside, "multi_edit", false},
		{"multi_edit outside /work", multiOutside, "multi_edit", true},
	} {
		if _, _, need := defaultApprovalGate(nil, tc.tool, tc.args); need != tc.want {
			t.Errorf("%s: need=%v, want %v", tc.name, need, tc.want)
		}
	}
}

// TestApprovalGateEditEscapingWorkIsGatedLive runs the escaping edit_file
// path through the full agent loop (not just the gate function in
// isolation), confirming an edit targeting outside /work actually pauses for
// approval.await like write_file does, end to end.
func TestApprovalGateEditEscapingWorkIsGatedLive(t *testing.T) {
	editArgs, _ := json.Marshal(map[string]any{"path": "/etc/passwd", "old": "", "new": "pwned"})
	doneArgs, _ := json.Marshal(map[string]string{"summary": "ok"})
	ft := &fakeTransport{
		approveAll: true,
		llmTurns: []abi.LLMMessage{
			{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "1", Name: "edit_file", Arguments: string(editArgs)}}},
			{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "2", Name: "done", Arguments: string(doneArgs)}}},
		},
	}
	client := NewClient(ft)
	agent := NewAgent(Config{Goal: "g", MaxTurns: 8, Capabilities: abi.Capabilities{Approval: true}}, client)

	if _, err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ft.approvalCalls) != 1 || ft.approvalCalls[0].Action != "edit_file" {
		t.Fatalf("approval calls = %+v, want exactly one for edit_file", ft.approvalCalls)
	}
}

// ----- steering -----

func TestSteerInjectsMessageAtNextTurn(t *testing.T) {
	bashArgs, _ := json.Marshal(map[string]string{"script": "true"})
	doneArgs, _ := json.Marshal(map[string]string{"summary": "ok"})
	ft := &fakeTransport{
		steerMsgs: []string{"focus on X"},
		llmTurns: []abi.LLMMessage{
			{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "1", Name: "bash", Arguments: string(bashArgs)}}},
			{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "2", Name: "done", Arguments: string(doneArgs)}}},
		},
	}
	client := NewClient(ft)
	agent := NewAgent(Config{Goal: "g", MaxTurns: 8, Capabilities: abi.Capabilities{Steer: true}}, client)

	if _, err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ft.msgRecvCalls == 0 {
		t.Fatal("expected msg.recv to be polled")
	}
	var found bool
	for _, m := range agent.messages {
		if m.Role == "user" && m.Content == "focus on X" {
			found = true
		}
	}
	if !found {
		t.Fatalf("steering message not injected; messages=%+v", agent.messages)
	}
}

func TestSteerStopEndsRunGracefully(t *testing.T) {
	ft := &fakeTransport{
		steerMsgs: []string{"/stop"},
		llmTurns: []abi.LLMMessage{
			{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "1", Name: "bash", Arguments: `{"script":"true"}`}}},
		},
	}
	client := NewClient(ft)
	agent := NewAgent(Config{Goal: "g", MaxTurns: 8, Capabilities: abi.Capabilities{Steer: true}}, client)

	if _, err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ft.llmIdx != 0 {
		t.Fatalf("llm.call issued %d times, want 0 (a /stop steer message should preempt the turn)", ft.llmIdx)
	}
}

func TestSteerAbsenceIsNoOp(t *testing.T) {
	bashArgs, _ := json.Marshal(map[string]string{"script": "echo hi > /work/out.txt"})
	doneArgs, _ := json.Marshal(map[string]string{"summary": "all done"})
	ft := &fakeTransport{
		llmTurns: []abi.LLMMessage{
			{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "1", Name: "bash", Arguments: string(bashArgs)}}},
			{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "2", Name: "done", Arguments: string(doneArgs)}}},
		},
	}
	client := NewClient(ft)
	agent := NewAgent(Config{Goal: "g", MaxTurns: 8, Capabilities: abi.Capabilities{Steer: true}}, client)

	summary, err := agent.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary != "all done" {
		t.Fatalf("summary = %q", summary)
	}
	if ft.msgRecvCalls != 2 {
		t.Fatalf("msg.recv calls = %d, want 2 (one per turn, no message either time)", ft.msgRecvCalls)
	}
}

func TestSteerCapabilityOffMeansNoPolling(t *testing.T) {
	doneArgs, _ := json.Marshal(map[string]string{"summary": "ok"})
	ft := &fakeTransport{
		llmTurns: []abi.LLMMessage{
			{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "1", Name: "done", Arguments: string(doneArgs)}}},
		},
	}
	client := NewClient(ft)
	// Steer capability not granted (default Config zero value).
	agent := NewAgent(Config{Goal: "g", MaxTurns: 8}, client)

	if _, err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ft.msgRecvCalls != 0 {
		t.Fatalf("msg.recv calls = %d, want 0 (Steer capability not granted)", ft.msgRecvCalls)
	}
}

func TestSteerEveryThrottle(t *testing.T) {
	ft := &fakeTransport{
		llmTurns: []abi.LLMMessage{
			{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "1", Name: "bash", Arguments: `{"script":"true"}`}}},
			{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "2", Name: "bash", Arguments: `{"script":"true"}`}}},
			{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "3", Name: "done", Arguments: `{"summary":"ok"}`}}},
		},
	}
	client := NewClient(ft)
	agent := NewAgent(Config{Goal: "g", MaxTurns: 8, Capabilities: abi.Capabilities{Steer: true}}, client)
	agent.SteerEvery = 2 // poll on turns 0, 2, 4, ...

	if _, err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ft.msgRecvCalls != 2 {
		t.Fatalf("msg.recv calls = %d, want 2 (turns 0 and 2 of 3)", ft.msgRecvCalls)
	}
}

// ----- edit_file / multi_edit / plan: replay determinism -----

// scriptedEditPlanTurns builds a fixed sequence of tool calls exercising
// edit_file, multi_edit, and plan together, followed by "done". Both are pure
// in-guest computation over the VFS and agent state with no hostcalls (spec
// 06), so running the identical script twice must reproduce byte-identical
// VFS contents and message transcripts — that is the replay guarantee this
// test stands in for (no new event kind is introduced, so there is nothing
// for a host-level event log replay to diverge on; determinism reduces to
// "same inputs, same outputs" at the guest level, which this checks directly).
func scriptedEditPlanTurns() []abi.LLMMessage {
	createArgs, _ := json.Marshal(map[string]any{"path": "/work/app.go", "old": "", "new": "func handler() {}\n"})
	multiArgs, _ := json.Marshal(map[string]any{
		"path": "/work/app.go",
		"edits": []map[string]string{
			{"old": "func handler() {}\n", "new": "func handler(ctx context.Context) {\n\treturn\n}\n"},
		},
	})
	planArgs, _ := json.Marshal(map[string]any{
		"op": "set",
		"items": []map[string]any{
			{"id": 1, "text": "add ctx param", "status": "done"},
			{"id": 2, "text": "run tests", "status": "pending"},
		},
	})
	doneArgs, _ := json.Marshal(map[string]string{"summary": "refactored handler"})
	return []abi.LLMMessage{
		{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "1", Name: "edit_file", Arguments: string(createArgs)}}},
		{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "2", Name: "multi_edit", Arguments: string(multiArgs)}}},
		{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "3", Name: "plan", Arguments: string(planArgs)}}},
		{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "4", Name: "done", Arguments: string(doneArgs)}}},
	}
}

func runScriptedEditPlanAgent(t *testing.T) *Agent {
	t.Helper()
	ft := &fakeTransport{llmTurns: scriptedEditPlanTurns()}
	agent := NewAgent(Config{Goal: "refactor", MaxTurns: 8}, NewClient(ft))
	if _, err := agent.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return agent
}

func TestReplayEditAndPlanReproducesIdenticalVFSAndTranscript(t *testing.T) {
	first := runScriptedEditPlanAgent(t)
	second := runScriptedEditPlanAgent(t)

	firstFiles := first.VFS().SnapshotFull()
	secondFiles := second.VFS().SnapshotFull()
	if len(firstFiles) != len(secondFiles) {
		t.Fatalf("file counts differ: %d vs %d", len(firstFiles), len(secondFiles))
	}
	for p, data := range firstFiles {
		if string(secondFiles[p]) != string(data) {
			t.Errorf("VFS %s diverged:\nfirst:  %q\nsecond: %q", p, data, secondFiles[p])
		}
	}

	firstJSON, _ := json.Marshal(first.messages)
	secondJSON, _ := json.Marshal(second.messages)
	if string(firstJSON) != string(secondJSON) {
		t.Errorf("transcripts diverged:\nfirst:  %s\nsecond: %s", firstJSON, secondJSON)
	}

	if len(first.plan) != len(second.plan) {
		t.Fatalf("plan lengths differ: %d vs %d", len(first.plan), len(second.plan))
	}
	for i := range first.plan {
		if first.plan[i] != second.plan[i] {
			t.Errorf("plan item %d diverged: %+v vs %+v", i, first.plan[i], second.plan[i])
		}
	}

	// Sanity: the run actually exercised the edit/plan path, so the assertions
	// above are meaningful and not vacuously true.
	want := "func handler(ctx context.Context) {\n\treturn\n}\n"
	if got := string(firstFiles["/work/app.go"]); got != want {
		t.Fatalf("app.go = %q, want %q", got, want)
	}
	if len(first.plan) != 2 || first.plan[0].Status != PlanDone {
		t.Fatalf("plan = %+v, want the scripted two-item plan", first.plan)
	}
}
