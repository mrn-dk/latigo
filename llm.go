// Package main: llm.go is the OpenAI-compatible HTTP client Latigo uses to
// reach inference. Latigo has no other path to inference — it speaks the
// OpenAI-compatible chat completions dialect to any endpoint that implements
// it; which endpoint is configured by URL.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// LLMClient talks to an OpenAI-compatible endpoint over HTTP.
type LLMClient struct {
	BaseURL string // e.g. "http://localhost:8080/v1"
	APIKey  string
	Model   string
	HTTP    *http.Client
}

// LLMResult is the outcome of one chat completion call.
type LLMResult struct {
	Message      Message
	FinishReason string
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	Model        string
	Latency      time.Duration
	// RetryAfter is an advisory hint, parsed from a Retry-After header on a
	// 429/503, that the agent loop may use to back off.
	RetryAfter time.Duration
}

// DeltaSink receives text deltas as they arrive during a streamed chat
// completion. It is an ephemeral output path: the sink is used for live UX only
// and is never used to record the transcript or the event log — the assembled
// message is the record. A nil sink means streaming still works but deltas are
// not forwarded anywhere.
type DeltaSink func(text string)

// Call performs one chat completion. It does not retry — bounded retry with
// model-visible feedback lives in the agent loop (validate.go / agent.go); this
// is the single transport call.
func (c *LLMClient) Call(ctx context.Context, req chatRequest) (LLMResult, error) {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 120 * time.Second}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return LLMResult{}, fmt.Errorf("llm: marshal request: %w", err)
	}
	url := strings.TrimRight(c.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return LLMResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	start := time.Now()
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return LLMResult{}, fmt.Errorf("llm: request: %w", err)
	}
	defer resp.Body.Close()
	latency := time.Since(start)

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return LLMResult{}, fmt.Errorf("llm: read body: %w", err)
	}

	if resp.StatusCode >= 400 {
		var cr chatResponse
		_ = json.Unmarshal(raw, &cr) // best-effort parse of the OpenAI error shape
		msg := fmt.Sprintf("llm: http %d", resp.StatusCode)
		if cr.Error != nil && cr.Error.Message != "" {
			msg = fmt.Sprintf("llm: http %d: %s", resp.StatusCode, cr.Error.Message)
		}
		res := LLMResult{Latency: latency}
		if ra := parseRetryAfter(resp.Header.Get("Retry-After")); ra > 0 {
			res.RetryAfter = ra
		}
		return res, fmt.Errorf("%s", msg)
	}

	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return LLMResult{}, fmt.Errorf("llm: decode response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return LLMResult{Latency: latency}, fmt.Errorf("llm: empty choices")
	}
	ch := cr.Choices[0]
	model := cr.Model
	if model == "" {
		model = req.Model
	}
	return LLMResult{
		Message:      ch.Message,
		FinishReason: ch.FinishReason,
		InputTokens:  cr.Usage.PromptTokens,
		OutputTokens: cr.Usage.CompletionTokens,
		TotalTokens:  cr.Usage.TotalTokens,
		Model:        model,
		Latency:      latency,
	}, nil
}

// wireMessages converts the internal transcript into the OpenAI wire shape:
// text-only messages send a string content; multimodal messages send an array
// of content parts. Tool-role messages keep their string content.
func wireMessages(msgs []Message) []messageWire {
	out := make([]messageWire, 0, len(msgs))
	for _, m := range msgs {
		w := messageWire{Role: m.Role, ToolCallID: m.ToolCallID, Name: m.Name, ToolCalls: m.ToolCalls}
		if len(m.Parts) > 0 {
			w.Content = m.Parts
		} else {
			w.Content = m.Content
		}
		out = append(out, w)
	}
	return out
}

// parseRetryAfter parses the HTTP Retry-After header as seconds (delta-seconds
// form only; the absolute-date form is intentionally ignored — Latigo's clock
// is the runtime's, and a relative hint is all the loop needs).
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var secs int
	if _, err := fmt.Sscanf(v, "%d", &secs); err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// CallStream performs one streamed chat completion. It posts the request with
// `stream: true`, reads the Server-Sent Events delta stream, forwards text
// deltas to sink inline as they arrive, and assembles the full assistant
// message and usage. It returns the same LLMResult shape as Call.
//
// If the streaming request fails before the first delta is received, it falls
// back to a single non-streaming Call for the turn (degrade, don't fail). After
// deltas have started, a stream error is terminal: the partial message cannot
// be safely completed, so it is surfaced as an llm error rather than retried.
func (c *LLMClient) CallStream(ctx context.Context, req chatRequest, sink DeltaSink) (LLMResult, error) {
	req.Stream = true
	req.StreamOptions = &streamOptions{IncludeUsage: true}

	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 120 * time.Second}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return LLMResult{}, fmt.Errorf("llm: marshal request: %w", err)
	}
	url := strings.TrimRight(c.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return LLMResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	start := time.Now()
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		// Pre-response failure: degrade to non-streaming.
		return c.degradeToCall(ctx, req, start, sink)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		// Error before any delta: degrade to non-streaming rather than fail.
		return c.degradeToCall(ctx, req, start, sink)
	}

	res, deltasStarted, err := c.readSSE(ctx, resp, req.Model, sink)
	res.Latency = time.Since(start)
	if err != nil {
		if !deltasStarted {
			// No deltas were forwarded yet; a non-streaming retry is safe.
			return c.degradeToCall(ctx, req, start, sink)
		}
		return res, fmt.Errorf("llm: stream: %w", err)
	}
	return res, nil
}

