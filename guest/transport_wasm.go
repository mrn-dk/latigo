//go:build wasip1

package guest

import (
	"encoding/json"
	"errors"
	"unsafe"

	"github.com/mrn-dk/latigo/abi"
)

//go:wasmimport latigo_abi hostcall
func hostcall(reqPtr, reqLen, respPtr, respCap uint32) int32

// respBuf is a stable, pinned scratch buffer for host responses. Go's wasm
// runtime uses a non-moving allocator, so the address of the backing array is
// stable for the lifetime of the slice.
var respBuf = make([]byte, 1<<16)

// wasmTransport is the real ABI transport backed by the imported hostcall.
type wasmTransport struct{}

func newDefaultTransport() Transport { return wasmTransport{} }

func (wasmTransport) Hostcall(req abi.Request) (abi.Response, error) {
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return abi.Response{}, err
	}
	reqPtr := uint32(uintptr(unsafe.Pointer(&reqBytes[0])))
	reqLen := uint32(len(reqBytes))

	for {
		respPtr := uint32(uintptr(unsafe.Pointer(&respBuf[0])))
		n := hostcall(reqPtr, reqLen, respPtr, uint32(len(respBuf)))
		if n < 0 {
			return abi.Response{}, errors.New("latigo: hostcall transport error")
		}
		if int(n) > len(respBuf) {
			respBuf = make([]byte, int(n))
			continue
		}
		var resp abi.Response
		if err := json.Unmarshal(respBuf[:n], &resp); err != nil {
			return abi.Response{}, err
		}
		// Keep reqBytes alive across the call.
		runtimeKeepAlive(reqBytes)
		return resp, nil
	}
}

// runtimeKeepAlive prevents the argument from being collected before this point.
//
//go:noinline
func runtimeKeepAlive(x any) {}
