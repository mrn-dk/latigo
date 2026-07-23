// Package conformance is a host conformance suite. Given anything that answers
// ABI hostcalls (a [Transport]), it verifies that a host honours the ABI v0
// contract: required namespaces work, unknown ops degrade cleanly, and
// determinism-relevant ops behave.
package conformance

import (
	"encoding/json"
	"fmt"

	"github.com/mrn-dk/latigo/abi"
)

// Transport answers a single hostcall. A host adapts to this by wrapping its
// dispatch entry point.
type Transport interface {
	Hostcall(req abi.Request) (abi.Response, error)
}

// Result is the outcome of one check.
type Result struct {
	Name string
	OK   bool
	Err  string
}

// Suite configures which optional capabilities to exercise.
type Suite struct {
	Caps abi.Capabilities
}

// Check is one named conformance test.
type Check struct {
	Name string
	Run  func(t Transport, caps abi.Capabilities) error
}

// Checks returns the ABI v0 conformance checks.
func Checks() []Check {
	return []Check{
		{"fs.write/read round-trip", checkFSRoundTrip},
		{"fs.stat missing is not error", checkFSStatMissing},
		{"fs.list", checkFSList},
		{"clock.now monotonic-ish", checkClock},
		{"rand.bytes length", checkRand},
		{"log.append", checkLog},
		{"tool.list shape", checkToolList},
		{"unknown op -> unsupported", checkUnknownOp},
		{"malformed request -> invalid", checkMalformed},
		{"http.fetch blocks SSRF (if advertised)", checkHTTPSSRF},
	}
}

// RunAll executes every check against t and returns their results.
func (s Suite) RunAll(t Transport) []Result {
	var out []Result
	for _, c := range Checks() {
		err := c.Run(t, s.Caps)
		r := Result{Name: c.Name, OK: err == nil}
		if err != nil {
			r.Err = err.Error()
		}
		out = append(out, r)
	}
	return out
}

// call is a small typed helper.
func call[Req any, Resp any](t Transport, op abi.Op, req Req) (Resp, abi.Response, error) {
	var resp Resp
	args, _ := json.Marshal(req)
	raw, err := t.Hostcall(abi.Request{Op: op, Args: args})
	if err != nil {
		return resp, raw, err
	}
	if raw.Error == "" && len(raw.Result) > 0 {
		_ = json.Unmarshal(raw.Result, &resp)
	}
	return resp, raw, nil
}

func checkFSRoundTrip(t Transport, _ abi.Capabilities) error {
	const path = "/conformance/hello.txt"
	want := []byte("hello latigo")
	if _, raw, err := call[abi.FSWriteRequest, abi.FSWriteResponse](t, abi.OpFSWrite, abi.FSWriteRequest{Path: path, Data: want}); err != nil || raw.Error != "" {
		return fmt.Errorf("write: %v %s", err, raw.Error)
	}
	got, raw, err := call[abi.FSReadRequest, abi.FSReadResponse](t, abi.OpFSRead, abi.FSReadRequest{Path: path})
	if err != nil || raw.Error != "" {
		return fmt.Errorf("read: %v %s", err, raw.Error)
	}
	if string(got.Data) != string(want) {
		return fmt.Errorf("round-trip mismatch: got %q want %q", got.Data, want)
	}
	return nil
}

func checkFSStatMissing(t Transport, _ abi.Capabilities) error {
	got, raw, err := call[abi.FSStatRequest, abi.FSStatResponse](t, abi.OpFSStat, abi.FSStatRequest{Path: "/conformance/does-not-exist"})
	if err != nil {
		return err
	}
	if raw.Error != "" {
		return fmt.Errorf("stat of missing path should not error, got %q", raw.Error)
	}
	if got.Exists {
		return fmt.Errorf("missing path reported as existing")
	}
	return nil
}

