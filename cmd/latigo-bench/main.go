// Command latigo-bench measures Latigo's agent spin-up performance and, when
// available, compares it against Docker container start-up.
//
// It reports three distinct phases:
//
//   - compile: the one-time cost of compiling the guest WASM to native code.
//     A real host does this once at startup and keeps the module hot.
//   - spin-up (warm): the per-agent cost of instantiating a fresh, fully
//     isolated sandbox from the already-compiled module and booting the guest
//     to a ready state (capability negotiation + first agent turn). This is the
//     number that matters when a server spins up many agents on demand.
//   - cold start: a full cold path — fresh runtime, compile, and spin-up — for
//     the very first agent after process start.
//
// The Docker baseline (`docker run --rm <image> true`) measures the cost of
// merely starting an empty container that does no useful work, which is the
// closest apples-to-apples comparison for "spin up a new isolated environment".
//
// Build & run:
//
//	make guest
//	go run ./cmd/latigo-bench -wasm latigo.wasm -n 200 -docker
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"time"

	"github.com/mrn-dk/latigo/abi"
	"github.com/mrn-dk/latigo/host"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

func main() {
	var (
		wasmPath  = flag.String("wasm", "latigo.wasm", "path to the guest wasm module")
		n         = flag.Int("n", 200, "iterations for the warm spin-up benchmark")
		coldN     = flag.Int("cold-n", 20, "iterations for the cold-start benchmark")
		dockerImg = flag.String("docker-image", "alpine", "image for the Docker baseline")
		doDocker  = flag.Bool("docker", false, "also benchmark `docker run` for comparison")
		asJSON    = flag.Bool("json", false, "emit results as JSON")
	)
	flag.Parse()

	wasm, err := os.ReadFile(*wasmPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "latigo-bench:", err)
		os.Exit(1)
	}

	ctx := context.Background()
	results := []result{}

	fmt.Fprintf(os.Stderr, "latigo-bench: wasm=%s (%s), warm n=%d, cold n=%d\n",
		*wasmPath, humanBytes(len(wasm)), *n, *coldN)

	// Phase 1: one-time compile cost (fresh compilation cache each time).
	results = append(results, benchCompile(ctx, wasm, min(*coldN, 10)))

	// Phase 2: warm per-agent spin-up (compiled module kept hot).
	results = append(results, benchWarmSpinup(ctx, wasm, *n))

	// Phase 3: cold start (fresh runtime + compile + spin-up).
	results = append(results, benchColdStart(ctx, wasm, *coldN))

	// Phase 3b: cold start with a persisted compilation cache (the compile is
	// paid once ever; later process starts reuse the cached native code).
	results = append(results, benchColdStartCached(ctx, wasm, *coldN))

	// Phase 4: Docker baseline.
	if *doDocker {
		if r, ok := benchDocker(*dockerImg, min(*coldN, 20)); ok {
			results = append(results, r)
		} else {
			fmt.Fprintln(os.Stderr, "latigo-bench: docker unavailable, skipping baseline")
		}
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(results)
		return
	}
	printTable(results)
}

// --- benchmark phases -------------------------------------------------------

func benchCompile(ctx context.Context, wasm []byte, iters int) result {
	var samples []time.Duration
	for i := 0; i < iters; i++ {
		// A fresh runtime with a fresh cache => a true cold compile.
		rt := wazero.NewRuntime(ctx)
		start := time.Now()
		if _, err := rt.CompileModule(ctx, wasm); err != nil {
			fatal("compile", err)
		}
		samples = append(samples, time.Since(start))
		_ = rt.Close(ctx)
	}
	return summarize("compile (one-time)", samples)
}

func benchWarmSpinup(ctx context.Context, wasm []byte, iters int) result {
	// A single hot runtime: WASI + host module registered once, guest compiled
	// once. Each iteration instantiates a brand-new, isolated sandbox — exactly
	// what a server does when spinning up a fresh agent on demand.
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	sl := &slot{}
	registerHostModule(ctx, rt, sl)

	compiled, err := rt.CompileModule(ctx, wasm)
	if err != nil {
		fatal("compile", err)
	}

	root, _ := os.MkdirTemp("", "latigo-bench-*")
	defer os.RemoveAll(root)

	var samples []time.Duration
	// Warm the JIT/paths once (not measured).
	spinOnce(ctx, rt, compiled, sl, root)
	for i := 0; i < iters; i++ {
		start := time.Now()
		spinOnce(ctx, rt, compiled, sl, root)
		samples = append(samples, time.Since(start))
	}
	return summarize("spin-up: warm (per agent)", samples)
}

