// Command latigo-local is the reference Latigo host: a local filesystem, a
// direct OpenAI-compatible (or Mortise) LLM endpoint, a static/MCP-shaped tool
// catalog, and a JSONL event log. It instantiates the guest WASM and runs it to
// completion, or replays a prior run from its event log.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mrn-dk/latigo/abi"
	"github.com/mrn-dk/latigo/host"
)

func main() {
	var (
		wasmPath  = flag.String("wasm", "latigo.wasm", "path to the guest wasm module")
		logPath   = flag.String("log", "latigo.events.jsonl", "path to the JSONL event log")
		root      = flag.String("root", "./workspace", "host filesystem sandbox root")
		model     = flag.String("model", envOr("LATIGO_MODEL", "gpt-4o-mini"), "model name")
		baseURL   = flag.String("base-url", os.Getenv("OPENAI_BASE_URL"), "OpenAI-compatible base URL (empty uses the mock LLM)")
		apiKey    = flag.String("api-key", os.Getenv("OPENAI_API_KEY"), "API key")
		maxTurns  = flag.Int("max-turns", 16, "maximum agent turns")
		allowExec = flag.String("exec-allow", "", "comma-separated argv[0] allowlist enabling exec.run (empty disables it)")
		execNet   = flag.Bool("exec-net", false, "allow networked native exec (UNSAFE: bypasses http.fetch policy)")
		allowHTTP = flag.Bool("http", false, "enable the governed http.fetch capability")
		httpAllow = flag.String("http-allow", "", "comma-separated host globs for http.fetch (empty denies all)")
		approve   = flag.Bool("approve", false, "require interactive approval for actions")
		replay    = flag.Bool("replay", false, "replay a run from the event log instead of executing")
	)
	flag.Parse()
	goal := strings.Join(flag.Args(), " ")
	if goal == "" && !*replay {
		goal = "Explore the workspace and report what you find."
	}

	cfg := runOptions{
		wasmPath: *wasmPath, logPath: *logPath, root: *root, model: *model,
		baseURL: *baseURL, apiKey: *apiKey, goal: goal, maxTurns: *maxTurns,
		execAllow: *allowExec, execNet: *execNet, allowHTTP: *allowHTTP, httpAllow: *httpAllow,
		approve: *approve, replay: *replay,
	}
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "latigo-local:", err)
		os.Exit(1)
	}
}

// runOptions carries the CLI configuration for a run.
type runOptions struct {
	wasmPath, logPath, root, model string
	baseURL, apiKey, goal          string
	maxTurns                       int
	execAllow, httpAllow           string
	execNet, allowHTTP             bool
	approve, replay                bool
}

func run(o runOptions) error {
	wasm, err := os.ReadFile(o.wasmPath)
	if err != nil {
		return fmt.Errorf("read wasm (build with: GOOS=wasip1 GOARCH=wasm go build -o %s ./cmd/latigo-guest): %w", o.wasmPath, err)
	}

	ctx := context.Background()

	if o.replay {
		return doReplay(ctx, wasm, o.logPath, o.root, o.model, o.goal, o.maxTurns)
	}

	log, err := host.OpenEventLog(o.logPath, "latigo-local/0.0.0")
	if err != nil {
		return err
	}
	defer log.Close()

	h := host.New(abi.Capabilities{FSWrite: true, HostVersion: "latigo-local/0.0.0"}, log)
	configure(h, o)

	return h.Run(ctx, host.RunConfig{
		Wasm: wasm, Goal: o.goal, Model: o.model, MaxTurns: o.maxTurns,
		Stdout: prefixWriter("guest> "), Stderr: prefixWriter("guest! "),
	})
}

func configure(h *host.Host, o runOptions) {
	root, baseURL, apiKey, model, goal := o.root, o.baseURL, o.apiKey, o.model, o.goal
	_ = h.FS(root, true)
	h.Clock(nil)
	h.Rand(nil)
	h.Log(prefixWriter("log: "))
	h.Messaging(host.Messenger{
		Out: func(channel, content string) { fmt.Printf("msg[%s]: %s\n", channel, content) },
	})

	// Tool catalog (static; a real host could route to MCP servers here).
	tools := host.NewStaticTools()
	tools.Register(abi.ToolSpec{
		Name:        "now",
		Description: "Return the host wall-clock time as RFC3339.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(ctx context.Context, _ json.RawMessage) (json.RawMessage, bool, error) {
		return json.RawMessage(`"` + nowRFC3339() + `"`), false, nil
	})
	h.Tools(tools)

	if baseURL != "" {
		h.LLM(host.NewLLMClient(baseURL, apiKey, model))
	} else {
		fmt.Fprintln(os.Stderr, "latigo-local: no --base-url set; using the deterministic mock LLM")
		host.ScriptedMockLLM(goal).Register(h)
	}

	// Governed HTTP egress: the single sanctioned path to the network.
	if o.allowHTTP {
		h.HTTP(host.HTTPFetcher(host.HTTPPolicy{
			AllowHosts:   splitList(o.httpAllow),
			AllowMethods: []string{"GET", "HEAD", "POST"},
			MaxBytes:     5 << 20,
			Timeout:      15 * time.Second,
		}))
		if strings.TrimSpace(o.httpAllow) == "" {
			fmt.Fprintln(os.Stderr, "latigo-local: -http set with empty -http-allow; all requests will be denied")
		}
	}

	// Ambient escalation: exec.run runs native host binaries. Deny-by-default;
	// only enabled with an explicit command allowlist, and network-isolated
	// unless -exec-net is set.
	if allow := splitList(o.execAllow); len(allow) > 0 {
		h.Exec(host.LocalExec(host.ExecPolicy{
			AllowCommands: allow,
			Network:       o.execNet,
			Dir:           root,
			Timeout:       30 * time.Second,
		}))
		msg := "latigo-local: exec.run ENABLED (ambient) for: " + strings.Join(allow, ", ")
		if o.execNet {
			msg += " — WITH NETWORK (bypasses http.fetch policy)"
		}
		fmt.Fprintln(os.Stderr, msg)
	}

	if o.approve {
		h.Approval(interactiveApproval)
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func doReplay(ctx context.Context, wasm []byte, logPath, root, model, goal string, maxTurns int) error {
	evs, err := host.ReadEvents(logPath)
	if err != nil {
		return err
	}
	// In replay we still need a run config; the goal comes from run_start.
	for _, ev := range evs {
		if ev.Kind == "run_start" {
			var rs struct {
				Goal string `json:"goal"`
			}
			_ = json.Unmarshal(ev.Payload, &rs)
			if rs.Goal != "" {
				goal = rs.Goal
			}
		}
	}
	h := host.New(abi.Capabilities{FSWrite: true}, nil)
	// No real side-effecting handlers are needed; replay returns recorded results.
	if err := h.LoadReplay(evs); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "latigo-local: replaying %d events\n", len(evs))
	return h.Run(ctx, host.RunConfig{
		Wasm: wasm, Goal: goal, Model: model, MaxTurns: maxTurns,
		Stdout: prefixWriter("replay> "), Stderr: prefixWriter("replay! "),
	})
}

func interactiveApproval(action string, details json.RawMessage) (bool, string) {
	fmt.Printf("approve %q %s ? [y/N] ", action, string(details))
	sc := bufio.NewScanner(os.Stdin)
	if sc.Scan() {
		ans := strings.TrimSpace(strings.ToLower(sc.Text()))
		if ans == "y" || ans == "yes" {
			return true, "approved by operator"
		}
	}
	return false, "denied by operator"
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
