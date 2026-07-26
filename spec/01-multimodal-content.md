# Spec 01 — Multimodal content (images in messages and tool results)

- **Status:** Proposed
- **New capability:** `multimodal`
- **Affects:** `abi/messages.go`, `abi/capabilities.go`, `guest/tools.go`, `guest/agent.go`, `guest/builtins.go`, `host/llm.go`, `docs/ABI.md`
- **Sourcing note:** Claude Code / Pi behaviour below is from general knowledge of these tools and the Pi docs bundled with this workspace, not a live audit; treat specifics as directional.

## Problem

Latigo's transcript is text-only. `abi.LLMMessage.Content` is a bare `string`,
and tool results ultimately become a string (`ToolInvokeResponse.Result` /
`guest.Tool.Invoke` returns `string`). There is no way to:

- send an image to a vision model (screenshot, diagram, PDF page, chart), or
- return an image *from a tool* (e.g. a browser/screenshot tool, a plot
  produced by a Starlark script, a rendered diff image).

Every mainstream harness is multimodal today; this is the single biggest
capability gap for a "modern agent harness."

## Prior art

- **Claude Code:** users paste/drag images into the prompt; the model reasons
  over them (screenshots of failing UIs, error dialogs, design mocks). Tool
  results can carry images.
- **Pi:** images appear across the TUI, RPC, and SDK surfaces (the docs
  reference image handling in `tui.md`, `rpc.md`, `sdk.md`). Pasted/attached
  images are delivered to the model.
- **Provider wire formats:** both OpenAI and Anthropic represent a message's
  content as an **array of typed parts** (`{type:"text",...}`,
  `{type:"image", ...}` with base64 or URL + media type). Anthropic tool results
  may themselves contain image blocks.

## Design

### ABI (content parts)

Introduce a structured, backward-compatible content model. `Content string`
stays as the text shorthand; add an optional `Parts` array. When `Parts` is
non-empty it is authoritative and `Content` is ignored (or treated as the first
text part).

```go
// abi/messages.go
type ContentPart struct {
    Type  string     `json:"type"`            // "text" | "image"
    Text  string     `json:"text,omitempty"`  // when Type=="text"
    Image *ImageData `json:"image,omitempty"` // when Type=="image"
}

type ImageData struct {
    MediaType string `json:"media_type"`      // e.g. "image/png", "image/jpeg"
    Data      []byte `json:"data,omitempty"`  // base64 via encoding/json
    URL       string `json:"url,omitempty"`   // alternative to inline data
}

type LLMMessage struct {
    Role       string        `json:"role"`
    Content    string        `json:"content"`          // text shorthand (kept)
    Parts      []ContentPart `json:"parts,omitempty"`  // NEW: multimodal content
    Name       string        `json:"name,omitempty"`
    ToolCallID string        `json:"tool_call_id,omitempty"`
    ToolCalls  []LLMToolCall `json:"tool_calls,omitempty"`
}
```

Tool results gain the same shape so tools can return images:

```go
type ToolInvokeResponse struct {
    Result  RawJSON       `json:"result"`
    Parts   []ContentPart `json:"parts,omitempty"` // NEW
    IsError bool          `json:"is_error"`
}
```

### Capability negotiation

Add `Multimodal bool` to `abi.Capabilities` (the host advertises it only when
the configured model actually accepts images). `Negotiate` ANDs it. The guest
**must not** emit image parts when `!caps.Multimodal`; it degrades by dropping
images and inserting a text placeholder (`"[image omitted: host is not
multimodal]"`), so a text-only host still works.

### Guest

- `guest.Tool.Invoke` signature grows a structured return. To stay source-compatible,
  add an optional richer tool type or a second interface:
  ```go
  type RichResult struct { Text string; Parts []abi.ContentPart }
  ```
  Built-ins keep returning strings; a new screenshot/plot tool returns `Parts`.
- `Registry.Invoke` maps a tool's `Parts` into the `tool` role message's `Parts`.
- Add a built-in **`attach_image`** (or extend `read_file`) that reads an image
  from the VFS/host FS and attaches it as an image part to the next user turn.
- The agent's initial user turn may include image parts supplied by the host
  (see below).

### Host / reference

- `host/llm.go` translates `Parts` into the provider's content-array format
  (OpenAI/Anthropic). This is the only place that knows the wire dialect.
- `latigo-local` gains a way to attach images to the initial goal, e.g.
  `-image path.png` (repeatable), placed into `run_start`/the first user message.

### Determinism & replay

Images are ordinary bytes inside `llm.call` requests, which are already recorded
verbatim in the event log, so **replay is unaffected** — the exact bytes the
model saw are reconstructed. Two caveats:

- **Log size.** Base64 images inflate the log. Recommend a host-side cap
  (e.g. downscale to a max dimension / N MB before send) and note that
  checkpoints (Spec) should store image *references* (VFS paths) rather than
  inlining bytes twice.
- The oversized-response retry cache in `host/runtime.go` already tolerates large
  payloads.

## Testing

- ABI round-trip of a message with mixed text+image parts.
- `host/llm.go` translation to OpenAI/Anthropic content arrays (table test).
- Guest degradation when `!caps.Multimodal` (image dropped, placeholder added).
- A fake tool returning image `Parts` surfaces as a `tool` message with an image.

## Non-goals / open questions

- Audio/video content (future `Type` values; the enum is open).
- Client-side OCR or image resizing in the guest (host's job).
- Whether `Content`+`Parts` coexistence should be normalised at the ABI boundary
  or left to the host translator (leaning: normalise in the guest client).

## Rollout

Additive and capability-gated: text-only hosts and existing logs are unaffected
because `Parts` is `omitempty` and defaults to empty.
