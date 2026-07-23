// Package abi defines the Latigo host/guest ABI, version 0.
//
// This package is the contract between a Latigo guest (the harness compiled to
// WebAssembly) and any conformant host. It intentionally depends on nothing but
// the standard library so that hosts can import it cheaply.
//
// # Transport
//
// The guest and host exchange length-prefixed JSON in the guest's linear
// memory through a single imported function:
//
//	//go:wasmimport latigo_abi hostcall
//	func hostcall(reqPtr, reqLen, respPtr, respCap uint32) int32
//
// The guest writes a JSON-encoded [Request] into guest memory at reqPtr and
// hands the host a scratch buffer [respPtr:respPtr+respCap]. The host writes a
// JSON-encoded [Response] into that buffer and returns the number of bytes
// written. If the return value is greater than respCap, the guest must grow its
// buffer and retry with the same request. A negative return value indicates a
// fatal transport error.
package abi

// Version is the ABI version implemented by this module.
const Version = "0"

// ImportModule is the WebAssembly import module name that hosts must provide.
const ImportModule = "latigo_abi"

// ImportName is the name of the single hostcall function within [ImportModule].
const ImportName = "hostcall"

// Op identifies a hostcall operation. Operations are grouped into namespaces
// separated by a dot.
type Op string

// Hostcall operations. See the per-op request/response types in messages.go.
const (
	// Filesystem namespace (host-provided filesystem).
	OpFSRead   Op = "fs.read"
	OpFSWrite  Op = "fs.write"
	OpFSList   Op = "fs.list"
	OpFSStat   Op = "fs.stat"
	OpFSRemove Op = "fs.remove"
	OpFSMkdir  Op = "fs.mkdir"

	// Model inference.
	OpLLMCall Op = "llm.call"

	// Tool catalog and invocation (runtime-agnostic; routing is the host's).
	OpToolList   Op = "tool.list"
	OpToolInvoke Op = "tool.invoke"

	// Optional native execution capability.
	OpExecRun Op = "exec.run"

	// Optional governed HTTP egress. The single sanctioned way for a guest to
	// reach the network; the host mediates every request against a policy.
	OpHTTPFetch Op = "http.fetch"

	// Messaging between guest and the outside world.
	OpMsgSend Op = "msg.send"
	OpMsgRecv Op = "msg.recv"

	// Human-in-the-loop approval.
	OpApprovalAwait Op = "approval.await"

	// Structured logging.
	OpLogAppend Op = "log.append"

	// Host-injected clock and randomness (kept in the log for determinism).
	OpClockNow  Op = "clock.now"
	OpRandBytes Op = "rand.bytes"
)

// Request is the envelope the guest sends for every hostcall.
type Request struct {
	// Op is the operation to perform.
	Op Op `json:"op"`
	// Args is the operation-specific request payload, JSON-encoded.
	Args RawJSON `json:"args,omitempty"`
}

// Response is the envelope the host returns for every hostcall.
type Response struct {
	// Error, if non-empty, describes a host-side failure. Callers must treat a
	// response with a non-empty Error as failed regardless of Result.
	Error string `json:"error,omitempty"`
	// Code is a stable, machine-readable error code (see the Err* constants).
	Code string `json:"code,omitempty"`
	// Result is the operation-specific response payload, JSON-encoded.
	Result RawJSON `json:"result,omitempty"`
}

// RawJSON is a raw, already-encoded JSON value. It mirrors json.RawMessage but
// is defined here to avoid importing encoding/json in the type surface.
type RawJSON = []byte

// Stable error codes.
const (
	ErrUnsupported = "unsupported" // op or capability not available on this host
	ErrDenied      = "denied"      // capability present but access refused
	ErrNotFound    = "not_found"
	ErrInvalid     = "invalid" // malformed request
	ErrInternal    = "internal"
)
