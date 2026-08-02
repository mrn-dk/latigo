// Package main: tools.go is the model-facing tool surface (spec §2.3, §2.5).
//
// Latigo exposes three built-in tools to the model:
//
//   - shell — runs an allow-listed command line in the workspace. Discovery is
//     progressive: the system prompt states only that a shell exists; the
//     model enumerates available tools with tool_list and fetches full usage
//     on demand with "<name> --help" through the shell.
//   - tool_list — enumerates the command allow-list (name + one_line).
//   - finish — typed final output. Its output is validated against an optional
//     output schema (spec §2.5); calling it terminates the run.
//
// Tools are CLI binaries in the host image plus an allow-list entry (§2.3).
// There is no tool ABI, no registry, no plugin system: a tool is "a binary the
// model can run via the shell". The built-ins above are the only
// harness-internal tools.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Tool is a harness-internal tool exposed to the model.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema
	// Invoke runs the tool. The returned string is the tool-role message
	// content shown back to the model. isErr marks it as an error result.
	Invoke func(ctx context.Context, a *Agent, args json.RawMessage) (content string, isErr bool)
}

// builtinTools returns the harness-internal tools. The allow-list (CLI
// binaries) is not duplicated here — those are reached through `shell`.
func builtinTools(outputSchema *Schema) []*Tool {
	shellParams := json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "command": {"type": "string", "description": "The command line to run. Pipes and redirection are allowed; command substitution and eval are rejected."}
	  },
	  "required": ["command"],
	  "additionalProperties": false
	}`)

	var finishParams json.RawMessage
	if outputSchema != nil {
		b, _ := json.Marshal(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"output": outputSchema,
			},
			"required":             []string{"output"},
			"additionalProperties": false,
		})
		finishParams = b
	} else {
		finishParams = json.RawMessage(`{
		  "type": "object",
		  "properties": {
		    "output": {"type": "string", "description": "The final output of the task."}
		  },
		  "required": ["output"],
		  "additionalProperties": false
		}`)
	}

	return []*Tool{
		{
			Name:        "shell",
			Description: "Run an allow-listed command line in the workspace. Pipes (|) and redirection (>, >>, <) are allowed. Command substitution ($(...) and backticks) and eval are rejected. Run `tool_list` to see available commands and `<name> --help` to learn a command's usage.",
			Parameters:  shellParams,
			Invoke: func(ctx context.Context, a *Agent, args json.RawMessage) (string, bool) {
				var in struct {
					Command string `json:"command"`
				}
				if err := json.Unmarshal(args, &in); err != nil {
					return "error: invalid arguments: " + err.Error(), true
				}
				if strings.TrimSpace(in.Command) == "" {
					return "error: empty command", true
				}
				res, err := a.shell.Run(ctx, in.Command)
				if err != nil {
					return fmt.Sprintf("error: %v", err), true
				}
				out := res.Stdout
				if res.Denied != "" {
					return strings.TrimRight(res.Stderr, "\n"), true
				}
				if res.ExitCode != 0 {
					if res.Stderr != "" {
						if out != "" {
							out += "\n"
						}
						out += "[stderr]\n" + strings.TrimRight(res.Stderr, "\n")
					}
					return out + fmt.Sprintf("\n[exit %d]", res.ExitCode), true
				}
				if res.Stderr != "" && out == "" {
					return strings.TrimRight(res.Stderr, "\n"), false
				}
				return out, false
			},
		},
		{
			Name:        "tool_list",
			Description: "Enumerate the available tools (CLI binaries on the allow-list). Returns each tool's name and a one-line description. Run `<name> --help` through the shell for full usage.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			Invoke: func(ctx context.Context, a *Agent, _ json.RawMessage) (string, bool) {
				entries := a.allow.Entries()
				var b strings.Builder
				b.WriteString(fmt.Sprintf("%d tools available:\n", len(entries)))
				for _, e := range entries {
					if e.OneLine != "" {
						b.WriteString(fmt.Sprintf("- %s — %s\n", e.Name, e.OneLine))
					} else {
						b.WriteString(fmt.Sprintf("- %s\n", e.Name))
					}
				}
				return strings.TrimRight(b.String(), "\n"), false
			},
		},
		{
			Name:        "finish",
			Description: "End the task with a final output. The output must match the task's output schema. Calling finish terminates the run.",
			Parameters:  finishParams,
			Invoke: func(ctx context.Context, a *Agent, args json.RawMessage) (string, bool) {
				var in struct {
					Output json.RawMessage `json:"output"`
				}
				if err := json.Unmarshal(args, &in); err != nil {
					return "error: invalid arguments: " + err.Error(), true
				}
				if a.outputSchema != nil {
					if errs := validateValue(a.outputSchema, in.Output); len(errs) > 0 {
						a.finishInvalid = append(a.finishInvalid, errs)
						msg := "output does not match schema: " + strings.Join(errs, "; ") +
							". Correct the output and call finish again."
						return msg, true
					}
				}
				a.finishOutput = in.Output
				a.finishSet = true
				return "finished", false
			},
		},
	}
}

// toolSpecs builds the OpenAI tool specs advertised to the model.
func toolSpecs(tools []*Tool) []ToolSpec {
	specs := make([]ToolSpec, 0, len(tools))
	for _, t := range tools {
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		specs = append(specs, ToolSpec{
			Type: "function",
			Function: FuncSpec{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}
	return specs
}

// findTool returns the tool with the given name, or nil.
func findTool(tools []*Tool, name string) *Tool {
	for _, t := range tools {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// timebomb is a tiny helper for wall-clock deadline checks.
type timebomb struct {
	start    time.Time
	deadline time.Duration
}

func newTimebomb(d time.Duration) timebomb { return timebomb{start: time.Now(), deadline: d} }
func (t timebomb) remaining() time.Duration {
	return t.deadline - time.Since(t.start)
}
func (t timebomb) expired() bool {
	return t.deadline > 0 && time.Since(t.start) > t.deadline
}
