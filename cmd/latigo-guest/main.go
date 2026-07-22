// Command latigo-guest is the Latigo harness compiled to WebAssembly
// (GOOS=wasip1 GOARCH=wasm). The host instantiates it, which runs the agent
// loop to completion via write-ahead-logged hostcalls.
//
// Build:
//
//	GOOS=wasip1 GOARCH=wasm go build -o latigo.wasm ./cmd/latigo-guest
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/mrn-dk/latigo/guest"
)

// HarnessVersion stamps events emitted on behalf of this guest.
const HarnessVersion = "latigo-guest/0.0.0"

func main() {
	cfg := guest.LoadConfig()
	client := guest.NewClient(nil) // default transport = imported hostcall

	agent := guest.NewAgent(cfg, client)
	seedDefaults(agent)

	summary, err := agent.Run(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "latigo: run failed: %v\n", err)
		_ = client.LogAppend("error", "run failed", nil)
		os.Exit(1)
	}
	// Emit a final message to the host so a transcript is visible.
	_ = client.MsgSend("result", summary)
	fmt.Fprintln(os.Stdout, summary)
}

// seedDefaults populates the VFS and skills with a small starter set so the
// agent is useful out of the box.
func seedDefaults(a *guest.Agent) {
	_ = a.Skills().Seed("shell-basics", `# shell-basics
Use the bash tool to inspect and edit files under /work.
Common commands: ls, cat, grep, head, tail, wc, sort, find, mkdir, cp, mv, rm.
Redirect output with > and >>, and chain with pipes |.`)
	_ = a.VFS().WriteFile("/work/README", []byte("Latigo virtual workspace.\n"))
}
