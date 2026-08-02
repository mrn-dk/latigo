package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// sseWrite writes one OpenAI-style SSE data line + the terminating DONE marker.
func sseWrite(w http.ResponseWriter, chunks ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	for _, c := range chunks {
		w.Write([]byte("data: " + c + "\n\n"))
	}
	w.Write([]byte("data: [DONE]\n\n"))
}

// newMockSSEEndpoint serves scripted SSE streams, one per request.
type mockSSEEndpoint struct {
	server  *httptest.Server
	scripts [][]string
	calls   atomic.Int32
}

func newMockSSEEndpoint(t *testing.T, scripts ...[]string) *mockSSEEndpoint {
	m := &mockSSEEndpoint{scripts: scripts}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(m.calls.Add(1)) - 1
		if i < len(m.scripts) {
			sseWrite(w, m.scripts[i]...)
			return
		}
		sseWrite(w, `{"choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockSSEEndpoint) URL() string { return m.server.URL + "/v1" }

type mockEndpoint struct {
	server  *httptest.Server
	calls   atomic.Int32
	scripts []chatResponse
}

func newMockEndpoint(t *testing.T, scripts ...chatResponse) *mockEndpoint {
	m := &mockEndpoint{scripts: scripts}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(m.calls.Add(1)) - 1
		var resp chatResponse
		if i < len(m.scripts) {
			resp = m.scripts[i]
		} else {
			resp.Choices = append(resp.Choices, choiceMsg(Message{Content: "done"}, "stop"))
		}
		resp.Model = "mock-model"
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockEndpoint) URL() string { return m.server.URL + "/v1" }

type choiceT struct {
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

func choiceMsg(m Message, fr string) choiceT { return choiceT{Message: m, FinishReason: fr} }

func respWithToolCall(name, args string) chatResponse {
	var resp chatResponse
	resp.Choices = append(resp.Choices, choiceMsg(Message{
		Role: "assistant",
		ToolCalls: []ToolCall{{
			ID:       "call-1",
			Type:     "function",
			Function: FuncCall{Name: name, Arguments: args},
		}},
	}, "tool_calls"))
	return resp
}

// setupAgent builds an agent on a temp workspace + temp event log, applying the
// same defaults LoadConfig would so a Config with zero-value limits still runs.
func setupAgent(t *testing.T, cfg Config) *Agent {
	t.Helper()
	if cfg.MaxTurns == 0 {
		cfg.MaxTurns = defaultMaxTurns
	}
	if cfg.MaxWallClockSeconds == 0 {
		cfg.MaxWallClockSeconds = defaultMaxWallClock
	}
	if cfg.ShellExecTimeoutSeconds == 0 {
		cfg.ShellExecTimeoutSeconds = defaultShellExecTimeout
	}
	if cfg.Workspace == "" {
		cfg.Workspace = t.TempDir()
	}
	if err := os.MkdirAll(cfg.Workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if cfg.EventLog == "" {
		cfg.EventLog = filepath.Join(t.TempDir(), "events.jsonl")
	}
	cfg.Model = "mock-model"

	allow := NewAllowlist()
	allow.Add(AllowEntry{Name: "ls", OneLine: "list files"})
	allow.Add(AllowEntry{Name: "cat", OneLine: "print files"})

	log, err := OpenEventLog(cfg.EventLog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })

	shell := NewShell(cfg.Workspace, allow)
	shell.Env = []string{"PATH=" + os.Getenv("PATH")}

	llm := &LLMClient{Model: cfg.Model}
	return NewAgent(cfg, llm, shell, allow, log, nil)
}

func TestAgentEndToEndShellThenFinish(t *testing.T) {
	agent := setupAgent(t, Config{Goal: "list the files and finish"})
	writeFile(t, filepath.Join(agent.cfg.Workspace, "note.txt"), "hi\n")

	endpoint := newMockEndpoint(t,
		respWithToolCall("shell", `{"command":"ls"}`),
		respWithToolCall("finish", `{"output":"done"}`),
	)
	agent.llm.BaseURL = endpoint.URL()

	out, reason, err := agent.Run(t.Context())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if reason != "finished" {
		t.Fatalf("reason=%q out=%q", reason, out)
	}
	var s string
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("finish output not a string: %q", out)
	}
	if s != "done" {
		t.Fatalf("finish output=%q", out)
	}
	lt, _ := LoadTranscript(agent.cfg.EventLog)
	found := false
	for _, m := range lt.Messages {
		if m.Role == "tool" && strings.Contains(m.Content, "note.txt") {
			found = true
		}
	}
	if !found {
		t.Fatalf("ls result missing note.txt: %+v", lt.Messages)
	}
}

func TestAgentFinishSchemaValidation(t *testing.T) {
	schema := &Schema{
		Type:     "object",
		Required: []string{"count"},
		Properties: map[string]*Schema{
			"count": {Type: "integer", Minimum: ptrFloat(0)},
		},
		AdditionalProperties: boolFalse(),
	}
	agent := setupAgent(t, Config{Goal: "count files"})
	agent.outputSchema = schema
	agent.tools = builtinTools(schema)

	endpoint := newMockEndpoint(t,
		respWithToolCall("finish", `{"output":"not an object"}`),
		respWithToolCall("finish", `{"output":{"count":3}}`),
	)
	agent.llm.BaseURL = endpoint.URL()

	out, reason, err := agent.Run(t.Context())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if reason != "finished" {
		t.Fatalf("reason=%q", reason)
	}
	var got struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("finish output not object: %q", out)
	}
	if got.Count != 3 {
		t.Fatalf("count=%d", got.Count)
	}
}

func TestAgentMaxTurnsTerminates(t *testing.T) {
	agent := setupAgent(t, Config{Goal: "loop forever", MaxTurns: 2})
	endpoint := newMockEndpoint(t,
		respWithToolCall("shell", `{"command":"true"}`),
		respWithToolCall("shell", `{"command":"true"}`),
		respWithToolCall("shell", `{"command":"true"}`),
	)
	agent.llm.BaseURL = endpoint.URL()

	_, reason, err := agent.Run(t.Context())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if reason != "max_turns" {
		t.Fatalf("reason=%q", reason)
	}
}

func TestAgentStreamEndToEndSinkAndLog(t *testing.T) {
	// Two streamed turns: turn 0 streams text + a shell tool call; turn 1
	// streams a finish.
	sh := []string{
		`{"choices":[{"index":0,"delta":{"content":"let me "},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":"check"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"shell","arguments":"{\"command\":\"ls\"}"}}]},"finish_reason":"tool_calls"}]}`,
	}
	fin := []string{
		`{"choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}]}`,
		`{"choices":[],"model":"mock-model","usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`,
	}
	sse := newMockSSEEndpoint(t, sh, fin)
	agent := setupAgent(t, Config{Goal: "stream then finish", Stream: true})
	agent.llm.BaseURL = sse.URL()
	writeFile(t, filepath.Join(agent.cfg.Workspace, "note.txt"), "hi\n")

	var sinkGot strings.Builder
	agent.DeltaSink = func(text string) { sinkGot.WriteString(text) }

	out, reason, err := agent.Run(t.Context())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if reason != "answered" {
		t.Fatalf("reason=%q out=%q", reason, out)
	}
	// The sink saw the streamed text from both turns.
	if sinkGot.String() != "let me checkdone" {
		t.Fatalf("sink=%q want %q", sinkGot.String(), "let me checkdone")
	}
	// The event log's llm events carry the full assembled messages, not deltas.
	lt, _ := LoadTranscript(agent.cfg.EventLog)
	if len(lt.Messages) < 2 {
		t.Fatalf("transcript too short: %+v", lt.Messages)
	}
	first := lt.Messages[0]
	if first.Role != "assistant" || first.Content != "let me check" || len(first.ToolCalls) != 1 {
		t.Fatalf("first llm event wrong: %+v", first)
	}
	if first.ToolCalls[0].Function.Name != "shell" || first.ToolCalls[0].Function.Arguments != `{"command":"ls"}` {
		t.Fatalf("assembled tool call wrong: %+v", first.ToolCalls[0])
	}
}

