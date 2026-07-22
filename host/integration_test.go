package host_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/mrn-dk/latigo/abi"
	"github.com/mrn-dk/latigo/events"
	"github.com/mrn-dk/latigo/host"
)

// buildGuest compiles the guest to wasm into a temp file, or skips.
func buildGuest(t *testing.T) []byte {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	out := filepath.Join(t.TempDir(), "latigo.wasm")
	cmd := exec.Command("go", "build", "-o", out, "../cmd/latigo-guest")
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

// TestGuestRunAndReplay runs the real WASM guest end-to-end against a mock LLM,
// then replays the recorded event log and asserts the reconstruction matches.
func TestGuestRunAndReplay(t *testing.T) {
	wasm := buildGuest(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")

	// --- live run ---
	log, err := host.OpenEventLog(logPath, "test")
	if err != nil {
		t.Fatal(err)
	}
	h := host.New(abi.Capabilities{FSWrite: true}, log)
	if err := h.FS(filepath.Join(dir, "ws"), true); err != nil {
		t.Fatal(err)
	}
	h.Clock(nil)
	h.Rand(nil)
	h.Log(discard{})
	var sent []string
	h.Messaging(host.Messenger{Out: func(_, c string) { sent = append(sent, c) }})
	h.Tools(host.NewStaticTools())
	host.ScriptedMockLLM("integration").Register(h)

	var stdout bytes.Buffer
	if err := h.Run(context.Background(), host.RunConfig{
		Wasm: wasm, Goal: "integration", MaxTurns: 8, Stdout: &stdout, Stderr: &stdout,
	}); err != nil {
		t.Fatalf("run: %v\n%s", err, stdout.String())
	}
	log.Close()

	if len(sent) == 0 || sent[len(sent)-1] != "completed: integration" {
		t.Fatalf("unexpected result message: %v", sent)
	}

	// The log must contain run_start, hostcalls, and run_end.
	evs, err := host.ReadEvents(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var haveStart, haveEnd, haveHostcall bool
	for _, ev := range evs {
		switch ev.Kind {
		case events.KindRunStart:
			haveStart = true
		case events.KindRunEnd:
			haveEnd = true
		case events.KindHostcall:
			haveHostcall = true
		}
	}
	if !haveStart || !haveEnd || !haveHostcall {
		t.Fatalf("log missing required events: start=%v end=%v hostcall=%v", haveStart, haveEnd, haveHostcall)
	}

	// --- replay: no real handlers, results come from the log ---
	rh := host.New(abi.Capabilities{FSWrite: true}, nil)
	if err := rh.LoadReplay(evs); err != nil {
		t.Fatal(err)
	}
	// Replay returns recorded results without invoking handlers, so the guest
	// reconstructs the same state and prints the same summary to stdout.
	var rout bytes.Buffer
	if err := rh.Run(context.Background(), host.RunConfig{
		Wasm: wasm, Goal: "integration", MaxTurns: 8, Stdout: &rout, Stderr: &rout,
	}); err != nil {
		t.Fatalf("replay: %v\n%s", err, rout.String())
	}
	if !bytes.Contains(rout.Bytes(), []byte("completed: integration")) {
		t.Fatalf("replay produced different result: %q", rout.String())
	}
}
