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
  from the log, mount the workspace, continue.

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
| `LATIGO_SYSTEM_PROMPT` | — | inline system-prompt override (replaces the default); empty = unset |
| `LATIGO_SYSTEM_PROMPT_FILE` | — | path to a file whose contents override the default system prompt |
| `LATIGO_APPEND_SYSTEM_PROMPT` | — | inline text appended after the system prompt (default or override) |
| `LATIGO_APPEND_SYSTEM_PROMPT_FILE` | — | path to a file whose contents are appended after the system prompt |
| `LATIGO_COMPACTION` | `window` | `window` (deterministic) or `llm` (model-driven summary) |
| `LATIGO_MAX_TURNS` | `16` | usage limit |
| `LATIGO_MAX_TOTAL_TOKENS` | `0` = unlimited | usage limit |
| `LATIGO_MAX_TOOL_INVOCATIONS` | `0` = unlimited | usage limit |
| `LATIGO_MAX_WALL_CLOCK_S` | `1800` | usage limit |
| `LATIGO_SHELL_EXEC_TIMEOUT_S` | `60` | per-leaf-command timeout |

## System prompt

By default Latigo uses a built-in system prompt that identifies the harness,
lists its three tools (`shell`, `tool_list`, `finish`), and describes the
`tool_list` → `<name> --help` discovery convention. You can change it at
launch with two knobs, each settable inline or from a file:

- **Override** (`LATIGO_SYSTEM_PROMPT` / `LATIGO_SYSTEM_PROMPT_FILE`) — a
  complete prompt that *replaces* the default. Inline takes precedence over
  the file; an empty inline value is treated as unset.
- **Append** (`LATIGO_APPEND_SYSTEM_PROMPT` /
  `LATIGO_APPEND_SYSTEM_PROMPT_FILE`) — text concatenated after the prompt
  (default or override), separated by a blank line, for project-specific
  additions without rewriting the whole prompt.

A custom prompt is **opaque plain text**: Latigo does not interpret it (no
templating, no variable substitution) and does not merge the built-in tool
descriptions into it. An override that omits `finish` won't terminate on
`finish`, so an override is responsible for telling the model how to use the
harness's tools. A configured `*_FILE` path that is missing or unreadable fails
at launch with an error naming the path, rather than silently using the
default. The chosen source (`default` / `override` / `default+append` /
`override+append`) is recorded in the `run_start` event config.

## Event log

The event log is append-only JSONL, `fsync`'d before any result is acted upon
(write-ahead). It carries the conversation **plus** a thin operational layer.
There is no replay engine: Latigo is stateless between turns, and resume means
*load the transcript, mount the workspace, continue* — reading the recorded
conversation back out of the log, not re-executing it.

**Record shape:**

```json
{"seq":1,"kind":"run_start","time":"...","harness":"latigo/0.2.0","payload":{...}}
```

| field    | meaning                                                       |
|----------|---------------------------------------------------------------|
| `seq`    | strictly increasing sequence number                           |
| `kind`   | one of the kinds below                                        |
| `time`   | RFC3339 UTC                                                   |
| `harness`| harness version stamp                                         |
| `payload`| kind-specific JSON object (see below)                         |

**Kinds:**

- `run_start` — `{"run_id":"run-...","goal":"...","model":"...","llm_base_url":"...","grants":{"workspace":"/workspace","net":["llm.example"],"commands":["rg","pytest"]},"config":{...limits...}}`
- `turn` — a turn boundary, recorded at the top of each turn: `{"turn":1}`
- `llm` — one chat-completion result; the assistant message is recorded verbatim so the transcript can be rebuilt by reading the log: `{"turn":1,"model":"...","latency_ms":420,"input_tokens":120,"output_tokens":30,"total_tokens":150,"finish_reason":"tool_calls","message":{"role":"assistant","content":"...","tool_calls":[...]}}`
- `tool` — tool/exec intent and result, with an idempotency key. Intent is recorded **before** dispatch; the result shares the key + call id. `status` ∈ `intent` | `ok` | `error` | `invalid` | `denied`.
- `finish` — the validated final output: `{"output":"...","valid":true,"errors":[]}`
- `turn_end` — end-of-turn marker. `checkpoint_id` is assigned by the orchestrator; it is empty on the single-host path, where the log and workspace *are* the recoverable state. `egress` lists the destinations reached this turn.
- `run_end` — `{"reason":"finished","error":""}` where `reason` ∈ `finished` | `answered` | `max_turns` | `max_total_tokens` | `max_tool_invocations` | `max_wall_clock` | `llm_error` | `dispatch_error`.
- `log` — operational notes (validation failures, etc.) that are not part of the conversation.

**Turn numbering:** the first turn of an agent with no recorded history is
**turn 1**, and turn numbers continue across resume — a run resuming a log
whose highest turn is 6 starts at turn 7, so no turn number appears twice in a
log. The number is derived from the log on resume, the same way the sequence
number is. `max_turns` is a separate quantity: it bounds the turns taken by the
*current* run, so a resumed run gets its full budget rather than one reduced by
prior turns. Logs written before this change are 0-based; a resumed run
continues forward from such a log's highest turn, so it stays ordered.

**Transcript rebuild (resume):** `LoadTranscript` reads the log and rebuilds the conversation — `run_start` → the goal and model; each `llm` event → one assistant message (with any `tool_calls`); each terminal `tool` event (`ok`/`error`/`invalid`/`denied`) → one tool-role message, deduplicated by `call_id`. `intent`-only tool events, `turn`, `turn_end`, `run_end`, and `log` events are skipped. The system prompt is supplied by the caller; everything else comes from the log.

**Streaming and the log:** when chat completions are streamed (`LATIGO_STREAM=1`), the SSE text deltas are an **ephemeral output path** — forwarded to an optional sink for live display and **never written to the event log**. The durable record of a streamed turn is exactly one `llm` event carrying the fully assembled assistant message and usage, identical to a non-streamed turn. Tool calls are dispatched only after the stream is fully assembled, so tool-call arguments are complete before dispatch.

## Requirements

Go 1.25+ (the `mvdan.cc/sh/v3` dependency requires it).

[mvdan/sh]: https://github.com/mvdan/sh
