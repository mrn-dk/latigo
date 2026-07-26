package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mrn-dk/latigo/abi"
)

// LLMClient talks to an OpenAI-compatible /chat/completions endpoint. Mortise
// and other OpenAI-shaped gateways work unchanged.
type LLMClient struct {
	BaseURL    string
	APIKey     string
	Model      string
	HTTPClient *http.Client

	// Retry configures the client's internal retry/backoff behaviour. Retries
	// happen entirely inside call() (below the durability boundary): a single
	// llm.call hostcall still produces exactly one recorded result, so retries
	// are invisible to the event log and to replay. The zero value (as would
	// result from constructing an LLMClient literal directly rather than via
	// NewLLMClient) means MaxAttempts <= 0, which call() treats as "exactly one
	// attempt, no retries" — today's behaviour.
	Retry LLMRetry

	// sleep is the injectable backoff waiter, defaulting to a context-aware
	// sleep. It returns ctx.Err() if the wait is cut short by cancellation, so
	// a cancelled run does not sit out a long Retry-After. Tests override it to
	// make retry/backoff assertions instant.
	sleep func(ctx context.Context, d time.Duration) error
}

// sleepCtx waits for d or until ctx is done, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// LLMRetry bounds the client's internal retry/backoff behaviour.
type LLMRetry struct {
	// MaxAttempts is the total number of attempts (including the first),
	// e.g. 5 means up to 4 retries after the initial attempt.
	MaxAttempts int
	// BaseDelay is the backoff base; attempt N (1-indexed) waits up to
	// BaseDelay * 2^(N-1), jittered, before attempt N+1.
	BaseDelay time.Duration
	// MaxDelay caps the computed backoff. An explicit Retry-After is honoured
	// even when it exceeds MaxDelay — retrying earlier than the provider asked
	// just earns another 429 — but MaxTotalWait still bounds the whole call.
	MaxDelay time.Duration
	// MaxTotalWait bounds the cumulative time call() will spend sleeping
	// between attempts. When the next wait would exceed what remains, call()
	// gives up immediately rather than blocking the hostcall, and surfaces the
	// classified error (with the provider's Retry-After hint) instead. This is
	// what stops a hostile or misconfigured `Retry-After: 86400` from parking a
	// run for a day. 0 means unbounded.
	MaxTotalWait time.Duration
	// RetryOn decides whether a given failure is worth retrying. status is 0
	// for transport-level failures (no HTTP response was received). The
	// default retries 429, 5xx, and connection/timeout errors.
	RetryOn func(status int, err error) bool
}

// defaultLLMRetry is the retry policy NewLLMClient installs.
func defaultLLMRetry() LLMRetry {
	return LLMRetry{
		MaxAttempts:  5,
		BaseDelay:    500 * time.Millisecond,
		MaxDelay:     30 * time.Second,
		MaxTotalWait: 60 * time.Second,
		RetryOn:      defaultRetryOn,
	}
}