func benchColdStart(ctx context.Context, wasm []byte, iters int) result {
	root, _ := os.MkdirTemp("", "latigo-bench-*")
	defer os.RemoveAll(root)

	var samples []time.Duration
	for i := 0; i < iters; i++ {
		start := time.Now()
		rt := wazero.NewRuntime(ctx)
		wasi_snapshot_preview1.MustInstantiate(ctx, rt)
		sl := &slot{}
		registerHostModule(ctx, rt, sl)
		compiled, err := rt.CompileModule(ctx, wasm)
		if err != nil {
			fatal("compile", err)
		}
		spinOnce(ctx, rt, compiled, sl, root)
		samples = append(samples, time.Since(start))
		_ = rt.Close(ctx)
	}
	return summarize("cold start (compile + spin-up)", samples)
}

func benchColdStartCached(ctx context.Context, wasm []byte, iters int) result {
	root, _ := os.MkdirTemp("", "latigo-bench-*")
	defer os.RemoveAll(root)
	cacheDir, _ := os.MkdirTemp("", "latigo-cache-*")
	defer os.RemoveAll(cacheDir)

	cache, err := wazero.NewCompilationCacheWithDir(cacheDir)
	if err != nil {
		fatal("cache", err)
	}
	defer cache.Close(ctx)
	rtCfg := wazero.NewRuntimeConfig().WithCompilationCache(cache)

	// Prime the on-disk cache once (not measured).
	prime := wazero.NewRuntimeWithConfig(ctx, rtCfg)
	if _, err := prime.CompileModule(ctx, wasm); err != nil {
		fatal("compile", err)
	}
	_ = prime.Close(ctx)

	var samples []time.Duration
	for i := 0; i < iters; i++ {
		start := time.Now()
		rt := wazero.NewRuntimeWithConfig(ctx, rtCfg)
		wasi_snapshot_preview1.MustInstantiate(ctx, rt)
		sl := &slot{}
		registerHostModule(ctx, rt, sl)
		compiled, err := rt.CompileModule(ctx, wasm) // served from cache
		if err != nil {
			fatal("compile", err)
		}
		spinOnce(ctx, rt, compiled, sl, root)
		samples = append(samples, time.Since(start))
		_ = rt.Close(ctx)
	}
	return summarize("cold start (cached compile)", samples)
}

func benchDocker(image string, iters int) (result, bool) {
	if _, err := exec.LookPath("docker"); err != nil {
		return result{}, false
	}
	// Pull once so image-download time is excluded from the measurement.
	_ = exec.Command("docker", "pull", image).Run()
	// Warm run (not measured).
	if err := exec.Command("docker", "run", "--rm", image, "true").Run(); err != nil {
		return result{}, false
	}
	var samples []time.Duration
	for i := 0; i < iters; i++ {
		start := time.Now()
		if err := exec.Command("docker", "run", "--rm", image, "true").Run(); err != nil {
			return result{}, false
		}
		samples = append(samples, time.Since(start))
	}
	return summarize("docker run --rm "+image+" true", samples), true
}

// --- guest spin-up wiring ---------------------------------------------------

// slot holds the currently-active host and a one-entry response cache. The host
// module's imported function dispatches through the slot, so we can register the
// module once yet give every instantiated agent its own fresh host state.
type slot struct {
	h                       *host.Host
	pendingReq, pendingResp []byte
}

func registerHostModule(ctx context.Context, rt wazero.Runtime, sl *slot) {
	_, err := rt.NewHostModuleBuilder(abi.ImportModule).
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, reqPtr, reqLen, respPtr, respCap uint32) uint32 {
			reqBytes, ok := m.Memory().Read(reqPtr, reqLen)
			if !ok {
				return ^uint32(0)
			}
			req := append([]byte(nil), reqBytes...)
			var respBytes []byte
			if sl.pendingReq != nil && bytes.Equal(sl.pendingReq, req) {
				respBytes = sl.pendingResp
			} else {
				respBytes = sl.h.Dispatch(ctx, req)
			}
			if uint32(len(respBytes)) <= respCap {
				m.Memory().Write(respPtr, respBytes)
				sl.pendingReq, sl.pendingResp = nil, nil
			} else {
				sl.pendingReq, sl.pendingResp = req, respBytes
			}
			return uint32(len(respBytes))
		}).
		Export(abi.ImportName).
		Instantiate(ctx)
	if err != nil {
		fatal("host module", err)
	}
}

