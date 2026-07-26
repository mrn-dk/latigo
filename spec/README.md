# Latigo capability specs (roadmap)

These are **proposed** specifications for the capabilities Latigo is missing
relative to full coding-agent harnesses (Claude Code, Pi), prioritised from a
gap analysis. Each is designed to fit Latigo's charter: sandboxed by
construction, deterministic, and reconstructable from the event log. Every spec
is additive and capability-gated, so existing hosts and logs are unaffected.

| # | Spec | Prioritised because | New capability |
|---|------|---------------------|----------------|
| 01 | [Multimodal content](01-multimodal-content.md) | highest-impact gap; text-only ABI | `multimodal` |
| 02 | [In-loop steering & approval gating](02-inloop-steering-and-approval.md) | primitives already exist, just unwired; low risk | (uses `approval`, `msg.recv`) |
| 03 | [LLM robustness: retry/backoff](03-llm-robustness-retry.md) | a transient 429 currently kills a run | — (host-side) |
| 04 | [Cost & budget accounting](04-cost-and-budget-accounting.md) | unbounded spend; no durable cost record | `budget` |
| 05 | [Reasoning, prompt caching, parallel tools](05-provider-features-reasoning-caching.md) | cost + capability parity with modern providers | `reasoning`, `prompt_cache`, `parallel_tools` |
| 06 | [Structured editing & planning tools](06-editing-and-planning-tools.md) | table-stakes agent tools missing | — (in-guest tools) |
| 07 | [Streaming & session branching](07-streaming-and-session-branching.md) | bigger design efforts; deferred | `streaming` |

## Cross-cutting principle

The recurring design question is the boundary between the **deterministic event
log** and the **outside world**:

- Anything that must be *reconstructable* (LLM responses, tool results, approval
  decisions, steering messages, usage) is a **recorded hostcall/event** and
  replays verbatim.
- Anything that is *ephemeral UX* (streaming deltas, backoff waits) lives
  **below or beside** the durability boundary and is never logged, so replay is
  unaffected.

Specs are ordered by recommended implementation sequence; 02 and 03 are the
lowest-risk, highest-leverage starting points, and 01 is the highest-impact but
needs the content-type design in spec 01 landed first.