// defaultRetryOn retries 429, all 5xx, and transport-level connection/timeout
// failures. Other 4xx statuses (bad request, auth, not found, ...) are not
// retried since a retry cannot succeed without the caller changing something.
func defaultRetryOn(status int, err error) bool {
	switch {
	case status == http.StatusTooManyRequests:
		return true
	case status >= 500 && status < 600:
		return true
	case status != 0:
		return false
	}
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// NewLLMClient builds a client. baseURL should be the API root, e.g.
// https://api.openai.com/v1 or a Mortise endpoint.
func NewLLMClient(baseURL, apiKey, model string) *LLMClient {
	return &LLMClient{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		APIKey:     apiKey,
		Model:      model,
		HTTPClient: &http.Client{Timeout: 120 * time.Second},
		Retry:      defaultLLMRetry(),
		sleep:      sleepCtx,
	}
}

// LLM registers the llm.call handler against this client.
func (h *Host) LLM(c *LLMClient) {
	h.Handle(abi.OpLLMCall, handler(c.call))
}

// ----- wire types (OpenAI chat completions) -----

type oaiToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type oaiTool struct {
	Type     string          `json:"type"`
	Function oaiToolFunction `json:"function"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content"`
	Name       string        `json:"name,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
}

type oaiRequest struct {
	Model       string       `json:"model"`
	Messages    []oaiMessage `json:"messages"`
	Tools       []oaiTool    `json:"tools,omitempty"`
	Temperature float64      `json:"temperature,omitempty"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
}

type oaiResponse struct {
	Choices []struct {
		Message      oaiMessage `json:"message"`
		FinishReason string     `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// call is the llm.call handler. It retries internally (bounded exponential
// backoff + jitter, honouring Retry-After) so that exactly one result is ever
// recorded for a single hostcall: retries live below the durability boundary
// and are invisible to the event log and to replay.
func (c *LLMClient) call(ctx context.Context, req abi.LLMCallRequest) (abi.LLMCallResponse, error) {
	model := req.Model
	if model == "" {
		model = c.Model
	}
	wire := oaiRequest{
		Model:       model,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	}
	for _, m := range req.Messages {
		om := oaiMessage{Role: m.Role, Content: m.Content, Name: m.Name, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			oc := oaiToolCall{ID: tc.ID, Type: "function"}
			oc.Function.Name = tc.Name
			oc.Function.Arguments = tc.Arguments
			om.ToolCalls = append(om.ToolCalls, oc)
		}
		wire.Messages = append(wire.Messages, om)
	}
	for _, t := range req.Tools {
		wire.Tools = append(wire.Tools, oaiTool{
			Type:     "function",
			Function: oaiToolFunction{Name: t.Name, Description: t.Description, Parameters: json.RawMessage(t.Parameters)},
		})
	}

	body, err := json.Marshal(wire)
	if err != nil {
		return abi.LLMCallResponse{}, err
	}

	retry := c.Retry
	if retry.MaxAttempts <= 0 {
		retry.MaxAttempts = 1
	}
	if retry.RetryOn == nil {
		retry.RetryOn = defaultRetryOn
	}
	sleep := c.sleep
	if sleep == nil {
		sleep = sleepCtx
	}

	var (
		out          abi.LLMCallResponse
		status       int
		retryAfterMS int
		callErr      error
		waited       time.Duration
	)
	for attempt := 1; attempt <= retry.MaxAttempts; attempt++ {
		out, status, retryAfterMS, callErr = c.doOnce(ctx, body)
		if callErr == nil {
			return out, nil
		}
		if attempt == retry.MaxAttempts || !retry.RetryOn(status, callErr) {
			break
		}
		if ctx.Err() != nil {
			return abi.LLMCallResponse{}, ctx.Err()
		}
		delay := backoffDelay(attempt, retry.BaseDelay, retry.MaxDelay, retryAfterMS)
		// Bound the total time this hostcall can spend asleep. A provider
		// asking for longer than the budget allows ends the call now: the
		// caller gets a classified error plus the retry-after hint and can
		// decide for itself, which beats blocking the guest indefinitely.
		if retry.MaxTotalWait > 0 && waited+delay > retry.MaxTotalWait {
			break
		}
		if err := sleep(ctx, delay); err != nil {
			return abi.LLMCallResponse{}, err
		}
		waited += delay
	}
	return abi.LLMCallResponse{}, classifyLLMError(status, retryAfterMS, callErr)
}

// doOnce performs a single HTTP round trip. status is 0 when no HTTP response
// was received (a transport-level failure). retryAfterMS is the parsed
// Retry-After header, when the server sent one and returned a response.
func (c *LLMClient) doOnce(ctx context.Context, body []byte) (out abi.LLMCallResponse, status int, retryAfterMS int, err error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return abi.LLMCallResponse{}, 0, 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return abi.LLMCallResponse{}, 0, 0, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	retryAfterMS = parseRetryAfter(resp.Header.Get("Retry-After"))

	if resp.StatusCode >= 300 {
		return abi.LLMCallResponse{}, resp.StatusCode, retryAfterMS, fmt.Errorf("llm http %d: %s", resp.StatusCode, truncate(string(respBody), 400))
	}

	var oaiResp oaiResponse
	if err := json.Unmarshal(respBody, &oaiResp); err != nil {
		return abi.LLMCallResponse{}, resp.StatusCode, retryAfterMS, fmt.Errorf("llm decode: %w", err)
	}
	if oaiResp.Error != nil {
		return abi.LLMCallResponse{}, resp.StatusCode, retryAfterMS, fmt.Errorf("llm error: %s", oaiResp.Error.Message)
	}
	if len(oaiResp.Choices) == 0 {
		return abi.LLMCallResponse{}, resp.StatusCode, retryAfterMS, fmt.Errorf("llm returned no choices")
	}
	choice := oaiResp.Choices[0]
	msg := abi.LLMMessage{Role: choice.Message.Role, Content: choice.Message.Content}
	for _, tc := range choice.Message.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, abi.LLMToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return abi.LLMCallResponse{
		Message:      msg,
		FinishReason: choice.FinishReason,
		InputTokens:  oaiResp.Usage.PromptTokens,
		OutputTokens: oaiResp.Usage.CompletionTokens,
	}, resp.StatusCode, retryAfterMS, nil
}

// backoffDelay computes the wait before the next attempt: full-jitter
// exponential backoff based on attempt (1-indexed, the attempt that just
// failed), capped at max, but never less than an explicit Retry-After hint
// (which is treated as authoritative even if it exceeds max).
func backoffDelay(attempt int, base, max time.Duration, retryAfterMS int) time.Duration {
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	shift := attempt - 1
	if shift > 20 { // guard against overflow on pathological MaxAttempts
		shift = 20
	}
	d := base * time.Duration(int64(1)<<uint(shift))
	if max > 0 && d > max {
		d = max
	}
	delay := time.Duration(0)
	if d > 0 {
		delay = time.Duration(rand.Int63n(int64(d) + 1))
	}
	if retryAfterMS > 0 {
		if hint := time.Duration(retryAfterMS) * time.Millisecond; hint > delay {
			delay = hint
		}
	}
	return delay
}

// parseRetryAfter parses a Retry-After header (either delta-seconds or an
// HTTP-date) into milliseconds. Returns 0 when absent, malformed, or in the
// past.
func parseRetryAfter(v string) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return secs * 1000
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return int(d.Milliseconds())
		}
	}
	return 0
}

// classifyLLMError maps a terminal (post-retry) failure onto a stable ABI
// error code so the guest (or a host policy) can distinguish "give up
// entirely" from "the provider is rate-limited/overloaded/timing out".
func classifyLLMError(status, retryAfterMS int, err error) error {
	if err == nil {
		err = fmt.Errorf("llm call failed")
	}
	code := abi.ErrInternal
	switch {
	case status == http.StatusTooManyRequests:
		code = abi.ErrRateLimited
	case status >= 500 && status < 600:
		code = abi.ErrOverloaded
	case status == 0 && isTimeoutErr(err):
		code = abi.ErrTimeout
	}
	ce := &CodedError{Code: code, Msg: err.Error()}
	if retryAfterMS > 0 {
		if hint, hErr := json.Marshal(abi.LLMCallResponse{RetryAfterMS: retryAfterMS}); hErr == nil {
			ce.Result = hint
		}
	}
	return ce
}

func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
