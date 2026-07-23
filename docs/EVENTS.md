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

Checkpointing is an optional capability (`Capabilities.Checkpoint`). When the
host grants it, the guest periodically calls **`state.checkpoint`** with an
opaque, restorable snapshot of its state (transcript, virtual filesystem
contents, and the current turn — see the guest's `agentSnapshot`). The host
records each as a `checkpoint` event carrying `since_seq` and the `state` blob.
`state.checkpoint` is written as a `checkpoint` event, not a `hostcall`, so the
snapshot bytes are not duplicated into the hostcall stream.

At startup the guest calls **`state.restore`** (always its second hostcall,
after `tool.list`). On a fresh run this returns `found: false` and the guest
starts normally. On a resumed or compacted run it returns the latest snapshot,
and the guest rehydrates and continues from the recorded turn.

**Compaction** (`host.CompactLog`) rewrites the log to the tail since the most
recent checkpoint: it keeps `run_start`, the guest's initial `tool.list` and
`state.restore` (rewriting the latter to hand back the snapshot), every event
after the checkpoint, and `run_end` — dropping the hostcalls of all folded
turns. On replay the guest restores from the snapshot instead of re-running
those turns, so **replay stays reconstruction, never re-execution**, while the
log is bounded. To keep replay aligned, the reference host injects a synthetic
journal entry for each retained checkpoint (the guest re-issues `state.checkpoint`
at the same points), and skips re-emitting the boundary checkpoint it resumed
from.

This is what makes long-running agents practical: the log does not grow without
bound, and an interrupted run can resume from its last snapshot.

## Durable actors: park & reactivate

Checkpointing also underpins the **durable-actor** lifecycle, where an agent is
never destroyed — it computes, returns a result, and is *parked* as a checkpoint
blob the host stores (e.g. in a database) with zero running footprint.

- **Checkpoint on terminate.** When a run finishes, the guest emits a final
  `state.checkpoint` capturing its completed state (`done`, `summary`, transcript,
  virtual filesystem). This is the up-to-date blob the host parks. (It is skipped
  when an activation did no work — e.g. a pure bounded-replay reconstruction —
  so it never diverges from a compacted journal.)
- **Reactivation.** To re-task a parked agent, the host resumes it as a *new
  activation*: `state.restore` returns `reactivate: true` and an `input` string.
  The guest clears its terminal state, appends `input` as a new user turn, keeps
  its prior transcript and filesystem, and runs again with a fresh turn budget.
  The new activation records its own log and, on completion, parks again.

Both flavours of `state.restore` — bounded-replay/crash *resume* (`reactivate:
false`) and *reactivation* (`reactivate: true`) — are ordinary recorded
hostcalls, so each activation is independently replayable. Identity, storage of
blobs, and single-writer scheduling across many agents are host concerns; the
ABI stays free of leases and tenants.
