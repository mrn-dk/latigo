// Package events defines the Latigo durable event log and checkpoint format.
//
// The event log is the source of truth for a Latigo run. Every hostcall result
// is written ahead (appended and acknowledged) before the guest is allowed to
// observe it, so a run can be reconstructed by replaying recorded results
// rather than re-executing side effects.
package events

import (
	"encoding/json"
	"time"

	"github.com/mrn-dk/latigo/abi"
)

// SchemaVersion is the version of the event schema in this package.
const SchemaVersion = "0"

// Kind enumerates the event types that appear in a log.
type Kind string

const (
	// KindRunStart is the first event; it records negotiated capabilities.
	KindRunStart Kind = "run_start"
	// KindHostcall records a completed hostcall and its result (write-ahead).
	KindHostcall Kind = "hostcall"
	// KindCatalog records a tool-catalog snapshot so catalogs are replay-safe.
	KindCatalog Kind = "catalog"
	// KindCheckpoint records a compacted guest state snapshot for bounded replay.
	KindCheckpoint Kind = "checkpoint"
	// KindRunEnd is the final event; it records termination.
	KindRunEnd Kind = "run_end"
)

// Event is a single record in the durable log. Events are appended in strictly
// increasing Seq order.
type Event struct {
	Seq     uint64          `json:"seq"`
	Kind    Kind            `json:"kind"`
	Time    time.Time       `json:"time"`
	Harness string          `json:"harness_version"`
	Schema  string          `json:"schema_version"`
	Payload json.RawMessage `json:"payload"`
}

// RunStart is the payload of a KindRunStart event.
type RunStart struct {
	RunID        string           `json:"run_id"`
	ABIVersion   string           `json:"abi_version"`
	Capabilities abi.Capabilities `json:"capabilities"`
	Goal         string           `json:"goal,omitempty"`
}

// Hostcall is the payload of a KindHostcall event. The op and result are
// recorded verbatim so replay can return the exact bytes the guest observed.
type Hostcall struct {
	Op       abi.Op          `json:"op"`
	Request  json.RawMessage `json:"request"`
	Response json.RawMessage `json:"response"`
}

// Catalog is the payload of a KindCatalog event.
type Catalog struct {
	Epoch int            `json:"epoch"`
	Tools []abi.ToolSpec `json:"tools"`
}

// Checkpoint is the payload of a KindCheckpoint event. State is an opaque,
// guest-defined snapshot blob; SinceSeq is the last event folded into it, so a
// host may compact everything up to and including SinceSeq.
type Checkpoint struct {
	SinceSeq uint64          `json:"since_seq"`
	State    json.RawMessage `json:"state"`
}

// RunEnd is the payload of a KindRunEnd event.
type RunEnd struct {
	Reason string `json:"reason"`
	Error  string `json:"error,omitempty"`
}

// Marshal wraps a payload into an Event with the given metadata.
func Marshal(seq uint64, kind Kind, harness string, payload any) (Event, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}
	return Event{
		Seq:     seq,
		Kind:    kind,
		Time:    time.Now().UTC(),
		Harness: harness,
		Schema:  SchemaVersion,
		Payload: b,
	}, nil
}
