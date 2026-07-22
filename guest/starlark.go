package guest

import (
	"fmt"
	"strings"

	"go.starlark.net/starlark"
)

// ScriptBudget bounds a Starlark script tool's resource use.
type ScriptBudget struct {
	// MaxSteps caps abstract computation steps (0 uses a default).
	MaxSteps uint64
	// MaxOutput caps captured output bytes (0 uses a default).
	MaxOutput int
}

func (b ScriptBudget) withDefaults() ScriptBudget {
	if b.MaxSteps == 0 {
		b.MaxSteps = 1_000_000
	}
	if b.MaxOutput == 0 {
		b.MaxOutput = 64 * 1024
	}
	return b
}

// ScriptRunner executes sandboxed Starlark script tools with step/output
// budgets and a small memory-style key/value store shared across invocations.
type ScriptRunner struct {
	budget ScriptBudget
	memory *starlark.Dict
}

// NewScriptRunner returns a runner with the given budget.
func NewScriptRunner(b ScriptBudget) *ScriptRunner {
	return &ScriptRunner{budget: b.withDefaults(), memory: starlark.NewDict(0)}
}

// Run executes src. The script may call print(...) to produce output and use
// memory[...] (a persistent dict) to retain state between runs. It returns the
// captured output.
func (r *ScriptRunner) Run(name, src string) (string, error) {
	var out strings.Builder
	limit := r.budget.MaxOutput

	thread := &starlark.Thread{
		Name: name,
		Print: func(_ *starlark.Thread, msg string) {
			if out.Len() < limit {
				out.WriteString(msg)
				out.WriteByte('\n')
			}
		},
	}
	thread.SetMaxExecutionSteps(r.budget.MaxSteps)

	predeclared := starlark.StringDict{
		"memory": r.memory,
	}
	if _, err := starlark.ExecFile(thread, name+".star", src, predeclared); err != nil {
		if evalErr, ok := err.(*starlark.EvalError); ok {
			return out.String(), fmt.Errorf("%s", evalErr.Backtrace())
		}
		return out.String(), err
	}
	return out.String(), nil
}
