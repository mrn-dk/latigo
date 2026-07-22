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
capabilities are `exec`, `approval`, and `fs_write`.

## Operations

| Namespace | Op | Required | Purpose |
|-----------|----|----------|---------|
| `fs` | `fs.read`, `fs.write`, `fs.list`, `fs.stat`, `fs.remove`, `fs.mkdir` | yes | Host filesystem, sandboxed by the host |
| `llm` | `llm.call` | yes | OpenAI-compatible chat completion with tools |
| `tool` | `tool.list`, `tool.invoke` | yes | Runtime-agnostic tool catalog; routing is the host's business |
| `exec` | `exec.run` | optional | Native process execution |
| `msg` | `msg.send`, `msg.recv` | yes | Messaging to/from the outside world |
| `approval` | `approval.await` | optional | Human-in-the-loop gating |
| `log` | `log.append` | yes | Structured logging |
| `clock` | `clock.now` | yes | Host-injected time (recorded for determinism) |
| `rand` | `rand.bytes` | yes | Host-injected randomness (recorded for determinism) |

Request/response payloads for every op are defined in
[`abi/messages.go`](../abi/messages.go).

### Determinism

`clock.now` and `rand.bytes` are hostcalls precisely so their results are
captured in the event log. The guest never reads a real clock or entropy source
directly. Replay returns the recorded values, so a run is fully reconstructable.

## Conformance

The [`conformance`](../conformance) package verifies a host against this
contract. A host adapts to the suite's `Transport` interface (the reference host
provides `(*host.Host).AsTransport`). See `host/conformance_test.go`.
