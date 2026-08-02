package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseLines builds an OpenAI-style SSE body from a sequence of JSON chunk payloads.
func sseLines(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: ")
		b.WriteString(c)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func sseServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	if status == 0 {
		status = 200
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
}

func TestCallStreamAssemblesText(t *testing.T) {
	body := sseLines(
		`{"choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":", "},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":"world!"},"finish_reason":"stop"}]}`,
		`{"choices":[],"model":"mock-model","usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}`,
	)
	srv := sseServer(t, body, 0)
	defer srv.Close()

	c := &LLMClient{BaseURL: srv.URL + "/v1", Model: "mock-model"}
	var got []string
	sink := func(text string) { got = append(got, text) }
	res, err := c.CallStream(context.Background(), chatRequest{Model: "mock-model"}, sink)
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	if res.Message.Content != "Hello, world!" {
		t.Fatalf("content=%q want %q", res.Message.Content, "Hello, world!")
	}
	if res.FinishReason != "stop" {
		t.Fatalf("finish=%q", res.FinishReason)
	}
	if res.TotalTokens != 13 {
		t.Fatalf("total=%d want 13", res.TotalTokens)
	}
	if len(got) != 3 || strings.Join(got, "") != "Hello, world!" {
		t.Fatalf("sink got %v", got)
	}
}

func TestCallStreamAssemblesToolCalls(t *testing.T) {
	body := sseLines(
		`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"shell","arguments":""}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"comm"}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"and\":\"ls\"}"}}]},"finish_reason":"tool_calls"}]}`,
	)
	srv := sseServer(t, body, 0)
	defer srv.Close()

	c := &LLMClient{BaseURL: srv.URL + "/v1", Model: "mock-model"}
	res, err := c.CallStream(context.Background(), chatRequest{Model: "mock-model"}, nil)
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	if len(res.Message.ToolCalls) != 1 {
		t.Fatalf("toolcalls=%d want 1: %+v", len(res.Message.ToolCalls), res.Message.ToolCalls)
	}
	tc := res.Message.ToolCalls[0]
	if tc.ID != "call-1" || tc.Function.Name != "shell" || tc.Function.Arguments != `{"command":"ls"}` {
		t.Fatalf("assembled tc=%+v", tc)
	}
	if res.FinishReason != "tool_calls" {
		t.Fatalf("finish=%q", res.FinishReason)
	}
}

func TestCallStreamNoSinkStillAssembles(t *testing.T) {
	body := sseLines(`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`)
	srv := sseServer(t, body, 0)
	defer srv.Close()
	c := &LLMClient{BaseURL: srv.URL + "/v1", Model: "mock-model"}
	res, err := c.CallStream(context.Background(), chatRequest{Model: "mock-model"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Message.Content != "x" {
		t.Fatalf("content=%q", res.Message.Content)
	}
}

func TestCallStreamFallsBackOnHTTPError(t *testing.T) {
	// Streaming server returns 500; the non-streaming fallback returns a real choice.
	sse := sseServer(t, "", 500)
	defer sse.Close()
	non := sseServer(t, "", 0)
	defer non.Close()
	_ = non
	// Point CallStream at the failing streaming server; the fallback Call goes
	// to the same BaseURL, so to prove degradation we use a server that 500s on
	// the streaming Accept header but 200s otherwise.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "text/event-stream" {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"fallback"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2},"model":"mock-model"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &LLMClient{BaseURL: srv.URL + "/v1", Model: "mock-model"}
	var sinkGot strings.Builder
	res, err := c.CallStream(context.Background(), chatRequest{Model: "mock-model"}, func(s string) { sinkGot.WriteString(s) })
	if err != nil {
		t.Fatalf("expected fallback, got err: %v", err)
	}
	if res.Message.Content != "fallback" {
		t.Fatalf("content=%q want fallback", res.Message.Content)
	}
	if sinkGot.String() != "fallback" {
		t.Fatalf("degrade should forward assembled text to sink once, got %q", sinkGot.String())
	}
}

func TestCallStreamUsageAbsentEstimates(t *testing.T) {
	// No usage chunk at all; the harness estimates from the assembled message.
	body := sseLines(`{"choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":"stop"}]}`)
	srv := sseServer(t, body, 0)
	defer srv.Close()
	c := &LLMClient{BaseURL: srv.URL + "/v1", Model: "mock-model"}
	res, err := c.CallStream(context.Background(), chatRequest{Model: "mock-model"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalTokens == 0 {
		t.Fatalf("expected a non-zero estimate, got 0")
	}
}
