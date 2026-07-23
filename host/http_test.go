package host

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mrn-dk/latigo/abi"
)

func TestHTTPFetchAllowlist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "ok")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("hello from " + r.Method))
	}))
	defer srv.Close()

	// The test server binds to 127.0.0.1, so we must allow private addresses to
	// exercise the happy path; the SSRF tests below cover the default-deny case.
	fetch := HTTPFetcher(HTTPPolicy{
		AllowHosts:   []string{"127.0.0.1", "localhost"},
		AllowPrivate: true,
	})
	resp, err := fetch(context.Background(), abi.HTTPFetchRequest{URL: srv.URL})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if resp.Status != 200 || !strings.Contains(string(resp.Body), "hello from GET") {
		t.Fatalf("unexpected response: %d %q", resp.Status, resp.Body)
	}
	if resp.Headers["X-Test"] != "ok" {
		t.Errorf("missing response header: %+v", resp.Headers)
	}
}

func TestHTTPFetchDenies(t *testing.T) {
	// AllowPrivate=false is the real default: private/loopback/metadata blocked.
	fetch := HTTPFetcher(HTTPPolicy{AllowHosts: []string{"*"}})

	cases := []struct {
		name string
		req  abi.HTTPFetchRequest
		code string
	}{
		{"loopback", abi.HTTPFetchRequest{URL: "http://127.0.0.1/"}, abi.ErrDenied},
		{"metadata", abi.HTTPFetchRequest{URL: "http://169.254.169.254/latest/meta-data/"}, abi.ErrDenied},
		{"private-10", abi.HTTPFetchRequest{URL: "http://10.0.0.1/"}, abi.ErrDenied},
		{"bad-scheme", abi.HTTPFetchRequest{URL: "file:///etc/passwd"}, abi.ErrDenied},
		{"bad-url", abi.HTTPFetchRequest{URL: "://nope"}, abi.ErrInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := fetch(context.Background(), c.req)
			if err == nil {
				t.Fatalf("expected denial, got nil")
			}
			if got := codeOf(err); got != c.code {
				t.Fatalf("code = %q, want %q (%v)", got, c.code, err)
			}
		})
	}
}

func TestHTTPFetchHostNotAllowed(t *testing.T) {
	fetch := HTTPFetcher(HTTPPolicy{AllowHosts: []string{"api.example.com"}})
	_, err := fetch(context.Background(), abi.HTTPFetchRequest{URL: "https://evil.example.org/"})
	if err == nil || codeOf(err) != abi.ErrDenied {
		t.Fatalf("expected denied for non-allowlisted host, got %v", err)
	}
}

func TestHTTPFetchMethodNotAllowed(t *testing.T) {
	fetch := HTTPFetcher(HTTPPolicy{AllowHosts: []string{"*"}, AllowMethods: []string{"GET"}})
	_, err := fetch(context.Background(), abi.HTTPFetchRequest{URL: "https://example.com/", Method: "DELETE"})
	if err == nil || codeOf(err) != abi.ErrDenied {
		t.Fatalf("expected denied for disallowed method, got %v", err)
	}
}

func TestHTTPFetchTruncates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 1000)))
	}))
	defer srv.Close()
	fetch := HTTPFetcher(HTTPPolicy{AllowHosts: []string{"127.0.0.1"}, AllowPrivate: true, MaxBytes: 100})
	resp, err := fetch(context.Background(), abi.HTTPFetchRequest{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Truncated || len(resp.Body) != 100 {
		t.Fatalf("expected truncated 100-byte body, got trunc=%v len=%d", resp.Truncated, len(resp.Body))
	}
}
