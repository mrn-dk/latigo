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
capabilities are `http`, `checkpoint`, `exec`, `approval`, `steer`, `fs_write`,
and `multimodal`.

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

### Multimodal content (images)

Messages and tool results are not limited to plain text. [`abi.LLMMessage`](../abi/messages.go)
carries `Content string` (the text shorthand, always populated for text-only
messages) alongside an optional `Parts []ContentPart`:

```go
type ContentPart struct {
    Type  string     `json:"type"`            // "text" | "image"
    Text  string     `json:"text,omitempty"`
    Image *ImageData `json:"image,omitempty"`
}

type ImageData struct {
    MediaType string `json:"media_type"` // e.g. "image/png"
    Data      []byte `json:"data,omitempty"`
    URL       string `json:"url,omitempty"`
}
```

`Parts` is `omitempty`: a text-only message serialises identically to before
this capability existed, so old event logs and hosts that never construct a
`Parts` slice are completely unaffected. When `Parts` is non-empty it is
authoritative — it is what actually gets sent to the model; `Content` is not
also re-emitted alongside it. `abi.ToolInvokeResponse` gains the same `Parts`
field so a tool can attach structured content (e.g. an image) to its result.

**Capability gate.** A host advertises `Capabilities.Multimodal` only when the
configured model actually accepts images; `Negotiate` ANDs it like the other
optional capabilities. The guest **must never** emit an image part to a
non-multimodal host: `guest.Registry.Invoke` and the agent's initial-turn
message construction both run every `Parts` slice through a shared
degradation policy that drops each image part and substitutes a text
placeholder (`"[image omitted: host is not multimodal]"`) whenever
`Capabilities.Multimodal` is false. A tool author (or the host) never needs to
check the capability themselves — attach whatever images you want; the guest
degrades unconditionally.

**Guest surface.** `guest.Tool.Invoke` keeps its existing
`func(ctx, args) (string, error)` signature untouched (every built-in still
implements only this). A tool that wants to return structured content sets
the new, optional `Tool.InvokeRich func(ctx, args) (RichResult, error)` field
instead, where `RichResult{Text string; Parts []abi.ContentPart}`;
`Registry.Invoke` prefers `InvokeRich` when set. The built-in `attach_image`
tool (`guest/builtins.go`) uses this to read a file from the VFS and attach it
as an image part to its tool result. The agent's initial user turn can also
carry host-attached images (`guest.Config.Images`, populated from the
`LATIGO_GOAL_IMAGES` environment variable — the same instantiation-time
mechanism `LATIGO_CAPABILITIES` uses), which `latigo-local`'s repeatable
`-image path.png` flag populates.

**Host translation.** `host/llm.go` is the only place that knows the wire
dialect. The reference `LLMClient` speaks OpenAI's `/chat/completions`
format, where a message's `content` is either a plain string or an array of
typed parts (`{"type":"text",...}` / `{"type":"image_url","image_url":{"url":...}}`,
the URL being either an `https://` URL or a `data:<media-type>;base64,...`
URI for inline bytes). This mapping is `oaiContentParts`
(`[]abi.ContentPart -> []oaiContentPart`), a small, separately unit-tested
pure function — deliberately factored so that a second wire dialect
(Anthropic's `messages` API, which also represents content as an array of
typed parts, just with `{"type":"image","source":{...}}` instead of
`image_url`) is a sibling function and a table test away. No Anthropic client
exists in this repo yet; only the OpenAI dialect is implemented.

**Log size.** Because the agent loop resends the entire transcript on every
turn and the host's write-ahead durability records each `llm.call` request
verbatim *before* any host-side processing runs, an oversized image attached
early in a run gets re-logged in full on every subsequent turn — there is no
way to shrink it back out of the log afterwards without breaking replay.
`host.CapImage` (`host/image.go`) is the mitigation: given an `ImageData` and
a byte budget, it downscales (nearest-neighbour resize, re-encoded as JPEG at
shrinking quality) until the image fits, or returns an error ("reject") if it
cannot be decoded or brought under budget within a bounded number of
attempts. It uses only `image`/`image/png`/`image/jpeg`/`image/gif` from the
standard library — no third-party image dependency. `latigo-local` applies it
(`host.DefaultMaxImageBytes`, 2 MiB) to every `-image` attachment before it
ever becomes part of a guest message. This does not (yet) address the
narrower case the spec calls out of `state.checkpoint` snapshots inlining the
full message history, images included, every few turns — a complete fix
would have the guest store images as VFS-path references in checkpoint blobs
rather than inline bytes; that is not implemented here.

