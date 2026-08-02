# Latigo event log

The event log is append-only JSONL, `fsync`'d before any result is acted upon
(write-ahead). It carries the conversation **plus** a thin operational layer.
There is no replay engine: Latigo is stateless between turns, and resume means
*load the transcript, mount the workspace, continue* — reading the recorded
conversation back out of the log, not re-executing it.

## Record shape

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

## Kinds

### `run_start`
```json
{"run_id":"run-...","goal":"...","model":"...","llm_base_url":"...",
 "grants":{"workspace":"/workspace","net":["llm.example"],"commands":["rg","pytest"]},
 "config":{...limits...}}
```

### `turn` — a turn boundary, recorded at the top of each turn.
```json
{"turn":0}
```

### `llm` — one chat-completion result; the assistant message is recorded
verbatim so the transcript can be rebuilt by reading the log.
```json
{"turn":0,"model":"...","latency_ms":420,"input_tokens":120,"output_tokens":30,
 "total_tokens":150,"finish_reason":"tool_calls",
 "message":{"role":"assistant","content":"...","tool_calls":[...]}}
```

### `tool` — tool/exec intent and result, with an idempotency key.
Intent is recorded **before** dispatch; the result shares the key + call id.
```json
{"call_id":"call-1","idempotency_key":"a1b2c3d4","name":"shell","args":{...},
 "status":"intent"}
{"call_id":"call-1","idempotency_key":"a1b2c3d4","name":"shell",
 "status":"ok","exit_code":0,"stdout":"...","stderr":"...","latency_ms":3}
```
`status` ∈ `intent` | `ok` | `error` | `invalid` | `denied`.

### `finish` — the validated final output (from the `finish` tool, or a plain
assistant answer).
```json
{"output":"...","valid":true,"errors":[]}
```

### `turn_end` — end-of-turn marker. `checkpoint_id` is assigned by the
orchestrator (Stonewall); it is empty on the single-host path, where the log
and workspace *are* the recoverable state. `egress` lists the destinations
reached this turn.
```json
{"turn":0,"checkpoint_id":"","egress":["llm.example"]}
```

### `run_end`
```json
{"reason":"finished","error":""}
```
`reason` ∈ `finished` | `answered` | `max_turns` | `max_total_tokens` |
`max_tool_invocations` | `max_wall_clock` | `llm_error` | `dispatch_error`.

### `log` — operational notes (validation failures, etc.) that are not part of
the conversation.
```json
{"level":"warn","message":"tool args validation failed","fields":{...}}
```

## Transcript rebuild (resume)

`LoadTranscript` reads the log and rebuilds the conversation:
- `run_start` → the goal and model;
- each `llm` event → one assistant message (with any `tool_calls`);
- each terminal `tool` event (`ok`/`error`/`invalid`/`denied`) → one
  tool-role message, deduplicated by `call_id`.

`intent`-only tool events, `turn`, `turn_end`, `run_end`, and `log` events are
skipped. The system prompt is supplied by the caller; everything else comes
from the log.
