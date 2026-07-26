# Spec 02 — In-loop steering and approval gating

- **Status:** Proposed
- **Capabilities:** existing `approval` (wire it in) + existing `msg.recv` (wire it in); optional new `SteeringPolicy`/`ApprovalPolicy` strategy points
- **Affects:** `guest/agent.go`, `guest/tools.go`, `cmd/latigo-local/main.go`, `docs/ABI.md`
- **Sourcing note:** other-harness behaviour is from general knowledge; treat as directional.

## Problem

Latigo already has the two primitives needed for human/host-in-the-loop control,
but **neither is used by the agent loop**:

- `Client.ApprovalAwait` (guest/client.go) exists and `approval.await` is a real
  host op, yet `guest/agent.go` invokes tools directly (`a.tools.Invoke(...)`)
  with no approval gate.
- `Client.MsgRecv` exists, but the loop never checks for an injected message
  between turns, so there is no way to *steer* a running agent.

So today a run is fire-and-forget: you cannot require confirmation before a
dangerous tool call, and you cannot nudge the agent mid-run.

## Prior art

- **Claude Code:** a permission system gates tools — allow/deny/ask per tool and
  per path, with an allowlist you can extend ("always allow `npm test`"). ESC
  interrupts the current turn; a message queue lets you type while it works and
  the input is consumed at the next turn boundary. Plan mode gates edits behind
  approval.
- **Pi:** extensions can **intercept tool calls** to block/modify them
  (permission gates like "confirm before `rm -rf`", "block writes to `.env`")
  and a message queue lets you steer between turns (see `extensions.md`,
  `usage.md`). Approval/permission is an interception concern, not a core hard-code.

## Design

Two cooperating pieces, both driven by overridable strategy points so hosts can
set policy without editing the loop.

### Approval gating

Add a guest strategy point:

```go
// guest/agent.go
// ApprovalGate decides whether a tool call needs host approval, returning the
// action name + details to show. Returning ok=false means "no approval needed".
ApprovalGate func(a *Agent, name string, args json.RawMessage) (action string, details json.RawMessage, need bool)
```

In the tool-dispatch section of `Run`, before `a.tools.Invoke`:

```go
if a.cfg.Capabilities.Approval {
    if action, details, need := a.ApprovalGate(a, tc.Name, args); need {
        dec, _ := a.client.ApprovalAwait(action, details)
        if !dec.Approved {
            // feed a denial back to the model as the tool result and continue
            out = "denied by host: " + dec.Reason
            appendToolResult(tc, out); continue
        }
    }
}
```

Default `ApprovalGate`: require approval for the ambient/dangerous surface —
`exec.run`-backed tools, `http_fetch`, VFS writes outside `/work`, `fs.remove`.
Because `ApprovalAwait` already degrades to "approved" when the capability is
absent (guest/client.go), text-only hosts are unaffected.

### Steering (mid-run messages)

Add a strategy point + a loop check at the **top of each turn**:

```go
// Steer decides whether to pull a pending host message and how to inject it.
Steer func(a *Agent) (inject *abi.LLMMessage, stop bool)
```

Default `Steer`: non-blocking `a.client.MsgRecv("steer", false)`; if a message
is present, append it as a `user` message (so the model sees new guidance next
turn); support a sentinel (e.g. `"/stop"`) to request graceful termination.

```go
for turn := startTurn; ; turn++ {
    if inject, stop := a.Steer(a); stop {
        break
    } else if inject != nil {
        a.messages = append(a.messages, *inject)
    }
    ...
}
```

### Host / reference

- `latigo-local` already has `interactiveApproval`; wire it to the new gate and
  keep the `-approve` flag. Add richer policy later (allowlist of pre-approved
  tools, per-path rules), mirroring Claude Code's allow/deny/ask.
- Provide a `msg.recv("steer")` source: for the CLI, a background goroutine that
  reads stdin lines and enqueues them; for embedders, the `Messenger.In` hook
  (already in `host.Messenger`).

## Determinism & replay

Both `approval.await` and `msg.recv` are ordinary hostcalls, already recorded
write-ahead. Their **results** are replayed verbatim, so an approved/denied
decision and any injected steering message reconstruct exactly — the human/host
input becomes part of the durable, replayable record. No new machinery.

One subtlety: `Steer` issues a `msg.recv` **every turn**, adding one recorded
hostcall per turn even when no message is present. That is fine (it replays
identically) but slightly enlarges the log; gate it behind a `Steerable`
capability or a `SteerEvery(n)` policy if noise matters.

## Testing

- Loop calls `ApprovalAwait` for a gated tool and skips it for an ungated one;
  a denial is fed back to the model and the run continues.
- With no `approval` capability, tools run without prompting (degradation).
- `Steer` injects a pending message at the next turn boundary; `/stop` ends the
  run; absence of a message is a no-op.
- Replay of a run that included an approval denial and a steering message
  reproduces the same transcript.

## Non-goals / open questions

- Interrupting a tool call **mid-execution** (true preemption) — out of scope;
  gating happens at call boundaries. Long-running tools should support
  cancellation via `context` (host concern).
- A full permission-rule DSL (allow/deny globs) — start with a Go policy
  function; a declarative format can come later.