// degradeToCall falls back to a non-streaming Call, clearing the streaming
// request fields first. It is used when a streaming attempt fails before any
// delta has been forwarded to the sink.
func (c *LLMClient) degradeToCall(ctx context.Context, req chatRequest, start time.Time, sink DeltaSink) (LLMResult, error) {
	req.Stream = false
	req.StreamOptions = nil
	res, err := c.Call(ctx, req)
	// Preserve the latency baseline so the caller's timing includes the failed
	// streaming attempt, and forward the whole assembled text to the sink now
	// so a live consumer still sees the turn's output.
	if err == nil && sink != nil && res.Message.Content != "" {
		sink(res.Message.Content)
	}
	res.Latency += time.Since(start)
	return res, err
}

// readSSE parses the SSE stream into an assembled LLMResult. deltasStarted is
// true once at least one delta has been forwarded to sink or accumulated, so the
// caller knows whether a mid-stream error leaves partial output the consumer
// may have seen.
func (c *LLMClient) readSSE(ctx context.Context, resp *http.Response, reqModel string, sink DeltaSink) (LLMResult, bool, error) {
	var res LLMResult
	var content strings.Builder
	// toolCalls accumulates fragments by index, the key the endpoint uses to
	// refer to a tool call across deltas.
	type tcFrag struct {
		ID        string
		Name      string
		Arguments string
	}
	frags := map[int]*tcFrag{}
	var finishReason string
	var model string
	var usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	}
	deltasStarted := false

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		if ctx.Err() != nil {
			return res, deltasStarted, ctx.Err()
		}
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue // SSE comment or keep-alive; ignore
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var ch streamChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			return res, deltasStarted, fmt.Errorf("decode sse chunk: %w", err)
		}
		deltasStarted = true
		if ch.Model != "" {
			model = ch.Model
		}
		if len(ch.Choices) > 0 {
			d := ch.Choices[0].Delta
			if d.Content != "" {
				content.WriteString(d.Content)
				if sink != nil {
					sink(d.Content)
				}
			}
			if len(d.ToolCalls) > 0 {
				for _, tc := range d.ToolCalls {
					idx := tc.Index
					f := frags[idx]
					if f == nil {
						f = &tcFrag{}
						frags[idx] = f
					}
					if tc.ID != "" {
						f.ID = tc.ID
					}
					if tc.Type != "" {
						f.Name = tc.Function.Name
					}
					if tc.Function.Name != "" {
						f.Name = tc.Function.Name
					}
					f.Arguments += tc.Function.Arguments
				}
			}
			if ch.Choices[0].FinishReason != "" {
				finishReason = ch.Choices[0].FinishReason
			}
		}
		// The final usage chunk (when include_usage is honoured) arrives in a
		// chunk whose choices array is empty and usage is populated.
		if ch.Usage != nil {
			usage.PromptTokens = ch.Usage.PromptTokens
			usage.CompletionTokens = ch.Usage.CompletionTokens
			usage.TotalTokens = ch.Usage.TotalTokens
		}
	}
	if err := sc.Err(); err != nil {
		return res, deltasStarted, err
	}

	// Assemble the message exactly as the non-streaming JSON path would.
	msg := Message{Role: "assistant", Content: content.String()}
	if len(frags) > 0 {
		indices := make([]int, 0, len(frags))
		for i := range frags {
			indices = append(indices, i)
		}
		sort.Ints(indices)
		tcs := make([]ToolCall, 0, len(indices))
		for _, i := range indices {
			f := frags[i]
			tcs = append(tcs, ToolCall{ID: f.ID, Type: "function", Function: FuncCall{Name: f.Name, Arguments: f.Arguments}})
		}
		msg.ToolCalls = tcs
	}
	if model == "" {
		model = reqModel
	}
	res.Message = msg
	res.FinishReason = finishReason
	res.InputTokens = usage.PromptTokens
	res.OutputTokens = usage.CompletionTokens
	res.TotalTokens = usage.TotalTokens
	// When the endpoint does not honour include_usage, fall back to a cheap
	// estimate of the assembled message so max_total_tokens is still enforced.
	if res.TotalTokens == 0 {
		est := estimateTokens([]Message{msg})
		res.OutputTokens = est
		res.TotalTokens = est
	}
	res.Model = model
	return res, deltasStarted, nil
}

// streamChunk is the relevant subset of one OpenAI SSE chat-completion chunk.
type streamChunk struct {
	Choices []struct {
		Index int   `json:"index"`
		Delta delta `json:"delta"`
		// FinishReason is set on the final chunk for a choice (null/empty before).
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Model string `json:"model"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage,omitempty"`
}

// delta is the incremental content of one SSE chunk.
type delta struct {
	Content   string          `json:"content,omitempty"`
	ToolCalls []deltaToolCall `json:"tool_calls,omitempty"`
}

// deltaToolCall is a fragment of a tool call. Function name and arguments
// arrive across multiple chunks; the index identifies which tool call a
// fragment belongs to.
type deltaToolCall struct {
	Index    int      `json:"index"`
	ID       string   `json:"id,omitempty"`
	Type     string   `json:"type,omitempty"`
	Function FuncCall `json:"function"`
}
