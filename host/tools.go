package host

import (
	"context"
	"encoding/json"

	"github.com/mrn-dk/latigo/abi"
)

// ToolProvider is the host's tool router. Routing (native, MCP, remote, ...) is
// the host's business; the guest only sees tool.list / tool.invoke.
type ToolProvider interface {
	// List returns the current catalog and its epoch. The epoch must change
	// whenever the catalog changes so the guest can detect updates.
	List(ctx context.Context) ([]abi.ToolSpec, int, error)
	// Invoke runs a named tool with raw JSON args.
	Invoke(ctx context.Context, name string, args json.RawMessage) (result json.RawMessage, isError bool, err error)
}

// Tools registers tool.list / tool.invoke against a provider.
func (h *Host) Tools(p ToolProvider) {
	h.Handle(abi.OpToolList, handler(func(ctx context.Context, _ abi.ToolListRequest) (abi.ToolListResponse, error) {
		tools, epoch, err := p.List(ctx)
		if err != nil {
			return abi.ToolListResponse{}, err
		}
		return abi.ToolListResponse{Tools: tools, Epoch: epoch}, nil
	}))
	h.Handle(abi.OpToolInvoke, handler(func(ctx context.Context, r abi.ToolInvokeRequest) (abi.ToolInvokeResponse, error) {
		res, isErr, err := p.Invoke(ctx, r.Name, json.RawMessage(r.Args))
		if err != nil {
			return abi.ToolInvokeResponse{}, err
		}
		return abi.ToolInvokeResponse{Result: json.RawMessage(res), IsError: isErr}, nil
	}))
}

// StaticTools is a fixed catalog whose tools are Go funcs. Useful for tests and
// simple hosts.
type StaticTools struct {
	specs []abi.ToolSpec
	funcs map[string]func(ctx context.Context, args json.RawMessage) (json.RawMessage, bool, error)
	epoch int
}

// NewStaticTools returns an empty static catalog.
func NewStaticTools() *StaticTools {
	return &StaticTools{funcs: map[string]func(context.Context, json.RawMessage) (json.RawMessage, bool, error){}}
}

// Register adds/updates a tool and bumps the epoch.
func (s *StaticTools) Register(spec abi.ToolSpec, fn func(ctx context.Context, args json.RawMessage) (json.RawMessage, bool, error)) {
	for i, sp := range s.specs {
		if sp.Name == spec.Name {
			s.specs[i] = spec
			s.funcs[spec.Name] = fn
			s.epoch++
			return
		}
	}
	s.specs = append(s.specs, spec)
	s.funcs[spec.Name] = fn
	s.epoch++
}

func (s *StaticTools) List(context.Context) ([]abi.ToolSpec, int, error) {
	return s.specs, s.epoch, nil
}

func (s *StaticTools) Invoke(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, bool, error) {
	fn, ok := s.funcs[name]
	if !ok {
		return nil, true, Errorf(abi.ErrNotFound, "unknown tool: %s", name)
	}
	return fn(ctx, args)
}
