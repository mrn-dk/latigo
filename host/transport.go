package host

import (
	"context"
	"encoding/json"

	"github.com/mrn-dk/latigo/abi"
)

// Transport adapts a Host to the in-process hostcall interface used by the
// guest client and the conformance suite. It exercises the same Dispatch path
// the WASM bridge uses, including write-ahead logging.
type Transport struct {
	h   *Host
	ctx context.Context
}

// AsTransport returns an in-process Transport over h.
func (h *Host) AsTransport(ctx context.Context) *Transport {
	if ctx == nil {
		ctx = context.Background()
	}
	return &Transport{h: h, ctx: ctx}
}

// Hostcall dispatches req and decodes the response envelope.
func (t *Transport) Hostcall(req abi.Request) (abi.Response, error) {
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return abi.Response{}, err
	}
	respBytes := t.h.Dispatch(t.ctx, reqBytes)
	var resp abi.Response
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return abi.Response{}, err
	}
	return resp, nil
}