// buildHost wires a minimal, fully-offline host: sandboxed FS, clock, rand,
// discarded logs/messages, an empty tool catalog, and a mock LLM that returns
// immediately. Booting the guest against it exercises capability negotiation
// and the first agent turn without any network or real work.
func buildHost(root string) *host.Host {
	h := host.New(abi.Capabilities{FSWrite: true, HostVersion: "latigo-bench/0.0.0"}, nil)
	_ = h.FS(root, true)
	h.Clock(nil)
	h.Rand(nil)
	h.Log(io.Discard)
	h.Messaging(host.Messenger{Out: func(string, string) {}})
	h.Tools(host.NewStaticTools())
	(&host.MockLLM{}).Register(h) // empty script => immediate final answer
	return h
}

func spinOnce(ctx context.Context, rt wazero.Runtime, compiled wazero.CompiledModule, sl *slot, root string) {
	sl.h = buildHost(root)
	sl.pendingReq, sl.pendingResp = nil, nil
	cfg := wazero.NewModuleConfig().
		WithName(""). // anonymous => no name clashes across instantiations
		WithStdout(io.Discard).
		WithStderr(io.Discard).
		WithArgs("latigo-guest", "benchmark boot").
		WithEnv("LATIGO_GOAL", "benchmark boot").
		WithEnv("LATIGO_MODEL", "mock").
		WithEnv("LATIGO_MAX_TURNS", "1")
	mod, err := rt.InstantiateModule(ctx, compiled, cfg)
	if err != nil && !isCleanExit(err) {
		fatal("instantiate", err)
	}
	if mod != nil {
		_ = mod.Close(ctx)
	}
}

func isCleanExit(err error) bool {
	// A clean guest exit(0) surfaces as *sys.ExitError with code 0.
	if e, ok := err.(*sys.ExitError); ok {
		return e.ExitCode() == 0
	}
	return false
}

// --- stats & reporting ------------------------------------------------------

type result struct {
	Name    string  `json:"name"`
	Samples int     `json:"samples"`
	MinMS   float64 `json:"min_ms"`
	P50MS   float64 `json:"p50_ms"`
	MeanMS  float64 `json:"mean_ms"`
	P90MS   float64 `json:"p90_ms"`
	MaxMS   float64 `json:"max_ms"`
}

func summarize(name string, samples []time.Duration) result {
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	var total time.Duration
	for _, s := range samples {
		total += s
	}
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }
	pct := func(p float64) time.Duration {
		if len(samples) == 0 {
			return 0
		}
		idx := int(p * float64(len(samples)-1))
		return samples[idx]
	}
	return result{
		Name:    name,
		Samples: len(samples),
		MinMS:   ms(samples[0]),
		P50MS:   ms(pct(0.50)),
		MeanMS:  ms(total / time.Duration(len(samples))),
		P90MS:   ms(pct(0.90)),
		MaxMS:   ms(samples[len(samples)-1]),
	}
}

func printTable(results []result) {
	fmt.Printf("\n%-34s %8s %10s %10s %10s %10s %10s\n",
		"phase", "samples", "min", "p50", "mean", "p90", "max")
	fmt.Println("  " + line(112))
	for _, r := range results {
		fmt.Printf("%-34s %8d %9.3fms %9.3fms %9.3fms %9.3fms %9.3fms\n",
			r.Name, r.Samples, r.MinMS, r.P50MS, r.MeanMS, r.P90MS, r.MaxMS)
	}
	fmt.Println()

	// Headline speedup vs Docker, if present.
	var warm, docker *result
	for i := range results {
		switch {
		case results[i].Name == "spin-up: warm (per agent)":
			warm = &results[i]
		case len(results[i].Name) >= 10 && results[i].Name[:10] == "docker run":
			docker = &results[i]
		}
	}
	if warm != nil && docker != nil && warm.P50MS > 0 {
		fmt.Printf("=> per-agent spin-up is %.0fx faster than a Docker container start (p50: %.3fms vs %.1fms)\n\n",
			docker.P50MS/warm.P50MS, warm.P50MS, docker.P50MS)
	}
}

// --- small helpers ----------------------------------------------------------

func line(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '-'
	}
	return string(b)
}

func humanBytes(n int) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := int64(n) / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "latigo-bench: %s: %v\n", what, err)
	os.Exit(1)
}
