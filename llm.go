// Package main: llm.go is the OpenAI-compatible HTTP client Latigo uses to
// reach inference. Latigo has no other path to inference — it speaks the
// OpenAI-compatible chat completions dialect to any endpoint that implements
// it; which endpoint is configured by URL.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
