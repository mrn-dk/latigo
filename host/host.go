package host

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mrn-dk/latigo/abi"
	"github.com/mrn-dk/latigo/events"
)

// Handler implements one ABI operation. It receives the raw args and returns
// the raw result. Returning an error with a non-empty code maps to a structured
// host error response.
type Handler func(ctx context.Context, args json.RawMessage) (result json.RawMessage, err error)

// CodedError carries a stable ABI error code.
type CodedError struct {
	Code string
	Msg  string
}

func (e *CodedError) Error() string { return e.Msg }

// Errorf builds a CodedError.
func Errorf(code, format string, a ...any) error {
	return &CodedError{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// Host dispatches guest hostcalls, enforces write-ahead durability, and (in
// replay mode) returns recorded results instead of re-executing side effects.
type Host struct {
	caps     abi.Capabilities
	handlers map[abi.Op]Handler
	log      *EventLog

	// replay journal: recorded hostcall responses to return during replay.
	replay    []events.Hostcall
	replayIdx int
	replaying bool
}

// New builds a Host with the given capabilities and event log.
func New(caps abi.Capabilities, log *EventLog) *Host {
	caps.ABIVersion = abi.Version
	return &Host{
		caps:     caps,
		handlers: map[abi.Op]Handler{},
		log:      log,
	}
}

// Handle registers a handler for op.
func (h *Host) Handle(op abi.Op, fn Handler) { h.handlers[op] = fn }

// Capabilities returns the host capability set.
func (h *Host) Capabilities() abi.Capabilities { return h.caps }

// LoadReplay puts the host into replay mode using recorded hostcall events.
// Handlers are never invoked while replaying; recorded responses are returned
// verbatim so state is reconstructed rather than re-executed.
func (h *Host) LoadReplay(evs []events.Event) error {
	h.replay = nil
	for _, ev := range evs {
		if ev.Kind != events.KindHostcall {
			continue
		}
		var hc events.Hostcall
		if err := json.Unmarshal(ev.Payload, &hc); err != nil {
			return err
		}
		h.replay = append(h.replay, hc)
	}
	h.replaying = len(h.replay) > 0
	h.replayIdx = 0
	return nil
}

// Replaying reports whether the host is in replay mode.
func (h *Host) Replaying() bool { return h.replaying }

// Dispatch executes (or replays) a single hostcall and returns the raw response
// bytes to hand back to the guest.
func (h *Host) Dispatch(ctx context.Context, reqBytes []byte) []byte {
	var req abi.Request
	if err := json.Unmarshal(reqBytes, &req); err != nil {
		return encodeResponse(abi.Response{Error: "malformed request: " + err.Error(), Code: abi.ErrInvalid})
	}

	// Replay: return the recorded response without side effects.
	if h.replaying && h.replayIdx < len(h.replay) {
		rec := h.replay[h.replayIdx]
		h.replayIdx++
		if rec.Op != req.Op {
			return encodeResponse(abi.Response{
				Error: fmt.Sprintf("replay divergence: expected %s, guest issued %s", rec.Op, req.Op),
				Code:  abi.ErrInternal,
			})
		}
		return rec.Response
	}

	resp := h.execute(ctx, req)
	respBytes := encodeResponse(resp)

	// Write-ahead: append and flush the result before the guest observes it.
	if h.log != nil {
		if _, err := h.log.Append(events.KindHostcall, events.Hostcall{
			Op:       req.Op,
			Request:  reqBytes,
			Response: respBytes,
		}); err != nil {
			return encodeResponse(abi.Response{Error: "durability failure: " + err.Error(), Code: abi.ErrInternal})
		}
	}
	return respBytes
}

func (h *Host) execute(ctx context.Context, req abi.Request) abi.Response {
	fn, ok := h.handlers[req.Op]
	if !ok {
		return abi.Response{Error: "unsupported op: " + string(req.Op), Code: abi.ErrUnsupported}
	}
	result, err := fn(ctx, req.Args)
	if err != nil {
		var ce *CodedError
		code := abi.ErrInternal
		if asCoded(err, &ce) {
			code = ce.Code
		}
		return abi.Response{Error: err.Error(), Code: code}
	}
	return abi.Response{Result: result}
}

func encodeResponse(r abi.Response) []byte {
	b, err := json.Marshal(r)
	if err != nil {
		return []byte(`{"error":"host encode failure","code":"internal"}`)
	}
	return b
}

func asCoded(err error, target **CodedError) bool {
	for err != nil {
		if ce, ok := err.(*CodedError); ok {
			*target = ce
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
