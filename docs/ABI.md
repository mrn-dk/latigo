# Latigo ABI — v0

This document specifies the contract between a Latigo **guest** (the harness
compiled to WebAssembly, `GOOS=wasip1 GOARCH=wasm`) and a **host**. The Go types
that encode this contract live in the [`abi`](../abi) package; this repo owns the
contract and hosts implement it.

> Rule for additions: the reference local host must implement an operation
> meaningfully, or it does not belong in the ABI.

## Transport

The guest and host exchange **length-prefixed JSON** in the guest's linear
memory through a single imported function:

```
module: "latigo_abi"
name:   "hostcall"
sig:    (reqPtr i32, reqLen i32, respPtr i32, respCap i32) -> i32
```

1. The guest serialises a [`Request`](../abi/abi.go) as JSON and places it at
   `[reqPtr, reqPtr+reqLen)`.
2. The guest hands the host a scratch buffer `[respPtr, respPtr+respCap)`.
3. The host serialises a [`Response`](../abi/abi.go), writes it into the scratch
   buffer if it fits, and returns the number of bytes.
4. If the return value `n > respCap`, the response did not fit; the guest grows
   its buffer and **retries with the identical request**. Hosts MUST treat a
   byte-identical retry as the same call and MUST NOT re-execute side effects
   (the reference host caches the last oversized response).
5. A negative return value signals a fatal transport error.

The `Request` envelope is `{ "op": "<namespace.op>", "args": <json> }`. The
`Response` envelope is `{ "result": <json> }` on success or
`{ "error": "...", "code": "..." }` on failure. Stable error codes:
`unsupported`, `denied`, `not_found`, `invalid`, `internal`, `rate_limited`,
`overloaded`, `timeout`.

The last three are *classified transient failures*: `rate_limited` (provider
signalled 429 / explicit rate limiting), `overloaded` (provider signalled 5xx
/ capacity exhaustion), and `timeout` (the request timed out or the
connection failed). They let a caller distinguish "the provider is
struggling right now" from a hard failure, without parsing free-text error
messages. A failed `Response` may also carry a `result` payload alongside
`error`/`code` — an operation-specific advisory hint (e.g. `llm.call`'s
`retry_after_ms`); most operations never set it.

## Capability negotiation

Negotiation happens **at instantiation**. The host passes the negotiated
[`Capabilities`](../abi/capabilities.go) to the guest via the WASI environment
variable `LATIGO_CAPABILITIES` (JSON), alongside `LATIGO_GOAL`, `LATIGO_MODEL`,
and `LATIGO_MAX_TURNS`. The guest reads them once and **degrades gracefully**
when an optional capability is absent (e.g. no `approval` capability means every
action is treated as pre-approved).

Required operations are always present on a conformant host. Optional
capabilities are `http`, `checkpoint`, `exec`, `approval`, `steer`, and
`fs_write`.

### Trust tiers and the single-egress rule

Capabilities fall into two trust tiers:

- **Governed** (`fs.*`, `http.fetch`, `llm.call`, `tool.*`, `msg.*`, …): every
  effect is mediated by the host, policy-gated, and recorded, so it is
  deterministic and replay-safe. This is the sandbox guarantee.
- **Ambient** (`exec.run`): runs native code carrying the host's own OS
  authority (network, filesystem, environment), which the ABI cannot govern from
  inside the guest.

The rule that keeps these coherent:

> **There is exactly one governed network egress: `http.fetch`. Any capability
> that can execute ambient code (`exec.run`) must be sandboxed by the host at
> least as strictly as `http.fetch`, or it forfeits the safety guarantee.**

Consequently the reference `exec.run` (`host.LocalExec`) is deny-by-default:
it requires an explicit `argv[0]` allowlist, never inherits or accepts
guest-supplied environment, and **network-isolates the child unless the operator
explicitly opts into unsafe networked exec** (failing closed where isolation is
unavailable). Whenever `exec` is granted, the negotiated capabilities set
`ambient: true`, which is written into the `run_start` event so the escalation
is permanently auditable.

## Operations

| Namespace | Op | Required | Purpose |
|-----------|----|----------|---------|
| `fs` | `fs.read`, `fs.write`, `fs.list`, `fs.stat`, `fs.remove`, `fs.mkdir` | yes | Host filesystem, sandboxed by the host |
| `llm` | `llm.call` | yes | OpenAI-compatible chat completion with tools |
| `tool` | `tool.list`, `tool.invoke` | yes | Runtime-agnostic tool catalog; routing is the host's business |
| `http` | `http.fetch` | optional | Governed HTTP(S) egress: the single sanctioned path to the network, allowlisted and SSRF-guarded by the host |
| `exec` | `exec.run` | optional | Native process execution (ambient; see the single-egress rule above) |
| `msg` | `msg.send`, `msg.recv` | yes | Messaging to/from the outside world |
| `approval` | `approval.await` | optional | Human-in-the-loop gating |
| `log` | `log.append` | yes | Structured logging |
| `state` | `state.checkpoint`, `state.restore` | optional | Durable state snapshots for log compaction, bounded replay, and resuming interrupted runs |
| `clock` | `clock.now` | yes | Host-injected time (recorded for determinism) |
| `rand` | `rand.bytes` | yes | Host-injected randomness (recorded for determinism) |

Request/response payloads for every op are defined in
[`abi/messages.go`](../abi/messages.go).

### Determinism

`clock.now` and `rand.bytes` are hostcalls precisely so their results are
captured in the event log. The guest never reads a real clock or entropy source
directly. Replay returns the recorded values, so a run is fully reconstructable.

