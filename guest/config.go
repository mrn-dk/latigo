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
	// Images are host-attached images (e.g. latigo-local's -image flag) to
	// include in the initial user turn alongside Goal. Degraded to a text
	// placeholder per Capabilities.Multimodal — see Agent.initialUserMessage.
	Images []abi.ImageData
	// MaxAttachBytes caps a single image the *guest* pulls into the transcript
	// (attach_image reading from the VFS). Unlike host-attached images, which
	// the host caps before they ever reach the guest, this path is driven by
	// the model: the whole transcript is resent on every llm.call and each
	// request is recorded verbatim, so one oversized attach inflates the log on
	// every subsequent turn. Attaching above the cap fails with a text error
	// rather than silently truncating. 0 uses DefaultMaxAttachBytes.
	MaxAttachBytes int
}

// DefaultMaxAttachBytes bounds a single guest-initiated image attachment. It
// mirrors host.DefaultMaxImageBytes; the guest cannot re-encode (that is the
// host's job), so it refuses rather than downscales.
const DefaultMaxAttachBytes = 2 << 20 // 2 MiB

// Environment variable / arg names understood by the guest.
const (
	EnvCapabilities = "LATIGO_CAPABILITIES"
	EnvGoal         = "LATIGO_GOAL"
	EnvModel        = "LATIGO_MODEL"
	EnvMaxTurns     = "LATIGO_MAX_TURNS"
	EnvCompaction   = "LATIGO_COMPACTION"
	// EnvGoalImages carries a JSON-encoded []abi.ImageData for images the host
	// attaches to the initial goal (e.g. -image on latigo-local). Empty/absent
	// means no images, identical to pre-multimodal behaviour.
	EnvGoalImages = "LATIGO_GOAL_IMAGES"
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
	if v := os.Getenv(EnvGoalImages); v != "" {
		_ = json.Unmarshal([]byte(v), &cfg.Images)
	}
	// A positional argument overrides the goal env var.
	if args := os.Args; len(args) > 1 && args[1] != "" {
		cfg.Goal = args[1]
	}
	return cfg
}

// maxAttachBytes returns the effective per-image attachment cap.
func (a *Agent) maxAttachBytes() int {
	if a.cfg.MaxAttachBytes > 0 {
		return a.cfg.MaxAttachBytes
	}
	return DefaultMaxAttachBytes
}
