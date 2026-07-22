//go:build !wasip1

package guest

import (
	"errors"

	"github.com/mrn-dk/latigo/abi"
)

// On non-wasm builds there is no imported hostcall. A Transport must be
// supplied explicitly (see NewClient). This keeps the guest package buildable
// by host tooling and unit tests.
func newDefaultTransport() Transport { return unavailableTransport{} }

type unavailableTransport struct{}

func (unavailableTransport) Hostcall(abi.Request) (abi.Response, error) {
	return abi.Response{}, errors.New("latigo: no ABI transport on this platform; supply one via NewClient")
}
