package guest

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mrn-dk/latigo/abi"
)

// imageOmittedPlaceholder replaces an image content part when the host has
// not negotiated the multimodal capability. The guest must never emit image
// parts to a non-multimodal host; this text stands in for the dropped image.
const imageOmittedPlaceholder = "[image omitted: host is not multimodal]"

// RichResult is the structured return value of Tool.InvokeRich: a text
// summary plus zero or more structured content parts (e.g. images) to attach
// to the resulting tool-role message.
type RichResult struct {
	Text  string
	Parts []abi.ContentPart
}

// Tool is an in-guest callable exposed to the model.
type Tool struct {
	Name        string
	Description string
	// Schema is a JSON Schema for the tool arguments.
	Schema json.RawMessage
	// Invoke runs the tool with raw JSON args and returns a text result. Every
	// built-in tool implements only this, so its signature stays untouched —
	// changing it would churn every existing tool for no benefit.
	Invoke func(ctx context.Context, args json.RawMessage) (string, error)
	// InvokeRich is an optional richer hook that can also return structured
	// content parts (e.g. an image). When non-nil, Registry.Invoke prefers it
	// over Invoke. A tool that wants to attach images to its result (a
	// screenshot tool, a plot, attach_image) sets this instead of/alongside
	// Invoke.
	InvokeRich func(ctx context.Context, args json.RawMessage) (RichResult, error)
}

// Registry holds the in-guest tools plus a bridge to host tools.
type Registry struct {
	local     map[string]Tool
	order     []string
	client    *Client
	hostEpoch int
	hostTools map[string]abi.ToolSpec
	// multimodal mirrors the negotiated Capabilities.Multimodal: whether the
	// host accepts image content parts. When false, Invoke degrades any
	// image parts a tool returns rather than emitting them.
	multimodal bool
}

// NewRegistry creates a registry bound to a client (for host tool proxying).
// multimodal is the negotiated abi.Capabilities.Multimodal: when false, image
// content parts returned by tools are degraded to a text placeholder instead
// of being surfaced to the model.
func NewRegistry(c *Client, multimodal bool) *Registry {
	return &Registry{
		local:      map[string]Tool{},
		client:     c,
		hostTools:  map[string]abi.ToolSpec{},
		multimodal: multimodal,
	}
}

// Add registers a local tool.
func (r *Registry) Add(t Tool) {
	if _, ok := r.local[t.Name]; !ok {
		r.order = append(r.order, t.Name)
	}
	r.local[t.Name] = t
}

// RefreshHostCatalog pulls the host tool catalog. Catalog changes arrive as
// events, so this is replay-safe.
func (r *Registry) RefreshHostCatalog() (int, error) {
	resp, err := r.client.ToolList()
	if err != nil {
		if IsUnsupported(err) {
			return r.hostEpoch, nil
		}
		return r.hostEpoch, err
	}
	r.hostEpoch = resp.Epoch
	r.hostTools = map[string]abi.ToolSpec{}
	for _, t := range resp.Tools {
		r.hostTools[t.Name] = t
	}
	return resp.Epoch, nil
}

// Specs returns the tool specs advertised to the model (local + host).
func (r *Registry) Specs() []abi.LLMToolSpec {
	var specs []abi.LLMToolSpec
	for _, name := range r.order {
		t := r.local[name]
		specs = append(specs, abi.LLMToolSpec{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  paramsOrEmpty(t.Schema),
		})
	}
	for name, t := range r.hostTools {
		specs = append(specs, abi.LLMToolSpec{
			Name:        name,
			Description: t.Description,
			Parameters:  paramsOrEmpty(t.Parameters),
		})
	}
	return specs
}

func paramsOrEmpty(s json.RawMessage) abi.RawJSON {
	if len(s) == 0 {
		return abi.RawJSON(`{"type":"object","properties":{}}`)
	}
	return abi.RawJSON(s)
}

// Invoke dispatches a tool call to a local tool or, failing that, to the
// host. It returns the tool's text result, any structured content parts to
// attach to the resulting tool-role message (already degraded per the
// negotiated multimodal capability — see sanitizeParts), and whether the call
// was an error.
func (r *Registry) Invoke(ctx context.Context, name string, args json.RawMessage) (string, []abi.ContentPart, bool) {
	if t, ok := r.local[name]; ok {
		if t.InvokeRich != nil {
			res, err := t.InvokeRich(ctx, args)
			if err != nil {
				return fmt.Sprintf("error: %v", err), nil, true
			}
			return res.Text, r.sanitizeParts(res.Parts), false
		}
		out, err := t.Invoke(ctx, args)
		if err != nil {
			return fmt.Sprintf("error: %v", err), nil, true
		}
		return out, nil, false
	}
	if _, ok := r.hostTools[name]; ok {
		resp, err := r.client.ToolInvoke(name, abi.RawJSON(args))
		if err != nil {
			return fmt.Sprintf("error: %v", err), nil, true
		}
		return string(resp.Result), r.sanitizeParts(resp.Parts), resp.IsError
	}
	return fmt.Sprintf("error: unknown tool %q", name), nil, true
}

// sanitizeParts enforces the multimodal capability gate: when the host has
// not negotiated it, image parts are never emitted — each is replaced with a
// text placeholder part instead, so a text-only host is unaffected regardless
// of what a tool tries to attach.
func (r *Registry) sanitizeParts(parts []abi.ContentPart) []abi.ContentPart {
	return sanitizeContentParts(parts, r.multimodal)
}

// sanitizeContentParts drops image parts (replacing each with a text
// placeholder) unless multimodal is true. It is a free function so both
// Registry.Invoke and the agent's initial-turn message construction share
// exactly one degradation policy.
func sanitizeContentParts(parts []abi.ContentPart, multimodal bool) []abi.ContentPart {
	if multimodal || len(parts) == 0 {
		return parts
	}
	out := make([]abi.ContentPart, 0, len(parts))
	for _, p := range parts {
		if p.Type == "image" {
			out = append(out, abi.ContentPart{Type: "text", Text: imageOmittedPlaceholder})
			continue
		}
		out = append(out, p)
	}
	return out
}
