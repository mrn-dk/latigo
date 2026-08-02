# Latigo — the agent harness

Latigo is **just the harness**. It is a WASM program run under Wasmer/WASIX that
does three things:

1. **Runs the agent in a loop** — a model with tools until the task is done,
   with usage limits, argument validation, and a typed `finish`.
2. **Compacts the conversation** — keeps its own context window bounded via
   overridable strategy points.
3. **Emits the right events** — an append-only, fsync'd write-ahead log that is
   the sole source of truth for resume (Latigo is stateless between turns).

## What is out of scope here

Everything that is not the harness:

- **The host system** — a container, managed and kept **warm** by Stonewall,
  that provides the image, the tools, and the command allow-list. When Latigo
  needs to run a shell command, it calls the host; the host decides what is
  available. Container boot-up cost, density, quotas, and the warm pool are
  Stonewall's concerns, not Latigo's.
- **The inference endpoint** — Latigo speaks the OpenAI-compatible chat
  completions dialect to *any* endpoint that implements it; it is not coupled
  to a specific gateway.
- **Checkpoints, forking, and the fleet** — all Stonewall.

## The harness's own surfaces

| Surface | What it does | Spec |
|---|---|---|
| agent loop | turn structure, tool dispatch, usage limits, `finish` | [`agent-loop`](../openspec/specs/agent-loop/spec.md) |
| compaction | transcript compaction strategy points | [`compaction`](../openspec/specs/compaction/spec.md) |
| event log | append-only JSONL, write-ahead, transcript rebuild/resume | [`event-log`](../openspec/specs/event-log/spec.md) |
| shell | allow-listed shell; parse, walk the AST, verify, never `bash -c`; calls the host for binaries | [`shell`](../openspec/specs/shell/spec.md) |
| LLM client | OpenAI-compatible HTTP client (streaming is a proposed change) | [`llm-client`](../openspec/specs/llm-client/spec.md) |

The authoritative specification of Latigo is in [`openspec/specs/`](../openspec/specs/).
See [`docs/EVENTS.md`](EVENTS.md) for the event-log record shapes.