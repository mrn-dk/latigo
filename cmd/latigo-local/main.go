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
	"path/filepath"
	"strings"
	"time"

	"github.com/mrn-dk/latigo/abi"
	"github.com/mrn-dk/latigo/events"
	"github.com/mrn-dk/latigo/host"
)

func main() {
	var (
		wasmPath   = flag.String("wasm", "latigo.wasm", "path to the guest wasm module")
		logPath    = flag.String("log", "latigo.events.jsonl", "path to the JSONL event log")
		root       = flag.String("root", "./workspace", "host filesystem sandbox root")
		model      = flag.String("model", envOr("LATIGO_MODEL", "gpt-4o-mini"), "model name")
		baseURL    = flag.String("base-url", os.Getenv("OPENAI_BASE_URL"), "OpenAI-compatible base URL (empty uses the mock LLM)")
		apiKey     = flag.String("api-key", os.Getenv("OPENAI_API_KEY"), "API key")
		maxTurns   = flag.Int("max-turns", 16, "maximum agent turns")
		allowExec  = flag.String("exec-allow", "", "comma-separated argv[0] allowlist enabling exec.run (empty disables it)")
		execNet    = flag.Bool("exec-net", false, "allow networked native exec (UNSAFE: bypasses http.fetch policy)")
		allowHTTP  = flag.Bool("http", false, "enable the governed http.fetch capability")
		httpAllow  = flag.String("http-allow", "", "comma-separated host globs for http.fetch (empty denies all)")
		approve    = flag.Bool("approve", false, "require interactive approval for actions")
		replay     = flag.Bool("replay", false, "replay a run from the event log instead of executing")
		checkpoint = flag.Bool("checkpoint", true, "enable state.checkpoint/state.restore (durable snapshots)")
		compact    = flag.Bool("compact", false, "compact the event log to the last checkpoint, then exit")
		subagents  = flag.Bool("subagents", false, "expose a host-orchestrated 'delegate' subagent tool")
		maxDepth   = flag.Int("max-depth", 2, "maximum subagent nesting depth")
	)
	flag.Parse()
	goal := strings.Join(flag.Args(), " ")
	if goal == "" && !*replay && !*compact {
		goal = "Explore the workspace and report what you find."
	}

	cfg := runOptions{
		wasmPath: *wasmPath, logPath: *logPath, root: *root, model: *model,
		baseURL: *baseURL, apiKey: *apiKey, goal: goal, maxTurns: *maxTurns,
		execAllow: *allowExec, execNet: *execNet, allowHTTP: *allowHTTP, httpAllow: *httpAllow,
		approve: *approve, replay: *replay, checkpoint: *checkpoint, compact: *compact,
		subagents: *subagents, maxDepth: *maxDepth,
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
	maxTurns, maxDepth             int
	execAllow, httpAllow           string
	execNet, allowHTTP             bool
	approve, replay                bool
	checkpoint, compact, subagents bool
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
	if o.compact {
		removed, err := host.CompactLog(o.logPath)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "latigo-local: compacted %s, removed %d events\n", o.logPath, removed)
		return nil
	}

	log, err := host.OpenEventLog(o.logPath, "latigo-local/0.0.0")
	if err != nil {
		return err
	}
	defer log.Close()

	h := host.New(abi.Capabilities{FSWrite: true, HostVersion: "latigo-local/0.0.0"}, log)
	configureHost(h, wasm, o, 0, nil)

	return h.Run(ctx, host.RunConfig{
		Wasm: wasm, Goal: o.goal, Model: o.model, MaxTurns: o.maxTurns,
		Stdout: prefixWriter("guest> "), Stderr: prefixWriter("guest! "),
	})
}

