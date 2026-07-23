package host_test

import (
	"context"
	"testing"

	"github.com/mrn-dk/latigo/abi"
	"github.com/mrn-dk/latigo/conformance"
	"github.com/mrn-dk/latigo/host"
)

// newTestHost builds a fully-configured in-memory host for conformance.
func newTestHost(t *testing.T) *host.Host {
	t.Helper()
	h := host.New(abi.Capabilities{FSWrite: true, HostVersion: "test"}, nil)
	if err := h.FS(t.TempDir(), true); err != nil {
		t.Fatal(err)
	}
	h.Clock(nil)
	h.Rand(nil)
	h.Log(discard{})
	h.Messaging(host.Messenger{})
	h.Tools(host.NewStaticTools())
	host.ScriptedMockLLM("test").Register(h)
	// Advertise governed HTTP so the conformance suite verifies the SSRF guard.
	h.HTTP(host.HTTPFetcher(host.HTTPPolicy{AllowHosts: []string{"*"}}))
	return h
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func TestConformance(t *testing.T) {
	h := newTestHost(t)
	suite := conformance.Suite{Caps: h.Capabilities()}
	results := suite.RunAll(h.AsTransport(context.Background()))
	for _, r := range results {
		if !r.OK {
			t.Errorf("conformance %q failed: %s", r.Name, r.Err)
		} else {
			t.Logf("ok: %s", r.Name)
		}
	}
}
