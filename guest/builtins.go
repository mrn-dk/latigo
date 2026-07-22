package guest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// registerBuiltins adds the in-guest tools (bash, file access, skills, scripts)
// to the registry.
func (a *Agent) registerBuiltins() {
	r := a.tools

	r.Add(Tool{
		Name:        "bash",
		Description: "Run a shell script in the virtual filesystem. Supports a coreutils subset (echo, cat, ls, grep, head, tail, wc, sort, find, mkdir, rm, cp, mv, touch, tee, ...), pipes, redirects, and control flow.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"script":{"type":"string","description":"the shell script to run"},"cwd":{"type":"string","description":"working directory (default /work)"}},"required":["script"]}`),
		Invoke: func(ctx context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Script string `json:"script"`
				Cwd    string `json:"cwd"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			res, err := a.bash.Run(ctx, in.Script, in.Cwd)
			if err != nil {
				return "", err
			}
			var b strings.Builder
			if res.Stdout != "" {
				b.WriteString(res.Stdout)
			}
			if res.Stderr != "" {
				b.WriteString(res.Stderr)
			}
			fmt.Fprintf(&b, "\n[exit %d]", res.ExitCode)
			return b.String(), nil
		},
	})

	r.Add(Tool{
		Name:        "read_file",
		Description: "Read a file from the virtual filesystem.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		Invoke: func(ctx context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			data, err := a.vfs.ReadFile(in.Path)
			if err != nil {
				return "", err
			}
			return string(data), nil
		},
	})

	r.Add(Tool{
		Name:        "write_file",
		Description: "Write (or overwrite) a file in the virtual filesystem.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
		Invoke: func(ctx context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			if err := a.vfs.WriteFile(in.Path, []byte(in.Content)); err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path), nil
		},
	})

	r.Add(Tool{
		Name:        "list_skills",
		Description: "List available on-demand skills (markdown playbooks).",
		Schema:      json.RawMessage(`{"type":"object","properties":{}}`),
		Invoke: func(ctx context.Context, _ json.RawMessage) (string, error) {
			skills := a.skills.List()
			if len(skills) == 0 {
				return "no skills available", nil
			}
			var b strings.Builder
			for _, s := range skills {
				fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
			}
			return b.String(), nil
		},
	})

	r.Add(Tool{
		Name:        "read_skill",
		Description: "Read the full markdown of a named skill.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
		Invoke: func(ctx context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			return a.skills.Read(in.Name)
		},
	})

	r.Add(Tool{
		Name:        "run_script",
		Description: "Run a sandboxed Starlark script with step and output budgets. Use print(...) for output and the persistent memory dict for state.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"source":{"type":"string"}},"required":["source"]}`),
		Invoke: func(ctx context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Source string `json:"source"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			out, err := a.script.Run("tool", in.Source)
			if err != nil {
				return out + "\nerror: " + err.Error(), nil
			}
			return out, nil
		},
	})

	r.Add(Tool{
		Name:        "done",
		Description: "Signal that the goal is complete. Provide a final summary.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`),
		Invoke: func(ctx context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Summary string `json:"summary"`
			}
			_ = json.Unmarshal(args, &in)
			a.done = true
			a.summary = in.Summary
			return "acknowledged", nil
		},
	})
}
