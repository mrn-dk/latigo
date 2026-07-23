package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/mrn-dk/latigo/abi"
	"github.com/mrn-dk/latigo/events"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

func isCleanExit(err error) bool {
	var ee *sys.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode() == 0
	}
	return false
}

// RunConfig configures a guest run.
type RunConfig struct {
	// Wasm is the compiled guest module bytes.
	Wasm []byte
	// Goal, Model, MaxTurns are passed to the guest via env/args.
	Goal     string
	Model    string
	MaxTurns int
	// Compaction selects the guest's transcript compaction strategy
	// ("window" or "llm"); empty uses the guest default.
	Compaction string
	// Stdout/Stderr capture the guest's process output.
	Stdout io.Writer
	Stderr io.Writer
}

// Run instantiates the guest WASM module with the ABI import wired to h and
// executes it to completion (the guest runs its agent loop inside _start).
func (h *Host) Run(ctx context.Context, cfg RunConfig) error {
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)

	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	// One-entry response cache so guest buffer-growth retries never re-execute
	// a hostcall (which would double side effects and double-log).
	var pendingReq, pendingResp []byte

	_, err := rt.NewHostModuleBuilder(abi.ImportModule).
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, reqPtr, reqLen, respPtr, respCap uint32) uint32 {
			reqBytes, ok := m.Memory().Read(reqPtr, reqLen)
			if !ok {
				return negLen()
			}
			// Copy: the guest's memory may be reused after we return.
			req := append([]byte(nil), reqBytes...)

			var respBytes []byte
			if pendingReq != nil && bytes.Equal(pendingReq, req) {
				respBytes = pendingResp
			} else {
				respBytes = h.Dispatch(ctx, req)
			}

			if uint32(len(respBytes)) <= respCap {
				m.Memory().Write(respPtr, respBytes)
				pendingReq, pendingResp = nil, nil
			} else {
				// Won't fit; remember it so the retry returns the same bytes.
				pendingReq, pendingResp = req, respBytes
			}
			return uint32(len(respBytes))
		}).
		Export(abi.ImportName).
		Instantiate(ctx)
	if err != nil {
		return fmt.Errorf("host module: %w", err)
	}

	compiled, err := rt.CompileModule(ctx, cfg.Wasm)
	if err != nil {
		return fmt.Errorf("compile guest: %w", err)
	}

	capsJSON, _ := json.Marshal(h.caps)
	modCfg := wazero.NewModuleConfig().
		WithStdout(cfg.Stdout).
		WithStderr(cfg.Stderr).
		WithArgs("latigo-guest", cfg.Goal).
		WithEnv("LATIGO_GOAL", cfg.Goal).
		WithEnv("LATIGO_MODEL", cfg.Model).
		WithEnv("LATIGO_CAPABILITIES", string(capsJSON)).
		WithEnv("LATIGO_MAX_TURNS", itoa(cfg.MaxTurns)).
		WithEnv("LATIGO_COMPACTION", cfg.Compaction)

	// run_start is the first durable event (records negotiated capabilities).
	if h.log != nil && !h.replaying {
		if _, err := h.log.Append(events.KindRunStart, events.RunStart{
			ABIVersion:   abi.Version,
			Capabilities: h.caps,
			Goal:         cfg.Goal,
		}); err != nil {
			return err
		}
	}

	// Instantiating a WASI command runs _start, i.e. the whole agent loop.
	mod, err := rt.InstantiateModule(ctx, compiled, modCfg)
	runReason, runErr := "completed", ""
	if err != nil {
		runReason, runErr = "error", err.Error()
	}
	if mod != nil {
		_ = mod.Close(ctx)
	}

	if h.log != nil && !h.replaying {
		_, _ = h.log.Append(events.KindRunEnd, events.RunEnd{Reason: runReason, Error: runErr})
	}
	if err != nil {
		// A clean guest exit(0) surfaces as sys.ExitError with code 0.
		if isCleanExit(err) {
			return nil
		}
		return fmt.Errorf("guest: %w", err)
	}
	return nil
}

func negLen() uint32 { return ^uint32(0) } // -1 as unsigned; guest treats <0 as error

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
