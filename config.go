// Package main: config.go assembles the run configuration. Latigo is a WASIX
// program; its capabilities are runtime grants (preopened dirs, a network
// allow-list, a command allow-list) applied by the orchestrator at instance
// time — not functions Latigo implements (spec §2.2). The configuration here
// is the ordinary program configuration (model, endpoint, limits, paths) that
// the instance is launched with.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the run configuration.
type Config struct {
	// Goal is the task. Set by the first positional arg or LATIGO_GOAL.
	Goal string
	// Model is the model name passed to the endpoint.
	Model string
	// LLMBaseURL is the OpenAI-compatible base URL of the inference endpoint
	// (e.g. "http://localhost:8080/v1"). Any OpenAI-compatible endpoint works.
	LLMBaseURL string
	// APIKey is the bearer token for the endpoint. Secrets never enter the
	// sandbox in the full architecture (the orchestrator injects credentials);
	// for the single-host dev path this is read from the environment.
	APIKey string
	// Workspace is the mounted workspace directory (preopened, rw).
	Workspace string
	// EventLog is the path to the append-only JSONL event log.
	EventLog string
	// AllowlistPath is the path to the command allow-list JSON.
	AllowlistPath string
	// AllowCSV is a comma-separated command allow-list (alternative to a file).
	AllowCSV string
	// OutputSchemaPath is an optional JSON Schema for the finish tool's output.
	OutputSchemaPath string
	// Resume continues an existing run by loading its transcript.
	Resume bool
	// Compaction selects the transcript compaction strategy: "window" (default,
	// deterministic) or "llm" (model-driven summarisation).
	Compaction string
	// Stream enables streaming chat completions (stream: true) with text
	// deltas forwarded to DeltaSink as they arrive. Off by default; when on, the
	// assembled message is still the unit of transcript and logging.
	Stream bool
	// SystemPrompt is a full override of the built-in default system prompt.
	// Empty means unset — the default is used. Resolved from LATIGO_SYSTEM_PROMPT
	// (inline, takes precedence) or LATIGO_SYSTEM_PROMPT_FILE (file contents).
	SystemPrompt string
	// AppendSystemPrompt is text appended after the system prompt (default or
	// override), separated by a blank line. Empty means no append. Resolved from
	// LATIGO_APPEND_SYSTEM_PROMPT (inline, takes precedence) or
	// LATIGO_APPEND_SYSTEM_PROMPT_FILE (file contents).
	AppendSystemPrompt string

	// Usage limits (spec §2.5), enforced in-loop.
	MaxTurns            int
	MaxTotalTokens      int
	MaxToolInvocations  int
	MaxWallClockSeconds int

	// ShellExecTimeoutSeconds caps each spawned leaf command.
	ShellExecTimeoutSeconds int
}

// Defaults keep the harness useful out of the box on the single-host dev path.
const (
	defaultModel            = "gpt-4o-mini"
	defaultLLMBaseURL       = "http://localhost:8080/v1"
	defaultWorkspace        = "./workspace"
	defaultEventLog         = "./latigo.events.jsonl"
	defaultMaxTurns         = 16
	defaultMaxWallClock     = 1800 // 30 min
	defaultShellExecTimeout = 60
)

// envNames are the environment variables Latigo reads.
const (
	EnvGoal                   = "LATIGO_GOAL"
	EnvModel                  = "LATIGO_MODEL"
	EnvLLMBaseURL             = "LATIGO_LLM_BASE_URL"
	EnvAPIKey                 = "LATIGO_API_KEY"
	EnvWorkspace              = "LATIGO_WORKSPACE"
	EnvEventLog               = "LATIGO_EVENT_LOG"
	EnvAllowlist              = "LATIGO_ALLOWLIST"
	EnvAllow                  = "LATIGO_ALLOW"
	EnvOutputSchema           = "LATIGO_OUTPUT_SCHEMA"
	EnvResume                 = "LATIGO_RESUME"
	EnvCompaction             = "LATIGO_COMPACTION"
	EnvStream                 = "LATIGO_STREAM"
	EnvSystemPrompt           = "LATIGO_SYSTEM_PROMPT"
	EnvSystemPromptFile       = "LATIGO_SYSTEM_PROMPT_FILE"
	EnvAppendSystemPrompt     = "LATIGO_APPEND_SYSTEM_PROMPT"
	EnvAppendSystemPromptFile = "LATIGO_APPEND_SYSTEM_PROMPT_FILE"
	EnvMaxTurns               = "LATIGO_MAX_TURNS"
	EnvMaxTokens              = "LATIGO_MAX_TOTAL_TOKENS"
	EnvMaxTools               = "LATIGO_MAX_TOOL_INVOCATIONS"
	EnvMaxWallClock           = "LATIGO_MAX_WALL_CLOCK_S"
	EnvShellTimeout           = "LATIGO_SHELL_EXEC_TIMEOUT_S"
)

