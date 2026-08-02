package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestShell(t *testing.T, allow *Allowlist) (*Shell, string) {
	t.Helper()
	ws := t.TempDir()
	allow.Add(AllowEntry{Name: "cat", OneLine: "print files"})
	allow.Add(AllowEntry{Name: "ls", OneLine: "list files"})
	allow.Add(AllowEntry{Name: "grep", OneLine: "search"})
	allow.Add(AllowEntry{Name: "wc", OneLine: "count"})
	allow.Add(AllowEntry{Name: "sort", OneLine: "sort lines"})
	allow.Add(AllowEntry{Name: "head", OneLine: "head"})
	allow.Add(AllowEntry{Name: "mkdir", OneLine: "make dirs"})
	allow.Add(AllowEntry{Name: "rm", OneLine: "remove"})
	allow.Add(AllowEntry{Name: "printf", OneLine: "format"}) // also a builtin; allow for clarity
	shell := NewShell(ws, allow)
	shell.Env = []string{"PATH=" + os.Getenv("PATH")}
	return shell, ws
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestShellAllowsAllowlistedCommand(t *testing.T) {
	shell, ws := newTestShell(t, NewAllowlist())
	writeFile(t, filepath.Join(ws, "a.txt"), "hello world\n")
	res, err := shell.Run(context.Background(), "cat a.txt")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit %d: %s", res.ExitCode, res.Stderr)
	}
	if res.Stdout != "hello world\n" {
		t.Fatalf("got %q", res.Stdout)
	}
}

func TestShellRejectsUnknownCommand(t *testing.T) {
	shell, _ := newTestShell(t, NewAllowlist())
	res, _ := shell.Run(context.Background(), "rg something")
	if res.Denied == "" {
		t.Fatalf("expected hard reject, got stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
	if res.ExitCode != 126 {
		t.Fatalf("expected exit 126, got %d", res.ExitCode)
	}
}

func TestShellRejectsCommandSubstitution(t *testing.T) {
	shell, _ := newTestShell(t, NewAllowlist())
	cases := []string{
		"echo $(cat a.txt)",
		"echo `cat a.txt`",
		"echo $(date)",
	}
	for _, c := range cases {
		res, _ := shell.Run(context.Background(), c)
		if res.Denied == "" {
			t.Fatalf("expected cmdsubst reject for %q, got %q %q", c, res.Stdout, res.Stderr)
		}
	}
}

func TestShellRejectsEval(t *testing.T) {
	shell, _ := newTestShell(t, NewAllowlist())
	res, _ := shell.Run(context.Background(), "eval 'cat a.txt'")
	if res.Denied == "" {
		t.Fatalf("expected eval reject, got %q %q", res.Stdout, res.Stderr)
	}
}

func TestShellRejectsDynamicCommandName(t *testing.T) {
	shell, _ := newTestShell(t, NewAllowlist())
	res, _ := shell.Run(context.Background(), "c=cat; $c a.txt")
	if res.Denied == "" {
		t.Fatalf("expected dynamic-name reject, got %q %q", res.Stdout, res.Stderr)
	}
}

func TestShellAllowsPipesAndRedirection(t *testing.T) {
	shell, ws := newTestShell(t, NewAllowlist())
	writeFile(t, filepath.Join(ws, "a.txt"), "banana\napple\ncherry\n")
	// Pipe: sort a.txt | head -1 -> "apple"
	res, _ := shell.Run(context.Background(), "sort a.txt | head -1")
	if res.ExitCode != 0 {
		t.Fatalf("pipe exit %d: %s", res.ExitCode, res.Stderr)
	}
	if strings.TrimSpace(res.Stdout) != "apple" {
		t.Fatalf("pipe got %q", res.Stdout)
	}
	// Redirection: printf writes to a file via >
	res, _ = shell.Run(context.Background(), "printf 'redirected\\n' > out.txt")
	if res.ExitCode != 0 {
		t.Fatalf("redir exit %d: %s", res.ExitCode, res.Stderr)
	}
	b, _ := os.ReadFile(filepath.Join(ws, "out.txt"))
	if string(b) != "redirected\n" {
		t.Fatalf("redir content %q", b)
	}
}

func TestShellBuiltinsRunWithoutAllowlist(t *testing.T) {
	// An empty allow-list still admits interp's pure builtins (cd, pwd, true).
	shell, ws := newTestShell(t, NewAllowlist())
	res, _ := shell.Run(context.Background(), "cd . && pwd")
	if res.ExitCode != 0 {
		t.Fatalf("exit %d: %s", res.ExitCode, res.Stderr)
	}
	abs, _ := filepath.Abs(ws)
	if strings.TrimSpace(res.Stdout) != abs {
		t.Fatalf("pwd got %q want %q", res.Stdout, abs)
	}
}

func TestShellNonZeroExitIsToolError(t *testing.T) {
	shell, _ := newTestShell(t, NewAllowlist())
	// grep with no match exits 1; the harness surfaces this so the model can react.
	res, _ := shell.Run(context.Background(), "grep nope a.txt")
	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero, got 0")
	}
}
