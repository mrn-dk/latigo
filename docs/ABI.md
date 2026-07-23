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
`unsupported`, `denied`, `not_found`, `invalid`, `internal`.

## Capability negotiation

Negotiation happens **at instantiation**. The host passes the negotiated
[`Capabilities`](../abi/capabilities.go) to the guest via the WASI environment
variable `LATIGO_CAPABILITIES` (JSON), alongside `LATIGO_GOAL`, `LATIGO_MODEL`,
and `LATIGO_MAX_TURNS`. The guest reads them once and **degrades gracefully**
when an optional capability is absent (e.g. no `approval` capability means every
action is treated as pre-approved).

Required operations are always present on a conformant host. Optional
capabilities are `http`, `checkpoint`, `exec`, `approval`, and `fs_write`.

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

## Conformance

The [`conformance`](../conformance) package verifies a host against this
contract. A host adapts to the suite's `Transport` interface (the reference host
provides `(*host.Host).AsTransport`). See `host/conformance_test.go`.
