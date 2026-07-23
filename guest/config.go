package guest

import (
	"encoding/json"
	"os"
	"strconv"

	"github.com/mrn-dk/latigo/abi"
)

// Config is the guest's run configuration, supplied by the host at
// instantiation through process args and environment variables (which the host
// controls via the WASI runtime). This is how capability negotiation happens
// "at instantiation" without a dedicated hostcall.
type Config struct {
	Goal         string
	Model        string
	MaxTurns     int
	Capabilities abi.Capabilities
	// Compaction selects the transcript compaction strategy: "window" (default,
	// deterministic) or "llm" (model-driven summarisation).
	Compaction string
}

// Environment variable / arg names understood by the guest.
const (
	EnvCapabilities = "LATIGO_CAPABILITIES"
	EnvGoal         = "LATIGO_GOAL"
	EnvModel        = "LATIGO_MODEL"
	EnvMaxTurns     = "LATIGO_MAX_TURNS"
	EnvCompaction   = "LATIGO_COMPACTION"
)

// LoadConfig reads the run configuration from the environment.
func LoadConfig() Config {
	cfg := Config{
		Goal:       os.Getenv(EnvGoal),
		Model:      os.Getenv(EnvModel),
		MaxTurns:   16,
		Compaction: os.Getenv(EnvCompaction),
	}
	if v := os.Getenv(EnvCapabilities); v != "" {
		_ = json.Unmarshal([]byte(v), &cfg.Capabilities)
	}
	if v := os.Getenv(EnvMaxTurns); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxTurns = n
		}
	}
	// A positional argument overrides the goal env var.
	if args := os.Args; len(args) > 1 && args[1] != "" {
		cfg.Goal = args[1]
	}
	return cfg
}
