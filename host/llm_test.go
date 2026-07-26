package host

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mrn-dk/latigo/abi"
)

// newTestLLMClient builds an LLMClient pointed at srv with a fast, injectable
// sleep so retry/backoff tests don't actually wait.
func newTestLLMClient(srv *httptest.Server, retry LLMRetry) (*LLMClient, *[]time.Duration) {
	var slept []time.Duration
	c := &LLMClient{
		BaseURL:    srv.URL,
		Model:      "test-model",
		HTTPClient: srv.Client(),
		Retry:      retry,
		sleep: func(d time.Duration) {
			slept = append(slept, d)
		},
	}
	return c, &slept
}

func chatOKBody(content string) []byte {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{
			{
				"message":       map[string]any{"role": "assistant", "content": content},
				"finish_reason": "stop",
			},
		},
	})
	return b
}

// TestLLMRetrySucceedsAfterRateLimit: a 429 with Retry-After, then a 200,
// yields exactly one successful result and the expected (Retry-After-driven)
// backoff.
func TestLLMRetrySucceedsAfterRateLimit(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(chatOKBody("hello"))
	}))
	defer srv.Close()

	c, slept := newTestLLMClient(srv, LLMRetry{
		MaxAttempts: 3,
		BaseDelay:   500 * time.Millisecond,
		MaxDelay:    30 * time.Second,
		RetryOn:     defaultRetryOn,
	})

	resp, err := c.call(context.Background(), abi.LLMCallRequest{Messages: []abi.LLMMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.Message.Content != "hello" {
		t.Errorf("content = %q, want %q", resp.Message.Content, "hello")
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	if len(*slept) != 1 {
		t.Fatalf("slept %d times, want 1: %v", len(*slept), *slept)
	}
	// The server's Retry-After (1s) is authoritative and should drive the
	// single backoff wait exactly, since it exceeds the jittered exponential
	// base delay ceiling for attempt 1 (up to 500ms).
	if (*slept)[0] != time.Second {
		t.Errorf("backoff = %v, want 1s (from Retry-After)", (*slept)[0])
	}
}

// TestLLMRetryBudgetExhausted: persistent 429s exhaust the retry budget and
// surface a single classified rate_limited error (not a raw/internal one).
func TestLLMRetryBudgetExhausted(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"still limited"}}`))
	}))
	defer srv.Close()

	c, slept := newTestLLMClient(srv, LLMRetry{
		MaxAttempts: 3,
		BaseDelay:   time.Millisecond, // keep any real timing negligible regardless
		MaxDelay:    10 * time.Millisecond,
		RetryOn:     defaultRetryOn,
	})

	_, err := c.call(context.Background(), abi.LLMCallRequest{Messages: []abi.LLMMessage{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (MaxAttempts)", calls)
	}
	if len(*slept) != 2 {
		t.Errorf("slept %d times, want 2 (between the 3 attempts)", len(*slept))
	}
	if code := codeOf(err); code != abi.ErrRateLimited {
		t.Errorf("code = %q, want %q", code, abi.ErrRateLimited)
	}

	var ce *CodedError
	if !asCoded(err, &ce) {
		t.Fatal("expected a *CodedError")
	}
	if len(ce.Result) == 0 {
		t.Errorf("expected an advisory Result payload, got none")
	}
}

// TestLLMRetryNonRetryableNotRetried: a 400 is not a transient failure and
// must not be retried.
func TestLLMRetryNonRetryableNotRetried(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer srv.Close()

	c, slept := newTestLLMClient(srv, LLMRetry{
		MaxAttempts: 5,
		BaseDelay:   time.Millisecond,
		MaxDelay:    10 * time.Millisecond,
		RetryOn:     defaultRetryOn,
	})

	_, err := c.call(context.Background(), abi.LLMCallRequest{Messages: []abi.LLMMessage{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry on 400)", calls)
	}
	if len(*slept) != 0 {
		t.Errorf("slept %d times, want 0", len(*slept))
	}
	if code := codeOf(err); code != abi.ErrInternal {
		t.Errorf("code = %q, want %q (non-retryable failures keep the generic code)", code, abi.ErrInternal)
	}
}

// TestLLMRetryOverloaded checks the 5xx -> overloaded classification once the
// budget is exhausted.
func TestLLMRetryOverloaded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"overloaded"}}`))
	}))
	defer srv.Close()

	c, _ := newTestLLMClient(srv, LLMRetry{
		MaxAttempts: 2,
		BaseDelay:   time.Millisecond,
		MaxDelay:    time.Millisecond,
		RetryOn:     defaultRetryOn,
	})

	_, err := c.call(context.Background(), abi.LLMCallRequest{})
	if code := codeOf(err); code != abi.ErrOverloaded {
		t.Errorf("code = %q, want %q", code, abi.ErrOverloaded)
	}
}

// TestLLMRetryZeroValueIsSingleAttempt: an LLMClient built as a struct literal
// (not via NewLLMClient) has a zero-value Retry, which must behave as exactly
// one attempt — the pre-existing behaviour — so nothing changes for hosts that
// don't opt in.
func TestLLMRetryZeroValueIsSingleAttempt(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := &LLMClient{BaseURL: srv.URL, HTTPClient: srv.Client()}
	_, err := c.call(context.Background(), abi.LLMCallRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

// TestNewLLMClientDefaults verifies NewLLMClient installs a sane retry policy.
func TestNewLLMClientDefaults(t *testing.T) {
	c := NewLLMClient("http://example.invalid", "key", "model")
	if c.Retry.MaxAttempts <= 1 {
		t.Errorf("MaxAttempts = %d, want > 1 by default", c.Retry.MaxAttempts)
	}
	if c.Retry.RetryOn == nil {
		t.Error("RetryOn should default to a non-nil function")
	}
	if c.sleep == nil {
		t.Error("sleep should default to a non-nil function")
	}
}

// TestParseRetryAfter covers both delta-seconds and unparsable inputs.
func TestParseRetryAfter(t *testing.T) {
	cases := map[string]int{
		"":     0,
		"0":    0,
		"-1":   0,
		"2":    2000,
		"junk": 0,
	}
	for in, want := range cases {
		if got := parseRetryAfter(in); got != want {
			t.Errorf("parseRetryAfter(%q) = %d, want %d", in, got, want)
		}
	}
}
