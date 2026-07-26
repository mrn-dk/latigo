# Spec 03 — LLM robustness: retry, backoff, and rate-limit handling

- **Status:** Proposed
- **Capability:** none new (host-side behaviour + optional guest policy)
- **Affects:** `host/llm.go`, `host/host.go` (error surfacing), `abi/messages.go` (structured LLM error), `guest/agent.go`, `docs/ABI.md`
- **Sourcing note:** other-harness behaviour is from general knowledge; treat as directional.

## Problem

`guest/agent.go` treats any `llm.call` failure as fatal:

```go
resp, err := a.client.LLMCall(...)
if err != nil {
    return "", fmt.Errorf("llm.call: %w", err)
}
```

A transient `429 Too Many Requests`, a `503`, or a dropped connection therefore
kills the whole run. Providers routinely rate-limit and blip; a production
harness must survive that. Latigo currently has **no retry, no backoff, and no
rate-limit awareness** anywhere (`host/llm.go` is a single `http.Client.Do`).

## Prior art

- **Claude Code:** retries transient errors with exponential backoff, honours
  `Retry-After` on 429s, and surfaces a clear "overloaded / rate-limited,
  retrying" state rather than aborting. Long backoffs are shown to the user.
- **Pi:** provider adapters handle transient failures and auth/refresh; the
  agent loop is expected to be resilient across provider hiccups (subscription
  token auto-refresh in `providers.md`).
- **General practice:** retry only *idempotent-safe* transient classes
  (429/500/502/503/504, connection reset, timeout); respect `Retry-After`; cap
  attempts and total wait; jitter the backoff.

## Design

Two layers. Most robustness belongs in the **host** (it owns the network); the
guest only needs a small, deterministic policy for when the host ultimately
gives up.

### Host retry (primary)

Make `host.LLMClient` retry internally with bounded exponential backoff + jitter,
honouring `Retry-After`. Configuration on the client:

```go
type LLMRetry struct {
    MaxAttempts int           // e.g. 5
    BaseDelay   time.Duration // e.g. 500ms
    MaxDelay    time.Duration // e.g. 30s
    RetryOn     func(status int, err error) bool // default: 429/5xx/timeout/reset
}
```

Because retries happen *inside* the handler, a single `llm.call` hostcall still
produces exactly one recorded result — retries are invisible to the log and to
determinism. This is the key point: **retry lives below the durability boundary.**

### Structured, classified errors (ABI)

When the host does give up, it should return a *classified* error so the guest
(or host policy) can decide what to do. Extend the error surface:

```go
// abi/abi.go error codes
ErrRateLimited = "rate_limited"
ErrOverloaded  = "overloaded"
ErrTimeout     = "timeout"
```

`llm.call` failures map to these instead of a generic `internal`. Optionally add
a hint in the response:

```go
type LLMCallResponse struct {
    ...
    RetryAfterMS int `json:"retry_after_ms,omitempty"` // advisory, when known
}
```

### Guest policy (secondary)

The guest loop gains a strategy point for terminal LLM failures so a host can
choose to degrade rather than crash:

```go
// OnLLMError decides how to handle a call that failed after host retries.
// Return retry=true to re-issue (rare; host already retried), or a fallback
// assistant message to inject, or ("",false) to abort as today.
OnLLMError func(a *Agent, err error, turn int) (fallback *abi.LLMMessage, retry bool)
```

Default keeps today's behaviour (abort) so nothing changes unless a host opts in.
A useful non-default: on `rate_limited`, sleep via a **recorded** `clock.now`
gap is *not* possible deterministically, so guest-side waiting is discouraged —
prefer host-side retry. The guest policy is mainly for graceful termination
("stopping: provider unavailable") with a clean summary.

## Determinism & replay

- Host retries are **not** recorded individually; only the final result/error is
  logged, so replay is deterministic regardless of how many attempts happened
  live.
- Wall-clock backoff must **never** be driven by the guest (that would be
  non-deterministic); it lives entirely in the host handler. The guest only sees
  the outcome.
- A recorded terminal error replays as the same error, so a run that ended due to
  rate limiting reconstructs identically.

## Testing

- `host/llm.go`: a stub server returning `429` with `Retry-After`, then `200`,
  yields one successful result after the expected backoff (use an injectable
  clock/sleeper to keep the test fast).
- Retry budget exhausted → classified `rate_limited` error surfaced once.
- Non-retryable (`400`) is not retried.
- Guest `OnLLMError` default aborts; an opt-in fallback injects a message and
  terminates cleanly.
- Replay of a rate-limited-then-succeeded run shows a single recorded `llm.call`.

## Non-goals / open questions

- Provider-side load balancing / failover across models (a host orchestration
  concern; could be a future `llm.call` routing policy).
- Guest-driven backoff sleeping (rejected: breaks determinism).
- Circuit breaking across runs (host process concern).
