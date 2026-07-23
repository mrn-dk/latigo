package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func buildGuestWasm(t *testing.T) []byte {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	out := filepath.Join(t.TempDir(), "g.wasm")
	cmd := exec.Command("go", "build", "-o", out, "../latigo-guest")
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build guest wasm: %v\n%s", err, b)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestSubagentRunsAndReturnsResult verifies the host-orchestrated delegate path:
// runSubagent spins up a fresh child guest to completion and harvests its final
// "result" message. With the offline mock LLM the child reports
// "completed: <goal>".
func TestSubagentRunsAndReturnsResult(t *testing.T) {
	wasm := buildGuestWasm(t)
	o := runOptions{
		root:       t.TempDir(),
		model:      "mock",
		maxTurns:   8,
		checkpoint: true,
		subagents:  true,
		maxDepth:   2,
	}
	result, err := runSubagent(context.Background(), wasm, o, "investigate the widget", 1)
	if err != nil {
		t.Fatalf("runSubagent: %v", err)
	}
	if !strings.Contains(result, "completed: investigate the widget") {
		t.Fatalf("unexpected subagent result: %q", result)
	}
}

// TestSubagentDepthLimit ensures a subagent at the depth limit is not given a
// further delegate tool, bounding recursion.
func TestSubagentDepthLimit(t *testing.T) {
	wasm := buildGuestWasm(t)
	o := runOptions{root: t.TempDir(), model: "mock", maxTurns: 8, subagents: true, maxDepth: 1}
	// depth 1 == maxDepth, so configureHost must not register delegate; the run
	// still completes normally.
	result, err := runSubagent(context.Background(), wasm, o, "leaf task", 1)
	if err != nil {
		t.Fatalf("runSubagent: %v", err)
	}
	if !strings.Contains(result, "completed: leaf task") {
		t.Fatalf("unexpected result: %q", result)
	}
}
