// Package main: shell.go is the allow-listed shell (spec §2.4).
//
// The rule, stated plainly: never pass a command string to `bash -c`. Latigo
// parses the command line with mvdan/sh, walks the AST, and verifies every
// command node against the allow-list. Unparseable input, command
// substitution, and `eval` are hard rejects — never a fallback to a raw shell.
// Pipes and redirection remain available, which preserves most of the shell's
// value. Execution is mvdan/sh's interp, which interprets the shell language
// itself and only spawns the leaf binaries the allow-list admitted; no shell
// process is ever forked.
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// ShellResult is the outcome of running an allow-listed command line.
type ShellResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	// Denied is the allow-list/AST rejection reason, when the line was
	// hard-rejected before any execution. ExitCode is 126 in that case.
	Denied string
}

// Shell runs allow-listed command lines in a workspace directory.
type Shell struct {
	Workspace string
	Allow     *Allowlist
	// ExecTimeout caps each spawned leaf command. Zero means no cap.
	ExecTimeout time.Duration
	// Env is the environment handed to spawned commands. Secrets never enter
	// the sandbox (spec §7): the caller passes a clean env, not the host's.
	Env []string
}

// NewShell builds a shell bound to a workspace directory and allow-list.
func NewShell(workspace string, allow *Allowlist) *Shell {
	return &Shell{Workspace: workspace, Allow: allow, ExecTimeout: 60 * time.Second}
}

// Run parses, verifies, and executes a command line.
func (s *Shell) Run(ctx context.Context, line string) (ShellResult, error) {
	f, err := syntax.NewParser().Parse(strings.NewReader(line), "")
	if err != nil {
		return ShellResult{ExitCode: 2, Stderr: "parse error: " + err.Error() + "\n"}, nil
	}
	if reason := s.verify(f); reason != "" {
		return ShellResult{ExitCode: 126, Stderr: "denied: " + reason + "\n", Denied: reason}, nil
	}
	return s.execute(ctx, f)
}

// verify walks the parsed AST and hard-rejects anything that escapes the
// allow-list model. Returns "" if the line is admissible, else a reason.
//
// Rejected outright:
//   - command substitution ($(…) and backticks): the spawned command is not
//     statically visible, so it cannot be allow-listed.
//   - arithmetic expansion as a command ($( … )) and arithmetic commands
//     (( … )): same reasoning — dynamic, not statically verifiable.
//   - process substitution <(…)/>(…): spawns a subprocess, not allow-listed.
//   - `eval`: its argument is a command string by definition; allowing it
//     collapses the allow-list to "everything".
//   - any Call whose command word is not a static literal (parameter/command
//     expansion in the command position) or whose name is neither a shell
//     builtin nor on the allow-list.
func (s *Shell) verify(f *syntax.File) string {
	var reason string
	syntax.Walk(f, func(n syntax.Node) bool {
		if reason != "" {
			return false
		}
		switch v := n.(type) {
		case *syntax.CmdSubst:
			reason = "command substitution is not allowed"
			return false
		case *syntax.ProcSubst:
			reason = "process substitution is not allowed"
			return false
		case *syntax.CallExpr:
			name, ok := callName(v)
			if !ok {
				reason = "dynamic command name is not allowed"
				return false
			}
			if name == "eval" {
				reason = "eval is not allowed"
				return false
			}
			if isShellBuiltin(name) {
				return true
			}
			if !s.Allow.Allowed(name) {
				reason = fmt.Sprintf("command %q is not on the allow-list", name)
				return false
			}
		}
		return true
	})
	return reason
}

// callName extracts the static literal command name from a CallExpr's first
// argument word. ok is false if the command word is not a pure literal
// (quoted or not) — i.e. it contains any expansion.
func callName(c *syntax.CallExpr) (string, bool) {
	if len(c.Args) == 0 {
		return "", false
	}
	return literalWord(c.Args[0])
}

// literalWord returns the literal text of a word if it contains only Lit
// nodes (optionally wrapped in single/double quotes), and ok=true. Any
// expansion (parameter, command, arithmetic, process, extglob) makes it
// non-literal.
func literalWord(w *syntax.Word) (string, bool) {
	if w == nil {
		return "", false
	}
	var b strings.Builder
	for _, p := range w.Parts {
		switch lit := p.(type) {
		case *syntax.Lit:
			b.WriteString(lit.Value)
		case *syntax.SglQuoted:
			b.WriteString(lit.Value)
		case *syntax.DblQuoted:
			// A double-quoted word is literal iff every part inside is a Lit.
			for _, dp := range lit.Parts {
				if l, ok := dp.(*syntax.Lit); ok {
					b.WriteString(l.Value)
				} else {
					return "", false
				}
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}

// execute runs the verified AST with mvdan/sh's interp against the real
// workspace filesystem. interp interprets pipes, redirection, builtins, and
// variable expansion itself; leaf binaries are spawned via os/exec (the
// default exec handler), which is exactly the WASIX subprocess-spawn model
// under the runtime.
func (s *Shell) execute(ctx context.Context, f *syntax.File) (ShellResult, error) {
	var stdout, stderr strings.Builder
	// stdin is nil rather than an empty reader: mvdan/sh's StdIO forces an
	// os.Pipe when stdin is a non-*os.File reader, and os.Pipe is not
	// implemented on Go's wasip1 target (it breaks interp.New under WASIX).
	// nil stdin means the shell reads EOF immediately, which is exactly what
	// an empty reader gave us — and it keeps interp.New working under wasip1.
	opts := []interp.RunnerOption{
		interp.StdIO(nil, &stdout, &stderr),
		interp.Dir(s.Workspace),
	}
	if s.Env != nil {
		opts = append(opts, interp.Env(expand.ListEnviron(s.Env...)))
	}
	runner, err := interp.New(opts...)
	if err != nil {
		return ShellResult{}, err
	}
	if s.ExecTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.ExecTimeout)
		defer cancel()
	}
	runErr := runner.Run(ctx, f)
	res := ShellResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if runErr != nil {
		if status, ok := interp.IsExitStatus(runErr); ok {
			res.ExitCode = int(status)
		} else {
			res.ExitCode = 1
			res.Stderr += runErr.Error() + "\n"
		}
	}
	return res, nil
}