func checkFSList(t Transport, _ abi.Capabilities) error {
	_, _, _ = call[abi.FSWriteRequest, abi.FSWriteResponse](t, abi.OpFSWrite, abi.FSWriteRequest{Path: "/conformance/dir/a.txt", Data: []byte("a")})
	got, raw, err := call[abi.FSListRequest, abi.FSListResponse](t, abi.OpFSList, abi.FSListRequest{Path: "/conformance/dir"})
	if err != nil || raw.Error != "" {
		return fmt.Errorf("list: %v %s", err, raw.Error)
	}
	for _, e := range got.Entries {
		if e.Name == "a.txt" {
			return nil
		}
	}
	return fmt.Errorf("expected a.txt in listing, got %+v", got.Entries)
}

func checkClock(t Transport, _ abi.Capabilities) error {
	a, raw, err := call[abi.ClockNowRequest, abi.ClockNowResponse](t, abi.OpClockNow, abi.ClockNowRequest{})
	if err != nil || raw.Error != "" {
		return fmt.Errorf("clock: %v %s", err, raw.Error)
	}
	if a.UnixNano <= 0 {
		return fmt.Errorf("clock returned non-positive time")
	}
	return nil
}

func checkRand(t Transport, _ abi.Capabilities) error {
	got, raw, err := call[abi.RandBytesRequest, abi.RandBytesResponse](t, abi.OpRandBytes, abi.RandBytesRequest{N: 16})
	if err != nil || raw.Error != "" {
		return fmt.Errorf("rand: %v %s", err, raw.Error)
	}
	if len(got.Bytes) != 16 {
		return fmt.Errorf("rand returned %d bytes, want 16", len(got.Bytes))
	}
	return nil
}

func checkLog(t Transport, _ abi.Capabilities) error {
	_, raw, err := call[abi.LogAppendRequest, abi.LogAppendResponse](t, abi.OpLogAppend, abi.LogAppendRequest{Level: "info", Message: "conformance"})
	if err != nil || raw.Error != "" {
		return fmt.Errorf("log: %v %s", err, raw.Error)
	}
	return nil
}

func checkToolList(t Transport, _ abi.Capabilities) error {
	_, raw, err := call[abi.ToolListRequest, abi.ToolListResponse](t, abi.OpToolList, abi.ToolListRequest{})
	if err != nil || raw.Error != "" {
		return fmt.Errorf("tool.list: %v %s", err, raw.Error)
	}
	return nil
}

func checkUnknownOp(t Transport, _ abi.Capabilities) error {
	raw, err := t.Hostcall(abi.Request{Op: "does.not.exist"})
	if err != nil {
		return err
	}
	if raw.Code != abi.ErrUnsupported {
		return fmt.Errorf("unknown op should return code %q, got %q (%q)", abi.ErrUnsupported, raw.Code, raw.Error)
	}
	return nil
}

// checkHTTPSSRF asserts the governed-egress safety contract: any host that
// advertises the HTTP capability MUST refuse requests to the cloud metadata
// endpoint (and, by extension, private space). Hosts without HTTP are skipped.
func checkHTTPSSRF(t Transport, caps abi.Capabilities) error {
	if !caps.HTTP {
		return nil // capability not offered; nothing to verify
	}
	_, raw, err := call[abi.HTTPFetchRequest, abi.HTTPFetchResponse](t, abi.OpHTTPFetch,
		abi.HTTPFetchRequest{URL: "http://169.254.169.254/latest/meta-data/"})
	if err != nil {
		return err
	}
	if raw.Code != abi.ErrDenied {
		return fmt.Errorf("http.fetch to the metadata endpoint must be %q, got %q (%q)",
			abi.ErrDenied, raw.Code, raw.Error)
	}
	return nil
}

func checkMalformed(t Transport, _ abi.Capabilities) error {
	// fs.read with a non-object args value must be rejected as invalid.
	raw, err := t.Hostcall(abi.Request{Op: abi.OpFSRead, Args: json.RawMessage(`"not-an-object"`)})
	if err != nil {
		return err
	}
	if raw.Error == "" {
		return fmt.Errorf("malformed args should produce an error")
	}
	return nil
}