func TestAgentResumeContinuesTranscript(t *testing.T) {
	// First run: one shell call, then finish.
	agent := setupAgent(t, Config{Goal: "list files and finish"})
	writeFile(t, filepath.Join(agent.cfg.Workspace, "seed.txt"), "x\n")
	endpoint := newMockEndpoint(t,
		respWithToolCall("shell", `{"command":"ls"}`),
		respWithToolCall("finish", `{"output":"done"}`),
	)
	agent.llm.BaseURL = endpoint.URL()
	if _, _, err := agent.Run(t.Context()); err != nil {
		t.Fatal(err)
	}

	// Resume: a fresh agent loads the transcript and continues with a new goal.
	resumed := setupAgent(t, Config{Resume: true, EventLog: agent.cfg.EventLog, Workspace: agent.cfg.Workspace})
	lt, err := LoadTranscript(resumed.cfg.EventLog)
	if err != nil {
		t.Fatal(err)
	}
	// The rebuilt transcript includes the prior assistant + tool turns.
	if len(lt.Messages) < 2 {
		t.Fatalf("resume transcript too short: %+v", lt.Messages)
	}
}

func ptrFloat(v float64) *float64 { return &v }

func TestAgentSystemPromptDefault(t *testing.T) {
	agent := setupAgent(t, Config{Goal: "g"})
	if agent.SystemPrompt != defaultSystemPrompt {
		t.Fatalf("default prompt not used; got %q", agent.SystemPrompt)
	}
}

func TestAgentSystemPromptOverride(t *testing.T) {
	agent := setupAgent(t, Config{Goal: "g", SystemPrompt: "my custom prompt"})
	if agent.SystemPrompt != "my custom prompt" {
		t.Fatalf("override not applied; got %q", agent.SystemPrompt)
	}
}

func TestAgentSystemPromptAppendToDefault(t *testing.T) {
	agent := setupAgent(t, Config{Goal: "g", AppendSystemPrompt: "extra rules"})
	want := defaultSystemPrompt + "\n\n" + "extra rules"
	if agent.SystemPrompt != want {
		t.Fatalf("append-to-default wrong;\n got=%q\nwant=%q", agent.SystemPrompt, want)
	}
}

func TestAgentSystemPromptAppendToOverride(t *testing.T) {
	agent := setupAgent(t, Config{Goal: "g", SystemPrompt: "custom", AppendSystemPrompt: "extra"})
	want := "custom" + "\n\n" + "extra"
	if agent.SystemPrompt != want {
		t.Fatalf("append-to-override wrong; got %q want %q", agent.SystemPrompt, want)
	}
}