`http.fetch` is likewise a **recorded side effect**: its response (status,
headers, body) is written to the log before the guest observes it and returned
verbatim on replay, so a replayed run never touches the network. Two *live* runs
may see different responses, but any single run is deterministically
reconstructable. This is also why networking is `http.fetch` and not raw
sockets — a request/response op can be recorded and replayed; a socket cannot.

### `llm.call` retry and rate-limit handling

Providers routinely rate-limit and blip (`429`, `5xx`, dropped connections).
Robustness for this lives almost entirely on the **host** side, below the
durability boundary:

- The reference `host.LLMClient` (`host/llm.go`) retries internally with
  bounded exponential backoff and jitter, honouring a `Retry-After` header
  when the provider sends one. This is configured via an `LLMRetry` struct
  (`MaxAttempts`, `BaseDelay`, `MaxDelay`, `MaxTotalWait`, `RetryOn`) on the
  client; a zero-value `LLMRetry` means exactly one attempt (today's pre-retry
  behaviour), so existing hosts that construct an `LLMClient` literal instead
  of using `NewLLMClient` are unaffected.
- Both attempts *and total wait* are bounded. A `Retry-After` longer than the
  client would otherwise wait is honoured rather than second-guessed, but
  `MaxTotalWait` (60s by default) caps the cumulative sleep: if the next wait
  would exceed the budget, the call gives up immediately and returns the
  classified error plus the `retry_after_ms` hint, letting the caller decide.
  This is what stops a hostile or misconfigured `Retry-After: 86400` from
  parking a run for a day. Backoff waits are also cancellable — a cancelled
  context cuts the wait short instead of sitting it out.
- Because every retry happens *inside* the `llm.call` handler, a single
  hostcall still produces **exactly one** recorded event. Retries are
  invisible to the event log and to replay — this is the key determinism
  property: replaying a run that hit a transient rate limit and then
  succeeded reconstructs the same single `llm.call` result, regardless of how
  many attempts happened live.
- When the host's retries are exhausted, the failure is surfaced as a
  classified error (`rate_limited`, `overloaded`, or `timeout` — see above)
  instead of the generic `internal`, so a caller can tell a transient
  provider hiccup from a hard failure. An advisory `retry_after_ms` hint may
  accompany the failure when the provider supplied one.
- The guest loop (`guest/agent.go`) has a small, overridable `OnLLMError`
  strategy point for *terminal* `llm.call` failures (i.e. after the host has
  already retried): a host can opt into returning a fallback assistant
  message (to terminate the run gracefully with a clear "stopping: provider
  unavailable" summary) or asking the guest to re-issue the call once more.
  The default aborts the run with a wrapped error, identical to the
  pre-existing behaviour — nothing changes unless a host opts in.
- Guest-side wall-clock backoff is deliberately **not** supported: sleeping
  driven by the guest would be non-deterministic and break replay. Backoff
  lives entirely in the host handler; the guest only ever sees the outcome.

### In-loop steering and approval gating

`approval.await` and `msg.recv` are ordinary governed hostcalls (see above),
but the reference guest agent loop (`guest/agent.go`) also gives them a
default *policy*, so a host that wires them gets human/host-in-the-loop
control for free, without the host having to understand the agent's internal
turn structure.

**Approval gating.** Before invoking a tool, the agent consults an overridable
`ApprovalGate` strategy point. This is consulted only when the host grants the
`approval` capability; the default gate requires approval for the
ambient/dangerous surface — `exec.run`-backed tools, `http_fetch`, VFS writes
that escape `/work`, and fs-removal tools — everything else (bash inside the
sandboxed VFS, reads, in-`/work` writes, skills, scripts) runs unattended. A
denial is **not** fatal: it is fed back to the model as the tool's result
(`"denied by host: <reason>"`) and the run continues, giving the model a
chance to try something else. Because `Client.ApprovalAwait` already degrades
to "approved" when the `approval` capability is absent, hosts that never call
`(*host.Host).Approval` are completely unaffected — no extra hostcall, no
change in behaviour.

**Steering.** At the top of each turn the agent consults an overridable
`Steer` strategy point, which by default performs a non-blocking
`msg.recv("steer", false)`. A pending message is appended as a `user` message
so the model sees it on its next `llm.call`; the sentinel `"/stop"` instead
ends the run gracefully before that turn's `llm.call`. This is gated behind a
new `steer` capability (`abi.Capabilities.Steer`) rather than being
unconditional: polling `msg.recv` every turn is harmless (it's just another
recorded hostcall that replays verbatim) but it does add one hostcall per turn
to the event log, so hosts that never wire a real steering source should see
*zero* change to their hostcall traffic or log shape. `(*host.Host).Messaging`
sets this capability automatically when given a non-nil `Messenger.In`. Hosts
that do wire steering but want fewer hostcalls can additionally raise the
guest's `Agent.SteerEvery` to poll every Nth turn instead of every turn.

Both mechanisms need no new replay machinery: `approval.await` and `msg.recv`
are recorded write-ahead like any other hostcall, so a denial or an injected
steering message reconstructs exactly on replay — the human/host input becomes
part of the durable record. One consequence worth calling out for host
authors: a replay host **must** advertise the same `Capabilities` the original
run negotiated (in particular `approval` and `steer`), or the guest will
skip/add hostcalls relative to what the log expects next and replay will
diverge with a "replay divergence" error. The reference CLI's `-replay` path
(`cmd/latigo-local/main.go`) does this by propagating the full `Capabilities`
struct recorded in the `run_start` event.

See `cmd/latigo-local/main.go`'s `-approve` (interactive y/n prompts) and
`-steer` (stdin lines injected as steering messages, `/stop` to end the run)
flags for a working reference wiring.

## Conformance

The [`conformance`](../conformance) package verifies a host against this
contract. A host adapts to the suite's `Transport` interface (the reference host
provides `(*host.Host).AsTransport`). See `host/conformance_test.go`.
