# Latigo — the agent harness

Latigo is **just the harness**: a WASM program run under Wasmer/WASIX that does
three things — runs the agent in a loop, compacts the conversation, and emits the
right events. Everything else (the host container, the warm pool, checkpoints,
forking, the fleet) is owned by Stonewall and is out of scope here.

## What it does

- **Agent loop** with turn structure, argument validation before dispatch,
  in-loop usage limits (`max_turns`, `max_total_tokens`, `max_tool_invocations`,
  `max_wall_clock`), and a typed `finish` tool.
- **Allow-listed shell** — the single surface the model uses to reach the host's
  tools. The command line is parsed with [mvdan/sh] and the AST is walked; every
  command node is verified against the allow-list before executing. Command
  substitution and `eval` are hard rejects — never a fallback to a raw shell.
  Pipes and redirection remain. The host (a Stonewall-managed container) owns
  the image and the allow-list; Latigo respects what the host grants.
- **OpenAI-compatible LLM client** — Latigo speaks the chat completions dialect
  to *any* endpoint that implements it; it is not coupled to a specific gateway.
  Streaming is opt-in (`LATIGO_STREAM=1`): text deltas are forwarded to an
  optional sink as they arrive, while the assembled message remains the unit of
  transcript and logging.
- **Durable event log** — append-only JSONL, `fsync`'d before any result is
  acted upon (write-ahead), carrying the conversation plus a thin operational
  layer. Latigo is stateless between turns; resume means load the transcript
  from the log, mount the workspace, continue. See [docs/EVENTS.md](docs/EVENTS.md).

## Specification

The authoritative specification is in [openspec/specs/](openspec/specs/):

- [`agent-loop`](openspec/specs/agent-loop/spec.md) — the loop, tools, validation, limits
- [`compaction`](openspec/specs/compaction/spec.md) — transcript compaction
- [`event-log`](openspec/specs/event-log/spec.md) — the durable log and resume
- [`shell`](openspec/specs/shell/spec.md) — the allow-listed shell
- [`llm-client`](openspec/specs/llm-client/spec.md) — the OpenAI-compatible client

Proposed changes live in [openspec/changes/](openspec/changes/) — notably
[add-streaming](openspec/changes/add-streaming/) for streaming chat completions.

## Quick start

```sh
make build            # native binary (the tested path)
make test             # real shell, mock endpoint, event log

# Run against any OpenAI-compatible endpoint and a workspace, with a command allow-list:
LATIGO_LLM_BASE_URL=http://localhost:8080/v1 \
LATIGO_API_KEY=sk-... \
LATIGO_WORKSPACE=./workspace \
LATIGO_ALLOWLIST=./allowlist.example.json \
  ./latigo "list the files under the workspace and report what you find"

# Or a comma-separated allow-list:
LATIGO_ALLOW=ls,cat,rg,grep ./latigo "find all TODO comments"

# Resume a run that was interrupted mid-task:
LATIGO_RESUME=1 ./latigo
```

### Building for WASM

```sh
make wasm            # GOOS=wasip1 GOARCH=wasm → latigo.wasm
```

The native `make build` path is the tested one; on a single host the OS plays
the runtime's role and the same code runs unchanged. WASIX runtime behaviour
under Wasmer (subprocess spawn, networking, quotas) is the host system's
concern, not Latigo's.

## Configuration

Latigo reads its run configuration from the environment (a WASIX program is
launched with env + args by the orchestrator):

| Variable | Default | Meaning |
|---|---|---|
| `LATIGO_GOAL` / arg[1] | — | the task |
| `LATIGO_MODEL` | `gpt-4o-mini` | model name passed to the endpoint |
| `LATIGO_LLM_BASE_URL` | `http://localhost:8080/v1` | OpenAI-compatible base URL |
| `LATIGO_API_KEY` | — | bearer token (secrets never enter the sandbox in the full architecture) |
| `LATIGO_WORKSPACE` | `./workspace` | mounted workspace directory |
| `LATIGO_EVENT_LOG` | `./latigo.events.jsonl` | append-only event log path |
| `LATIGO_ALLOWLIST` | — | path to an allow-list JSON (see `allowlist.example.json`) |
| `LATIGO_ALLOW` | — | comma-separated command allow-list (alternative) |
| `LATIGO_OUTPUT_SCHEMA` | — | optional JSON Schema for the `finish` tool's output |
| `LATIGO_RESUME` | `0` | `1`/`true` continues the last run from its log |
| `LATIGO_STREAM` | `0` | `1`/`true` streams chat completions (text deltas to a sink; the assembled message is still the record) |
| `LATIGO_COMPACTION` | `window` | `window` (deterministic) or `llm` (model-driven summary) |
| `LATIGO_MAX_TURNS` | `16` | usage limit |
| `LATIGO_MAX_TOTAL_TOKENS` | `0` = unlimited | usage limit |
| `LATIGO_MAX_TOOL_INVOCATIONS` | `0` = unlimited | usage limit |
| `LATIGO_MAX_WALL_CLOCK_S` | `1800` | usage limit |
| `LATIGO_SHELL_EXEC_TIMEOUT_S` | `60` | per-leaf-command timeout |

## Requirements

Go 1.25+ (the `mvdan.cc/sh/v3` dependency requires it).

[mvdan/sh]: https://github.com/mvdan/sh