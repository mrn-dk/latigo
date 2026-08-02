// Command latigo is the agent harness, a WASIX program (spec §2.1). It does
// not implement sandboxing; it runs inside one. Compiled to WASM it is
// executed by a WASIX-capable runtime (Wasmer) using only standard facilities
// — sockets (to the inference endpoint and the control plane), the mounted workspace
// filesystem, and subprocess spawn for tools. There are no custom host
// imports:
//
//	wasmer run latigo.wasm --dir /workspace --net ...
//
// On the single-host development path the same program runs natively against a
// local workspace directory and a local endpoint, which is exactly the runtime
// role with the OS as the sandbox boundary.
//
//	latigo "list the files under the workspace and report what you find"
//	LATIGO_RESUME=1 latigo            # continue the last run from its log
package main

import (
	"context"
	"fmt"
	"os"
	"time"
)

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "latigo:", err)
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "latigo: %v\n", err)
		os.Exit(1)
	}
}

func run(cfg Config) error {
	allow, err := loadAllowlist(cfg)
	if err != nil {
		return err
	}
	outputSchema, err := loadOutputSchema(cfg)
	if err != nil {
		return err
	}
	log, err := OpenEventLog(cfg.EventLog)
	if err != nil {
		return fmt.Errorf("open event log: %w", err)
	}
	defer log.Close()

	llm := &LLMClient{BaseURL: cfg.LLMBaseURL, APIKey: cfg.APIKey, Model: cfg.Model}
	shell := NewShell(cfg.Workspace, allow)
	shell.ExecTimeout = time.Duration(cfg.ShellExecTimeoutSeconds) * time.Second
	// A clean environment for spawned commands: only PATH and HOME plus an
	// explicit marker. No host secrets leak into the sandbox (spec §7:
	// secrets never enter the sandbox).
	shell.Env = cleanChildEnv()

	agent := NewAgent(cfg, llm, shell, allow, log, outputSchema)
	output, _, err := agent.Run(context.Background())
	if err != nil {
		return err
	}
	if output != "" {
		fmt.Println(output)
	}
	return nil
}

// loadAllowlist resolves the command allow-list from a file or a CSV env var.
// With neither set, the agent has only its built-in tools (shell with an empty
// allow-list, tool_list, finish): it can reason and finish, but spawn no
// external binary.
func loadAllowlist(cfg Config) (*Allowlist, error) {
	if cfg.AllowlistPath != "" {
		return LoadAllowlist(cfg.AllowlistPath)
	}
	if cfg.AllowCSV != "" {
		return AllowlistFromNames(cfg.AllowCSV), nil
	}
	return NewAllowlist(), nil
}

// loadOutputSchema reads the optional finish-tool output schema.
func loadOutputSchema(cfg Config) (*Schema, error) {
	if cfg.OutputSchemaPath == "" {
		return nil, nil
	}
	data, err := os.ReadFile(cfg.OutputSchemaPath)
	if err != nil {
		return nil, fmt.Errorf("read output schema: %w", err)
	}
	return parseSchema(data)
}

// cleanChildEnv builds a minimal environment for spawned tool commands.
func cleanChildEnv() []string {
	var env []string
	if v := os.Getenv("PATH"); v != "" {
		env = append(env, "PATH="+v)
	}
	if v := os.Getenv("HOME"); v != "" {
		env = append(env, "HOME="+v)
	}
	env = append(env, "LATIGO_SANDBOX=1")
	return env
}
