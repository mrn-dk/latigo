package host

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/mrn-dk/latigo/abi"
)

// HTTPPolicy is the host-side governance for http.fetch: the guest can only
// reach the network through this, and only within these bounds. The zero value
// denies everything (empty AllowHosts), which is the safe default.
type HTTPPolicy struct {
	// AllowHosts is a glob allowlist matched against the request host (no port).
	// Empty denies all requests. Patterns use path.Match semantics, e.g.
	// "api.github.com", "*.example.com".
	AllowHosts []string
	// DenyHosts is an explicit denylist; a match here always wins over AllowHosts.
	DenyHosts []string
	// AllowMethods is the permitted HTTP method set. Empty defaults to
	// GET, HEAD, POST.
	AllowMethods []string
	// MaxBytes is the hard cap on the response body. Zero defaults to 5 MiB.
	MaxBytes int
	// MaxRedirects caps redirect hops when the guest opts into following them.
	// Zero defaults to 5.
	MaxRedirects int
	// Timeout is the hard per-request timeout. Zero defaults to 30s.
	Timeout time.Duration
	// AllowPrivate permits requests that resolve to loopback/private/link-local
	// addresses. Default false blocks them (SSRF / metadata-endpoint defence).
	AllowPrivate bool
	// ReqHeaderAllow lists request headers the guest may set (case-insensitive).
	// Anything not listed is dropped; Authorization and Cookie are never
	// forwarded unless explicitly listed here.
	ReqHeaderAllow []string
}

const (
	defaultHTTPMaxBytes     = 5 << 20
	defaultHTTPMaxRedirects = 5
	defaultHTTPTimeout      = 30 * time.Second
)

// HTTPFetcher returns an http.fetch implementation governed by p. Register it
// with (*Host).HTTP. This is the reference governed egress; it enforces a
// scheme/method/host allowlist, blocks SSRF against private and metadata
// addresses (pinning the resolved IP to defeat DNS rebinding), caps the body
// size, and strips sensitive request headers.
func HTTPFetcher(p HTTPPolicy) func(context.Context, abi.HTTPFetchRequest) (abi.HTTPFetchResponse, error) {
	if p.MaxBytes <= 0 {
		p.MaxBytes = defaultHTTPMaxBytes
	}
	if p.MaxRedirects <= 0 {
		p.MaxRedirects = defaultHTTPMaxRedirects
	}
	if p.Timeout <= 0 {
		p.Timeout = defaultHTTPTimeout
	}
	if len(p.AllowMethods) == 0 {
		p.AllowMethods = []string{http.MethodGet, http.MethodHead, http.MethodPost}
	}

	// A dialer that re-checks every address it is about to connect to. Because
	// http.Transport resolves and then dials through this DialContext, pinning
	// the check here defeats DNS-rebinding: the IP we validate is the IP we use.
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	safeDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no addresses for %s", host)
		}
		for _, ip := range ips {
			if !p.AllowPrivate && !isPublicIP(ip.IP) {
				return nil, &CodedError{Code: abi.ErrDenied, Msg: "blocked non-public address " + ip.IP.String()}
			}
		}
		// Dial the first validated IP explicitly rather than re-resolving.
		return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}

	return func(ctx context.Context, req abi.HTTPFetchRequest) (abi.HTTPFetchResponse, error) {
		method := strings.ToUpper(strings.TrimSpace(req.Method))
		if method == "" {
			method = http.MethodGet
		}
		if !contains(p.AllowMethods, method) {
			return abi.HTTPFetchResponse{}, Errorf(abi.ErrDenied, "method not allowed: %s", method)
		}

		u, err := url.Parse(req.URL)
		if err != nil {
			return abi.HTTPFetchResponse{}, Errorf(abi.ErrInvalid, "bad url: %v", err)
		}
		if err := p.checkURL(u); err != nil {
			return abi.HTTPFetchResponse{}, err
		}

		timeout := p.Timeout
		if req.TimeoutMS > 0 && time.Duration(req.TimeoutMS)*time.Millisecond < timeout {
			timeout = time.Duration(req.TimeoutMS) * time.Millisecond
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		transport := &http.Transport{
			DialContext:           safeDial,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: timeout,
			DisableKeepAlives:     true,
		}
		client := &http.Client{
			Transport: transport,
			// Re-check policy on every redirect hop, and enforce the hop cap.
			CheckRedirect: func(r *http.Request, via []*http.Request) error {
				if !req.FollowRedirect {
					return http.ErrUseLastResponse
				}
				if len(via) >= p.MaxRedirects {
					return fmt.Errorf("too many redirects")
				}
				return p.checkURL(r.URL)
			},
		}

		var body io.Reader
		if len(req.Body) > 0 {
			body = strings.NewReader(string(req.Body))
		}
		hreq, err := http.NewRequestWithContext(ctx, method, u.String(), body)
		if err != nil {
			return abi.HTTPFetchResponse{}, Errorf(abi.ErrInvalid, "build request: %v", err)
		}
		for k, v := range req.Headers {
			if p.headerAllowed(k) {
				hreq.Header.Set(k, v)
			}
		}

		resp, err := client.Do(hreq)
		if err != nil {
			// Surface a policy denial (from safeDial/CheckRedirect) as denied.
			if code := codeOf(err); code == abi.ErrDenied {
				return abi.HTTPFetchResponse{}, Errorf(abi.ErrDenied, "%v", err)
			}
			return abi.HTTPFetchResponse{}, Errorf(abi.ErrInternal, "fetch: %v", err)
		}
		defer resp.Body.Close()

		limited := io.LimitReader(resp.Body, int64(p.MaxBytes)+1)
		data, _ := io.ReadAll(limited)
		truncated := false
		if len(data) > p.MaxBytes {
			data = data[:p.MaxBytes]
			truncated = true
		}

		out := abi.HTTPFetchResponse{
			Status:    resp.StatusCode,
			Body:      data,
			Truncated: truncated,
			FinalURL:  resp.Request.URL.String(),
			Headers:   map[string]string{},
		}
		for k := range resp.Header {
			out.Headers[k] = resp.Header.Get(k)
		}
		return out, nil
	}
}