// LoadConfig reads configuration from the environment, with a positional
// argument overriding the goal env var.
func LoadConfig() (Config, error) {
	cfg := Config{
		Goal:                    os.Getenv(EnvGoal),
		Model:                   os.Getenv(EnvModel),
		LLMBaseURL:              os.Getenv(EnvLLMBaseURL),
		APIKey:                  os.Getenv(EnvAPIKey),
		Workspace:               os.Getenv(EnvWorkspace),
		EventLog:                os.Getenv(EnvEventLog),
		AllowlistPath:           os.Getenv(EnvAllowlist),
		AllowCSV:                os.Getenv(EnvAllow),
		OutputSchemaPath:        os.Getenv(EnvOutputSchema),
		Compaction:              os.Getenv(EnvCompaction),
		MaxTurns:                defaultMaxTurns,
		MaxWallClockSeconds:     defaultMaxWallClock,
		ShellExecTimeoutSeconds: defaultShellExecTimeout,
	}
	if cfg.Model == "" {
		cfg.Model = defaultModel
	}
	if cfg.LLMBaseURL == "" {
		cfg.LLMBaseURL = defaultLLMBaseURL
	}
	if cfg.Workspace == "" {
		cfg.Workspace = defaultWorkspace
	}
	if cfg.EventLog == "" {
		cfg.EventLog = defaultEventLog
	}
	if v := os.Getenv(EnvResume); v == "1" || strings.EqualFold(v, "true") {
		cfg.Resume = true
	}
	if v := os.Getenv(EnvStream); v == "1" || strings.EqualFold(v, "true") {
		cfg.Stream = true
	}
	// System prompt: inline override takes precedence over a file; empty inline
	// is treated as unset. A configured file path that is missing/unreadable is a
	// hard launch error (fail loudly, do not silently fall back to the default).
	override, err := readPromptKnob(EnvSystemPrompt, EnvSystemPromptFile)
	if err != nil {
		return cfg, err
	}
	cfg.SystemPrompt = override
	append, err := readPromptKnob(EnvAppendSystemPrompt, EnvAppendSystemPromptFile)
	if err != nil {
		return cfg, err
	}
	cfg.AppendSystemPrompt = append
	cfg.MaxTurns = envInt(EnvMaxTurns, cfg.MaxTurns)
	cfg.MaxTotalTokens = envInt(EnvMaxTokens, cfg.MaxTotalTokens)
	cfg.MaxToolInvocations = envInt(EnvMaxTools, cfg.MaxToolInvocations)
	cfg.MaxWallClockSeconds = envInt(EnvMaxWallClock, cfg.MaxWallClockSeconds)
	cfg.ShellExecTimeoutSeconds = envInt(EnvShellTimeout, cfg.ShellExecTimeoutSeconds)

	// A positional argument overrides the goal env var.
	if args := os.Args; len(args) > 1 && args[1] != "" && args[1] != "-h" && args[1] != "--help" {
		cfg.Goal = args[1]
	}

	if cfg.Goal == "" && !cfg.Resume {
		return cfg, fmt.Errorf("no goal: set %s or pass it as the first argument", EnvGoal)
	}
	return cfg, nil
}

func envInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// readPromptKnob resolves a system-prompt knob (override or append) from its
// inline and file environment variables. Inline takes precedence; an empty
// inline value is treated as unset so it cannot blank the prompt. A configured
// file path that is missing or unreadable is a hard error — the caller must not
// silently fall back to the default.
func readPromptKnob(inlineEnv, fileEnv string) (string, error) {
	if v := os.Getenv(inlineEnv); v != "" {
		return v, nil
	}
	if path := os.Getenv(fileEnv); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read system prompt file %q: %w", path, err)
		}
		return string(b), nil
	}
	return "", nil
}

// configSummary is the shape recorded in the run_start event.
type configSummary struct {
	Compaction              string `json:"compaction,omitempty"`
	Stream                  bool   `json:"stream,omitempty"`
	SystemPromptSource      string `json:"system_prompt_source,omitempty"`
	MaxTurns                int    `json:"max_turns,omitempty"`
	MaxTotalTokens          int    `json:"max_total_tokens,omitempty"`
	MaxToolInvocations      int    `json:"max_tool_invocations,omitempty"`
	MaxWallClockSeconds     int    `json:"max_wall_clock_seconds,omitempty"`
	ShellExecTimeoutSeconds int    `json:"shell_exec_timeout_seconds,omitempty"`
}

// promptSource reports which system prompt this run uses, for the run_start
// event: "default", "override", "default+append", or "override+append".
func (c Config) promptSource() string {
	base := "default"
	if c.SystemPrompt != "" {
		base = "override"
	}
	if c.AppendSystemPrompt != "" {
		return base + "+append"
	}
	return base
}

func (c Config) summary() json.RawMessage {
	s := configSummary{
		Compaction:              c.Compaction,
		Stream:                  c.Stream,
		SystemPromptSource:      c.promptSource(),
		MaxTurns:                c.MaxTurns,
		MaxTotalTokens:          c.MaxTotalTokens,
		MaxToolInvocations:      c.MaxToolInvocations,
		MaxWallClockSeconds:     c.MaxWallClockSeconds,
		ShellExecTimeoutSeconds: c.ShellExecTimeoutSeconds,
	}
	b, _ := json.Marshal(s)
	return b
}
