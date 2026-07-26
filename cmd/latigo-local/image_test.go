package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mrn-dk/latigo/abi"
	"github.com/mrn-dk/latigo/events"
	"github.com/mrn-dk/latigo/host"
)

// TestImageFlagAttachesToInitialGoal is an end-to-end check of the -image
// flag: it runs a full guest activation (offline mock LLM, real wasm module)
// with -image pointing at a small PNG fixture and -multimodal set, then reads
// back the durable event log and confirms the recorded llm.call request for
// the first turn actually carries an image content part. This is the
// "replay reconstructs the bytes verbatim" property from spec 01: the image
// must show up in the write-ahead-logged request, not just in guest memory.
func TestImageFlagAttachesToInitialGoal(t *testing.T) {
	wasm := buildGuestWasm(t)
	dir := t.TempDir()

	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3, 4, 5, 6, 7, 8}
	imgPath := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(imgPath, png, 0o644); err != nil {
		t.Fatal(err)
	}
	wasmPath := filepath.Join(dir, "g.wasm")
	if err := os.WriteFile(wasmPath, wasm, 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "run.jsonl")

	o := runOptions{
		wasmPath:   wasmPath,
		logPath:    logPath,
		root:       filepath.Join(dir, "workspace"),
		model:      "mock",
		maxTurns:   4,
		checkpoint: true,
		goal:       "look at the screenshot",
		multimodal: true,
		imagePaths: []string{imgPath},
	}
	if err := run(o); err != nil {
		t.Fatalf("run: %v", err)
	}

	evs, err := host.ReadEvents(logPath)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}

	var sawRunStartMultimodal bool
	var llmCallCount int
	var foundImagePart bool
	for _, ev := range evs {
		switch ev.Kind {
		case events.KindRunStart:
			var rs events.RunStart
			if err := json.Unmarshal(ev.Payload, &rs); err != nil {
				t.Fatalf("decode run_start: %v", err)
			}
			sawRunStartMultimodal = rs.Capabilities.Multimodal
		case events.KindHostcall:
			var hc events.Hostcall
			if err := json.Unmarshal(ev.Payload, &hc); err != nil {
				t.Fatalf("decode hostcall: %v", err)
			}
			if hc.Op != abi.OpLLMCall {
				continue
			}
			llmCallCount++
			var envelope abi.Request
			if err := json.Unmarshal(hc.Request, &envelope); err != nil {
				t.Fatalf("decode hostcall envelope: %v", err)
			}
			var req abi.LLMCallRequest
			if err := json.Unmarshal(envelope.Args, &req); err != nil {
				t.Fatalf("decode llm.call request: %v", err)
			}
			for _, m := range req.Messages {
				for _, p := range m.Parts {
					if p.Type == "image" && p.Image != nil && string(p.Image.Data) == string(png) {
						foundImagePart = true
					}
				}
			}
		}
	}
	if !sawRunStartMultimodal {
		t.Error("run_start did not record the Multimodal capability as granted")
	}
	if llmCallCount == 0 {
		t.Fatal("no llm.call hostcall was recorded")
	}
	if !foundImagePart {
		t.Error("no recorded llm.call request carried the attached image bytes verbatim")
	}
}

// TestImageFlagWithoutMultimodalDegrades verifies that without -multimodal,
// the same -image attachment never reaches the recorded llm.call request as
// an image part; it should degrade to the text placeholder instead.
func TestImageFlagWithoutMultimodalDegrades(t *testing.T) {
	wasm := buildGuestWasm(t)
	dir := t.TempDir()

	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 9, 9, 9}
	imgPath := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(imgPath, png, 0o644); err != nil {
		t.Fatal(err)
	}
	wasmPath := filepath.Join(dir, "g.wasm")
	if err := os.WriteFile(wasmPath, wasm, 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "run.jsonl")

	o := runOptions{
		wasmPath:   wasmPath,
		logPath:    logPath,
		root:       filepath.Join(dir, "workspace"),
		model:      "mock",
		maxTurns:   4,
		checkpoint: true,
		goal:       "look at the screenshot",
		// multimodal left false
		imagePaths: []string{imgPath},
	}
	if err := run(o); err != nil {
		t.Fatalf("run: %v", err)
	}

	evs, err := host.ReadEvents(logPath)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	for _, ev := range evs {
		if ev.Kind != events.KindHostcall {
			continue
		}
		var hc events.Hostcall
		if err := json.Unmarshal(ev.Payload, &hc); err != nil {
			continue
		}
		if hc.Op != abi.OpLLMCall {
			continue
		}
		var envelope abi.Request
		if err := json.Unmarshal(hc.Request, &envelope); err != nil {
			continue
		}
		var req abi.LLMCallRequest
		if err := json.Unmarshal(envelope.Args, &req); err != nil {
			continue
		}
		for _, m := range req.Messages {
			for _, p := range m.Parts {
				if p.Type == "image" {
					t.Fatalf("an image part reached a recorded llm.call request on a non-multimodal host: %+v", p)
				}
			}
		}
	}
}