**Determinism & replay.** Images are ordinary bytes inside a message; once
part of `a.messages` they flow through the existing `llm.call` write-ahead
recording like any other request content, so replay reconstructs them
verbatim with no special-casing — see `cmd/latigo-local/image_test.go` for an
end-to-end check that a `-image` attachment lands in the recorded `llm.call`
request bytes.

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
sandboxed VFS, reads, in-`/work` writes, skills, scripts, plan) runs
unattended. `edit_file` and `multi_edit` (see "Structured editing and
planning tools" below) are gated exactly like `write_file`: same check
(escapes `/work`?) against the same top-level `path` field, since an edit is
just a more surgical VFS write. A denial is **not** fatal: it is fed back to the model as the tool's result
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

### Structured editing and planning tools (in-guest, no hostcalls)

Two built-in tool families (`guest/edit.go`, `guest/plan.go`, registered from
`guest/builtins.go`'s neighbours `Agent.registerEditTools`/
`Agent.registerPlanTools`) add no new capability and no new hostcall: both are
pure computation over the VFS and agent state, so they replay exactly like any
other deterministic guest logic — there is nothing new for the event log to
record.

**`edit_file`** does an exact-match string replace on a VFS file: `{path, old,
new, all}`. `old` must match exactly once — zero matches and ambiguous
(multiple) matches are both errors, never a guess — unless `all: true`, which
replaces every occurrence. Empty `old` on a file that doesn't exist yet
creates it with `new` as its content; empty `new` deletes the matched region.
The tool result is a unified-diff snippet (`--- a/path` / `+++ b/path` /
`@@ ... @@` hunks with 3 lines of context) so a host can render or approve the
change, composing with the multimodal (spec 01) and approval (spec 02)
machinery already in place — `edit_file` uses plain `Tool.Invoke` (text
result), not `InvokeRich`, since a diff is exactly the kind of thing that
reads fine as text.

**`multi_edit`** applies an ordered list of `{old, new}` edits to one file
atomically: each edit is checked against the result of the previous one, and
if any edit fails (zero or ambiguous matches) *none* of them are written — the
implementation builds the result in a local string and only calls
`VFS.WriteFile` once every edit in the batch has applied cleanly, so a failure
partway through leaves the file completely untouched rather than needing an
explicit rollback step.

Both are implemented with a from-scratch line differ (LCS-based hunk
computation) in `guest/edit.go` — the module carries no third-party diff
library; see `go.mod`, still four direct dependencies.

**`plan`** (`guest/plan.go`) is a durable todo list: `{"op": "set"|"update"|
"get", "items": [{id, text, status}]}` with `status` one of `pending`,
`in_progress`, `done`. `set` replaces the whole list; `update` edits existing
items by id (validated as an all-or-nothing batch — an unknown id or invalid
status aborts the whole update, leaving the plan untouched); `get` returns it
unchanged. The plan lives on `Agent.plan` (`[]guest.PlanItem`) and is folded
into `agentSnapshot` (`guest/agent.go`) as an `omitempty` `plan` field, so it
survives `checkpointState`/`restore` — and, critically, an *older* checkpoint
blob with no `"plan"` key at all still decodes cleanly (unmarshals to a nil
slice, identical to a run that never touched the tool).

The plan is **pinned into context every turn, not just carried in the
transcript**: `Agent.messagesForLLM` (consulted by the turn loop instead of
sending `a.messages` directly) appends a fresh `system`-role reminder
rendering the current plan checklist to the outgoing `llm.call` request, built
from `a.plan` on every call. That message is never written into `a.messages`
itself, so `guest/compaction.go`'s `defaultCompact` — which elides the *middle*
of `a.messages` under a summary — has nothing belonging to the plan to elide;
the plan survives long, repeatedly-compacted runs by construction rather than
by special-casing the compactor.

## Conformance

The [`conformance`](../conformance) package verifies a host against this
contract. A host adapts to the suite's `Transport` interface (the reference host
provides `(*host.Host).AsTransport`). See `host/conformance_test.go`.