// HTTP registers the governed http.fetch handler and advertises the HTTP
// capability. Passing nil omits the capability, in which case http.fetch
// returns "unsupported" and the guest has no network access.
func (h *Host) HTTP(fetch func(context.Context, abi.HTTPFetchRequest) (abi.HTTPFetchResponse, error)) {
	if fetch == nil {
		return
	}
	h.caps.HTTP = true
	h.Handle(abi.OpHTTPFetch, handler(fetch))
}

// checkURL validates scheme and host allow/deny for a URL (used for the initial
// request and every redirect hop). IP-level SSRF checks happen at dial time.
func (p HTTPPolicy) checkURL(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return Errorf(abi.ErrDenied, "scheme not allowed: %s", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return Errorf(abi.ErrInvalid, "missing host")
	}
	for _, d := range p.DenyHosts {
		if ok, _ := path.Match(d, host); ok {
			return Errorf(abi.ErrDenied, "host denied: %s", host)
		}
	}
	for _, a := range p.AllowHosts {
		if ok, _ := path.Match(a, host); ok {
			return nil
		}
	}
	return Errorf(abi.ErrDenied, "host not in allowlist: %s", host)
}

func (p HTTPPolicy) headerAllowed(name string) bool {
	for _, h := range p.ReqHeaderAllow {
		if strings.EqualFold(h, name) {
			return true
		}
	}
	return false
}

// isPublicIP reports whether ip is a globally routable unicast address, i.e.
// not loopback, private, link-local, ULA, multicast, or unspecified. This is
// the core SSRF defence (it also covers the 169.254.169.254 metadata endpoint,
// which is link-local).
func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	return true
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func codeOf(err error) string {
	var ce *CodedError
	if asCoded(err, &ce) {
		return ce.Code
	}
	return ""
}
