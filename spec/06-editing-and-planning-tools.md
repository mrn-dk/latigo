# Spec 06 — Structured editing and planning tools

- **Status:** Proposed
- **Capability:** none new (in-guest built-in tools)
- **Affects:** `guest/builtins.go`, `guest/vfs.go`, `guest/agent.go`, new `guest/edit.go`, `guest/plan.go`, `docs/`
- **Sourcing note:** other-harness behaviour is from general knowledge; treat as directional.

## Problem

Latigo ships `read_file`, `write_file`, and a virtual `bash`, but two categories
of table-stakes agent tools are missing:

1. **Structured file editing.** `write_file` overwrites whole files; there is no
   targeted, review-friendly *edit* (find/replace a region, apply a diff). Whole-
   file rewrites are token-heavy, clobber concurrent changes, and are hard to
   approve or display as diffs.
2. **Planning / task tracking.** There is no plan/todo tool, so the model has no
   durable, structured place to lay out and track multi-step work.

## Prior art

- **Claude Code:** ships a structured edit tool (`str_replace` / apply-patch
  style: exact old-string → new-string, must match uniquely) and a multi-edit
  variant; edits render as diffs and can be permission-gated. It also has a
  `TodoWrite`/plan mechanism and a plan mode that drafts steps before acting.
- **Pi:** deliberately keeps the core minimal and pushes plan mode / sub-agents
  to extensions and packages (README "Philosophy"), but editing and todo
  behaviours are common extension/tool patterns; the harness exposes tool
  registration for exactly this.

## Design

### Structured edit tool

Add an `edit_file` built-in operating on the VFS (and, where the host allows,
via `fs.*`). Semantics mirror the widely-used exact-match replace:

```jsonc
// tool: edit_file
{
  "path": "/work/app.go",
  "old": "func handler(",      // must occur exactly once unless "all" is set
  "new": "func handler(ctx context.Context, ",
  "all": false                  // replace every occurrence when true
}
```

Rules (borrowed from proven implementations):

- `old` must match **exactly once** or the edit fails (ambiguity is an error,
  not a guess) — unless `all:true`.
- Empty `old` + non-empty `new` on a missing file = create; empty `new` = delete
  region.
- Return a unified-diff snippet in the tool result so the host can render/approve
  it (composes with Spec 02 approval gating and Spec 01 for rich results).
- A `multi_edit` variant applies an ordered list of `{old,new}` edits atomically
  to one file (all-or-nothing).

Implementation: a small helper in `guest/edit.go` doing exact-match replacement
with occurrence counting, plus diff rendering. It reuses `VFS.ReadFile`/`WriteFile`.

### Plan / todo tool

Add a `plan` (a.k.a. `todo`) built-in backed by agent state:

```jsonc
// tool: plan
{ "op": "set", "items": [
    {"id": 1, "text": "read the failing test", "status": "done"},
    {"id": 2, "text": "fix the handler",       "status": "in_progress"},
    {"id": 3, "text": "run the suite",          "status": "pending"}
]}
// ops: "set" | "update" (by id) | "get"
```

- Stored on the `Agent` (e.g. `a.plan []PlanItem`) and **included in the
  checkpoint snapshot** (`agentSnapshot`, Spec on checkpoints) so it survives
  restore/compaction.
- The current plan can be surfaced into the system/context each turn (pinned),
  and rendered by the host UI.
- Optional integration with compaction: the plan is *never* elided (pinned
  context), so long runs keep their task structure.

### Guest wiring

Register both in `guest/builtins.go`. They are pure in-guest (VFS + agent state)
— **no hostcalls** — so they are deterministic and free to replay. `edit_file`
that targets the host filesystem instead of the VFS would go through `fs.read` +
`fs.write` hostcalls (recorded), which is the only variant that touches the log.

## Determinism & replay

- VFS-backed `edit_file` and `plan` are pure guest computation over recorded
  inputs, so they reconstruct exactly on replay with zero new event kinds.
- Host-FS edits use existing `fs.*` hostcalls (already recorded write-ahead).
- Because the plan lives in the checkpoint snapshot, a resumed/compacted run
  keeps its task list.

## Testing

- `edit_file`: unique-match replace succeeds; zero matches and multiple matches
  error; `all` replaces every occurrence; create/delete-region cases; returned
  diff is correct.
- `multi_edit` atomicity: a failing sub-edit rolls back the whole file.
- `plan` set/update/get; plan persists across `checkpointState`→`restore`.
- Replay of a run using both tools reproduces identical VFS + transcript.

## Non-goals / open questions

- Language-aware / AST edits (out of scope; exact-match is provider-agnostic and
  predictable).
- A separate "plan mode" that blocks execution until a plan is approved — that is
  an approval-policy composition (Spec 02), not a new tool.
- Concurrency between edits and bash writes to the same file (single-threaded
  guest, so not an issue today).
