package guest

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/mrn-dk/latigo/abi"
)

// curl implements a deliberately small, explicit subset of curl (and wget) over
// the governed http.fetch capability. Unknown flags are rejected rather than
// silently ignored, because surprising curl semantics are a footgun for an LLM.
//
// Supported: -X/--request, -H/--header (repeatable), -d/--data (implies POST),
// -o/--output <file>, -i/--include, -L/--location, -s/--silent,
// --max-time <sec>, -A/--user-agent. The single positional argument is the URL.
func (b *Bash) curl(name string, c *cmdCtx) int {
	req := abi.HTTPFetchRequest{Headers: map[string]string{}}
	var (
		url          string
		outFile      string
		includeHdr   bool
		silent       bool
		explicitVerb bool
	)

	args := c.args
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, bool) {
			if i+1 >= len(args) {
				return "", false
			}
			i++
			return args[i], true
		}
		switch {
		case a == "-X" || a == "--request":
			v, ok := next()
			if !ok {
				return curlErr(c, name, 2, "option %s: requires a value", a)
			}
			req.Method = strings.ToUpper(v)
			explicitVerb = true
		case a == "-H" || a == "--header":
			v, ok := next()
			if !ok {
				return curlErr(c, name, 2, "option %s: requires a value", a)
			}
			if k, val, found := strings.Cut(v, ":"); found {
				req.Headers[strings.TrimSpace(k)] = strings.TrimSpace(val)
			}
		case a == "-d" || a == "--data":
			v, ok := next()
			if !ok {
				return curlErr(c, name, 2, "option %s: requires a value", a)
			}
			req.Body = append(req.Body, []byte(v)...)
		case a == "-o" || a == "--output":
			v, ok := next()
			if !ok {
				return curlErr(c, name, 2, "option %s: requires a value", a)
			}
			outFile = v
		case a == "-A" || a == "--user-agent":
			v, ok := next()
			if !ok {
				return curlErr(c, name, 2, "option %s: requires a value", a)
			}
			req.Headers["User-Agent"] = v
		case a == "--max-time":
			v, ok := next()
			if !ok {
				return curlErr(c, name, 2, "option %s: requires a value", a)
			}
			sec, err := strconv.Atoi(v)
			if err != nil {
				return curlErr(c, name, 2, "option --max-time: invalid number %q", v)
			}
			req.TimeoutMS = sec * 1000
		case a == "-i" || a == "--include":
			includeHdr = true
		case a == "-L" || a == "--location":
			req.FollowRedirect = true
		case a == "-s" || a == "--silent":
			silent = true
		case a == "-O" || a == "--remote-name":
			// wget-style default behaviour is unsupported; be explicit.
			return curlErr(c, name, 2, "option %s: unsupported; use -o <file>", a)
		case strings.HasPrefix(a, "-") && a != "-":
			return curlErr(c, name, 2, "option %s: is unknown", a)
		default:
			if url != "" {
				return curlErr(c, name, 2, "only one URL is supported")
			}
			url = a
		}
	}

	if url == "" {
		return curlErr(c, name, 2, "no URL specified")
	}
	req.URL = url
	if len(req.Body) > 0 && !explicitVerb {
		req.Method = "POST"
	}

	if b.fetch == nil {
		return curlErr(c, name, 7, "no network capability (the host granted no http access)")
	}

	resp, err := b.fetch.HTTPFetch(req)
	if err != nil {
		if IsUnsupported(err) {
			return curlErr(c, name, 7, "no network capability (the host granted no http access)")
		}
		return curlErr(c, name, 7, "%v", err)
	}

	var out strings.Builder
	if includeHdr {
		fmt.Fprintf(&out, "HTTP/1.1 %d\n", resp.Status)
		keys := make([]string, 0, len(resp.Headers))
		for k := range resp.Headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&out, "%s: %s\n", k, resp.Headers[k])
		}
		out.WriteString("\n")
	}

	if outFile != "" {
		body := resp.Body
		if includeHdr {
			body = append([]byte(out.String()), body...)
		}
		if err := b.vfs.WriteFile(resolve(c.dir, outFile), body); err != nil {
			return curlErr(c, name, 23, "write %s: %v", outFile, err)
		}
	} else {
		out.Write(resp.Body)
		c.stdout.Write([]byte(out.String()))
	}

	if resp.Truncated && !silent {
		fmt.Fprintf(c.stderr, "%s: note: response truncated at host byte cap\n", name)
	}
	return 0
}

func curlErr(c *cmdCtx, name string, code int, format string, a ...any) int {
	fmt.Fprintf(c.stderr, name+": "+format+"\n", a...)
	return code
}
