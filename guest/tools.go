package guest

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mrn-dk/latigo/abi"
)

// Tool is an in-guest callable exposed to the model.
type Tool struct {
	Name        string
	Description string
	// Schema is a JSON Schema for the tool arguments.
	Schema json.RawMessage
	// Invoke runs the tool with raw JSON args and returns a text result.
	Invoke func(ctx context.Context, args json.RawMessage) (string, error)
}

// Registry holds the in-guest tools plus a bridge to host tools.
type Registry struct {
	local     map[string]Tool
	order     []string
	client    *Client
	hostEpoch int
	hostTools map[string]abi.ToolSpec
}

// NewRegistry creates a registry bound to a client (for host tool proxying).
func NewRegistry(c *Client) *Registry {
	return &Registry{
		local:     map[string]Tool{},
		client:    c,
		hostTools: map[string]abi.ToolSpec{},
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

// Invoke dispatches a tool call to a local tool or, failing that, to the host.
func (r *Registry) Invoke(ctx context.Context, name string, args json.RawMessage) (string, bool) {
	if t, ok := r.local[name]; ok {
		out, err := t.Invoke(ctx, args)
		if err != nil {
			return fmt.Sprintf("error: %v", err), true
		}
		return out, false
	}
	if _, ok := r.hostTools[name]; ok {
		resp, err := r.client.ToolInvoke(name, abi.RawJSON(args))
		if err != nil {
			return fmt.Sprintf("error: %v", err), true
		}
		return string(resp.Result), resp.IsError
	}
	return fmt.Sprintf("error: unknown tool %q", name), true
}
