package host_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/mrn-dk/latigo/abi"
	"github.com/mrn-dk/latigo/events"
	"github.com/mrn-dk/latigo/host"
)

// TestCheckpointCompactionReplay runs a multi-turn guest with checkpointing on,
// compacts the log down to the tail since the last checkpoint, then replays the
// compacted log and asserts the run is reconstructed identically — proving that
// bounded replay resumes from a checkpoint rather than re-executing every turn.
func TestCheckpointCompactionReplay(t *testing.T) {
	wasm := buildGuest(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")

	// A mock LLM that issues many bash turns; once exhausted it emits a final
	// stop, ending the loop. Enough turns to cross the turn%4 checkpoint points.
	bashArgs, _ := json.Marshal(map[string]string{"script": "echo step"})
	var turns []abi.LLMMessage
	for i := 0; i < 9; i++ {
		turns = append(turns, abi.LLMMessage{
			Role:      "assistant",
			ToolCalls: []abi.LLMToolCall{{ID: fmt.Sprintf("c%d", i), Name: "bash", Arguments: string(bashArgs)}},
		})
	}

	// --- live run with checkpointing ---
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
	(&host.MockLLM{Turns: turns}).Register(h)
	h.Checkpoints(nil) // enable state.checkpoint/state.restore

	var out bytes.Buffer
	if err := h.Run(context.Background(), host.RunConfig{
		Wasm: wasm, Goal: "cp", MaxTurns: 12, Stdout: &out, Stderr: &out,
	}); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	log.Close()
	if len(sent) == 0 {
		t.Fatal("no result message sent")
	}
	liveResult := sent[len(sent)-1]

	evs, err := host.ReadEvents(logPath)
	if err != nil {
		t.Fatal(err)
	}
	nCP := 0
	for _, e := range evs {
		if e.Kind == events.KindCheckpoint {
			nCP++
		}
	}
	if nCP == 0 {
		t.Fatal("expected checkpoints to be emitted")
	}

	// --- compact ---
	removed, err := host.CompactLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if removed <= 0 {
		t.Fatalf("expected compaction to remove events, removed=%d", removed)
	}
	cevs, err := host.ReadEvents(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cevs) >= len(evs) {
		t.Fatalf("compacted log not smaller: %d >= %d", len(cevs), len(evs))
	}

	// --- replay from the compacted log ---
	rh := host.New(abi.Capabilities{FSWrite: true, Checkpoint: true}, nil)
	if err := rh.LoadReplay(cevs); err != nil {
		t.Fatal(err)
	}
	var rout bytes.Buffer
	if err := rh.Run(context.Background(), host.RunConfig{
		Wasm: wasm, Goal: "cp", MaxTurns: 12, Stdout: &rout, Stderr: &rout,
	}); err != nil {
		t.Fatalf("replay: %v\n%s", err, rout.String())
	}
	if !bytes.Contains(rout.Bytes(), []byte(liveResult)) {
		t.Fatalf("replay reconstruction mismatch: live=%q replay=%q", liveResult, rout.String())
	}
}
