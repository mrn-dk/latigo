# Latigo — Durable Agent Harness

Latigo is an independent, embeddable agent harness written in Go and compiled to
WebAssembly (`wasip1`). It is **sandboxed by construction** (no direct network or
disk), **deterministic**, and **reconstructable from its event log** on any
conformant host.

The guest runs an agent loop with a virtual filesystem, a bash-like shell, on-
demand markdown skills, and sandboxed Starlark script tools. It reaches the
outside world only through a small, versioned ABI of host calls. Hosts implement
that ABI; this repo owns the contract.

## Repository layout

| Path | Artifact |
|------|----------|
| [`abi/`](abi) | ABI v0: the versioned host/guest contract as Go types |
| [`events/`](events) | Durable event schema and checkpoint format |
| [`guest/`](guest) | The in-guest harness: agent loop, VFS, virtual bash, skills, Starlark tools, tool registry |
| [`cmd/latigo-guest/`](cmd/latigo-guest) | `main` compiled to WASM |
| [`host/`](host) | Reference host library: dispatch, durability, replay, fs/llm/tools/exec/... handlers, wazero bridge |
| [`cmd/latigo-local/`](cmd/latigo-local) | Reference local host CLI (local FS, OpenAI-compatible/Mortise LLM, JSONL log) |
| [`conformance/`](conformance) | Host conformance suite |
| [`docs/`](docs) | [ABI spec](docs/ABI.md), [event schema](docs/EVENTS.md) |

## Quick start

```sh
# Build the guest to WebAssembly and the reference host.
make guest host

# Run with the built-in deterministic mock LLM (no network needed).
./latigo-local -wasm latigo.wasm "list the files under /work"

# Reconstruct the run from its durable event log — no re-execution.
./latigo-local -wasm latigo.wasm -replay

# Use a real OpenAI-compatible endpoint (or Mortise):
OPENAI_BASE_URL=https://api.openai.com/v1 OPENAI_API_KEY=sk-... \
  ./latigo-local -wasm latigo.wasm -model gpt-4o-mini "summarise README"
```

Run everything (including a real wasm run + replay integration test):

```sh
make test
```

## Design in one screen

- **Transport.** One imported function, `latigo_abi.hostcall`, carries length-
  prefixed JSON in the guest's linear memory. See [docs/ABI.md](docs/ABI.md).
- **Namespaces.** `fs.*`, `llm.call`, `tool.list`/`tool.invoke`, `exec.run`
  (optional), `msg.send`/`msg.recv`, `approval.await`, `log.append`, and host-
  injected `clock.now`/`rand.bytes`.
- **Capability negotiation** happens at instantiation; the guest degrades
  gracefully when an optional capability is absent.
- **Durability.** Write-ahead: every hostcall result is appended, flushed, and
  fsynced before the guest observes it. Events carry harness-version stamps.
  Periodic checkpoints enable compaction and bounded replay. **Replay is state
  reconstruction from recorded results — never re-execution.** See
  [docs/EVENTS.md](docs/EVENTS.md).
- **In-guest.** Agent loop with configurable strategy points (compaction,
  termination); virtual bash + VFS (`mvdan/sh` + `afero`); skills as on-demand
  markdown; Starlark script tools with step/output budgets; tool catalog
  received from the host.

## Non-goals

No direct network or disk access, no nested WASM, and no orchestration concepts
(leases, tenants) in the ABI.

## Requirements

Go 1.25+ (the wazero host runtime requires it; the toolchain is auto-selected via
`go.mod`).
