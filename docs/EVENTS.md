# Latigo Event Schema & Checkpoints — v0

The event log is the source of truth for a Latigo run. The Go types live in the
[`events`](../events) package. The reference host writes the log as **JSONL**
(one `Event` per line).

## Durability model

**Write-ahead.** Every hostcall result is appended to the log *and flushed +
fsynced* before the guest is allowed to observe it. If the process dies between
the side effect and the ack, the run can be resumed or replayed without
re-executing the side effect.

**Replay is reconstruction, never re-execution.** During replay the host returns
the recorded response for each hostcall in order; handlers are not invoked. A
divergence (the guest issuing a different op than recorded) is reported as an
`internal` error, which detects non-determinism and cross-version drift.

## Event envelope

```json
{
  "seq": 1,
  "kind": "hostcall",
  "time": "2026-01-01T00:00:00Z",
  "harness_version": "latigo-local/0.0.0",
  "schema_version": "0",
  "payload": { ... }
}
```

`seq` increases strictly. `harness_version` stamps who produced the event so
cross-version migration is possible.

## Event kinds

| Kind | Payload | Meaning |
|------|---------|---------|
| `run_start` | `RunStart` | First event: run id, ABI version, negotiated capabilities, goal |
| `hostcall` | `Hostcall` | A completed hostcall: op, request bytes, response bytes (write-ahead) |
| `catalog` | `Catalog` | A tool-catalog snapshot; catalog changes arrive as events, so catalogs are replay-safe |
| `checkpoint` | `Checkpoint` | An opaque, guest-defined state snapshot for compaction and bounded replay |
| `run_end` | `RunEnd` | Final event: termination reason and error, if any |

## Checkpoints & compaction

A `checkpoint` records `since_seq` (the last event folded into the snapshot) and
an opaque `state` blob defined by the guest (`(*guest.Agent).Checkpoint`). A host
may **compact** the log by discarding events up to and including `since_seq` and
starting bounded replay from the checkpoint, enabling long-running agents and
cross-version migration.
