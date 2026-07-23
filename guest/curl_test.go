package guest

import (
	"strings"
	"testing"

	"github.com/mrn-dk/latigo/abi"
)

// fakeFetcher records the last request and returns a canned response.
type fakeFetcher struct {
	last abi.HTTPFetchRequest
	resp abi.HTTPFetchResponse
	err  error
}

func (f *fakeFetcher) HTTPFetch(req abi.HTTPFetchRequest) (abi.HTTPFetchResponse, error) {
	f.last = req
	return f.resp, f.err
}

func TestCurlGet(t *testing.T) {
	f := &fakeFetcher{resp: abi.HTTPFetchResponse{Status: 200, Body: []byte("pong")}}
	b := NewBash(NewVFS(), f)
	res := run(t, b, `curl -s https://api.example.com/ping`)
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if res.Stdout != "pong" {
		t.Errorf("stdout=%q", res.Stdout)
	}
	if f.last.Method != "" && f.last.Method != "GET" {
		t.Errorf("method=%q, want GET/empty", f.last.Method)
	}
	if f.last.URL != "https://api.example.com/ping" {
		t.Errorf("url=%q", f.last.URL)
	}
}

func TestCurlPostWithDataAndHeader(t *testing.T) {
	f := &fakeFetcher{resp: abi.HTTPFetchResponse{Status: 201, Body: []byte("created")}}
	b := NewBash(NewVFS(), f)
	res := run(t, b, `curl -H "Authorization: Bearer x" -d '{"a":1}' https://api.example.com/things`)
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if f.last.Method != "POST" {
		t.Errorf("method=%q, want POST (implied by -d)", f.last.Method)
	}
	if string(f.last.Body) != `{"a":1}` {
		t.Errorf("body=%q", f.last.Body)
	}
	if f.last.Headers["Authorization"] != "Bearer x" {
		t.Errorf("headers=%+v", f.last.Headers)
	}
}

func TestCurlOutputToVFS(t *testing.T) {
	f := &fakeFetcher{resp: abi.HTTPFetchResponse{Status: 200, Body: []byte("filedata")}}
	vfs := NewVFS()
	b := NewBash(vfs, f)
	res := run(t, b, `curl -s -o /work/out.txt https://api.example.com/data`)
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if res.Stdout != "" {
		t.Errorf("stdout should be empty with -o, got %q", res.Stdout)
	}
	data, err := vfs.ReadFile("/work/out.txt")
	if err != nil || string(data) != "filedata" {
		t.Errorf("file contents = %q (%v)", data, err)
	}
}

func TestCurlNoCapability(t *testing.T) {
	b := NewBash(NewVFS(), nil) // no fetcher => no network capability
	res := run(t, b, `curl https://api.example.com/`)
	if res.ExitCode != 7 {
		t.Fatalf("exit=%d, want 7 (stderr %q)", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "no network capability") {
		t.Errorf("stderr=%q", res.Stderr)
	}
}

func TestCurlUnknownFlag(t *testing.T) {
	b := NewBash(NewVFS(), &fakeFetcher{})
	res := run(t, b, `curl --frobnicate https://api.example.com/`)
	if res.ExitCode != 2 {
		t.Fatalf("exit=%d, want 2 (stderr %q)", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "is unknown") {
		t.Errorf("stderr=%q", res.Stderr)
	}
}