// configureHost wires all handlers onto h for a run at the given subagent depth.
// When resultOut is non-nil, guest "result" messages are captured into it (used
// by the delegate tool to harvest a subagent's answer); at depth 0 messages are
// also printed. wasm is needed so the delegate tool can spin up child guests.
func configureHost(h *host.Host, wasm []byte, o runOptions, depth int, resultOut *string) {
	root, baseURL, apiKey, model, goal := o.root, o.baseURL, o.apiKey, o.model, o.goal
	_ = h.FS(root, true)
	h.Clock(nil)
	h.Rand(nil)
	if depth == 0 {
		h.Log(prefixWriter("log: "))
	} else {
		h.Log(discardWriter{})
	}
	h.Messaging(host.Messenger{
		Out: func(channel, content string) {
			if depth == 0 {
				fmt.Printf("msg[%s]: %s\n", channel, content)
			}
			if resultOut != nil && channel == "result" {
				*resultOut = content
			}
		},
	})

	if o.checkpoint {
		h.Checkpoints(nil) // fresh run: no resume state; enables durable snapshots
	}

	// Tool catalog (static; a real host could route to MCP servers here).
	tools := host.NewStaticTools()
	tools.Register(abi.ToolSpec{
		Name:        "now",
		Description: "Return the host wall-clock time as RFC3339.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(ctx context.Context, _ json.RawMessage) (json.RawMessage, bool, error) {
		return json.RawMessage(`"` + nowRFC3339() + `"`), false, nil
	})
	// Host-orchestrated subagents: delegate spins up a fresh, isolated child
	// guest to completion and returns its result. Registered only below the
	// depth limit so recursion is bounded. Orchestration lives in the host, not
	// the ABI; the child's result is recorded as this tool's invoke result, so
	// it is durable and replay-safe without re-running the child.
	if o.subagents && depth < o.maxDepth {
		tools.Register(abi.ToolSpec{
			Name:        "delegate",
			Description: "Delegate a self-contained subtask to a fresh subagent and return its final summary. Use for independent research or work that benefits from its own context.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"goal":{"type":"string","description":"the subtask for the subagent"}},"required":["goal"]}`),
		}, func(ctx context.Context, args json.RawMessage) (json.RawMessage, bool, error) {
			var in struct {
				Goal string `json:"goal"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, true, err
			}
			if strings.TrimSpace(in.Goal) == "" {
				return nil, true, fmt.Errorf("delegate: empty goal")
			}
			result, err := runSubagent(ctx, wasm, o, in.Goal, depth+1)
			if err != nil {
				return nil, true, err
			}
			b, _ := json.Marshal(result)
			return b, false, nil
		})
	}
	h.Tools(tools)

	if baseURL != "" {
		h.LLM(host.NewLLMClient(baseURL, apiKey, model))
	} else {
		if depth == 0 {
			fmt.Fprintln(os.Stderr, "latigo-local: no --base-url set; using the deterministic mock LLM")
		}
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

// runSubagent spins up a fresh, isolated child guest to run goal to completion
// and returns its final "result" message. The child has no event log of its own
// (nil log): its outcome is captured by the parent as the delegate tool's
// result, which the parent records — so subagents are durable and replay-safe
// without re-running them.
func runSubagent(ctx context.Context, wasm []byte, o runOptions, goal string, depth int) (string, error) {
	h := host.New(abi.Capabilities{FSWrite: true, HostVersion: "latigo-local/0.0.0"}, nil)
	var result string
	so := o
	so.goal = goal                                                 // drives the child's mock LLM
	so.root = filepath.Join(o.root, fmt.Sprintf(".sub-%d", depth)) // isolated workspace
	configureHost(h, wasm, so, depth, &result)
	err := h.Run(ctx, host.RunConfig{
		Wasm: wasm, Goal: goal, Model: o.model, MaxTurns: o.maxTurns,
		Stdout: discardWriter{}, Stderr: discardWriter{},
	})
	return result, err
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

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
	// In replay we still need a run config; the goal and the capabilities the
	// guest ran with come from run_start. The capabilities matter: the guest
	// only issues state.restore/state.checkpoint when it ran with Checkpoint, so
	// the replay host must advertise the same set to reproduce the hostcalls.
	caps := abi.Capabilities{FSWrite: true}
	for _, ev := range evs {
		if ev.Kind == events.KindRunStart {
			var rs events.RunStart
			_ = json.Unmarshal(ev.Payload, &rs)
			if rs.Goal != "" {
				goal = rs.Goal
			}
			caps.Checkpoint = rs.Capabilities.Checkpoint
		}
	}
	h := host.New(caps, nil)
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
