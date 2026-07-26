package abi

// Capabilities is the set of optional features a host advertises to the guest
// at instantiation time. The guest reads it once (via [OpClockNow]-adjacent
// bootstrap; in practice through the host-provided config event) and degrades
// gracefully when a capability is absent.
//
// Required operations (fs.*, llm.call, tool.*, msg.*, log.append, clock.now,
// rand.bytes) are always assumed present on a conformant host and are not
// listed here.
type Capabilities struct {
	// Exec reports whether exec.run is available. exec.run runs native code
	// with the host's own ambient authority and is therefore the one capability
	// that escapes Latigo's in-guest guarantees (see Ambient).
	Exec bool `json:"exec"`
	// HTTP reports whether http.fetch is available: the single governed network
	// egress. When false, the guest has no network access at all.
	HTTP bool `json:"http"`
	// HTTPHosts is an advisory allowlist (host globs) the guest may surface to
	// the model so it knows where it is permitted to fetch. It is never
	// authoritative; the host enforces the real policy on every request.
	HTTPHosts []string `json:"http_hosts,omitempty"`
	// Ambient is true iff the host granted a capability that runs code with
	// ungoverned OS authority (currently exec.run). It is recorded in run_start
	// so every run is permanently stamped as sandboxed or ambient. The guest
	// cannot negotiate it away; it purely reflects the host's grant.
	Ambient bool `json:"ambient"`
	// Checkpoint reports whether state.checkpoint/state.restore are available.
	// When true the guest periodically snapshots its state so the host can
	// compact the log to bounded replay, and resume interrupted runs.
	Checkpoint bool `json:"checkpoint"`
	// Approval reports whether approval.await is available. When false, the
	// guest treats every action as pre-approved.
	Approval bool `json:"approval"`
	// Steer reports whether the host has wired a real msg.recv("steer") source
	// (see host.Messenger.In). msg.recv itself is a required op and is always
	// present on a conformant host, but polling it every turn for nothing adds
	// a recorded hostcall per turn; the guest's default Steer strategy point
	// only polls when this is true, so hosts that do not wire a steering source
	// see identical hostcall traffic and event logs to before in-loop steering
	// existed.
	Steer bool `json:"steer"`
	// FSWrite reports whether the host filesystem is writable.
	FSWrite bool `json:"fs_write"`
	// Multimodal reports whether llm.call accepts image content parts
	// (abi.ContentPart{Type:"image"}) in message Parts. The host should only
	// advertise this when the configured model actually accepts images. The
	// guest must not emit image parts when this is false; it degrades by
	// dropping images and inserting a text placeholder instead (see
	// guest.Registry.Invoke and the initial-turn image handling in
	// guest/agent.go), so a text-only host still works unmodified.
	Multimodal bool `json:"multimodal"`
	// MaxLLMTokens is an advisory cap on tokens per llm.call, 0 meaning no cap.
	MaxLLMTokens int `json:"max_llm_tokens"`
	// HostVersion identifies the host implementation for diagnostics.
	HostVersion string `json:"host_version"`
	// ABIVersion is the ABI version the host implements. The guest refuses to
	// run against a mismatched major version.
	ABIVersion string `json:"abi_version"`
}

// Negotiate returns the effective capability set given what the guest requires
// and what the host offers. It never enables something the host does not offer.
func Negotiate(want, have Capabilities) Capabilities {
	eff := have
	eff.Exec = want.Exec && have.Exec
	eff.HTTP = want.HTTP && have.HTTP
	eff.Checkpoint = want.Checkpoint && have.Checkpoint
	eff.Approval = want.Approval && have.Approval
	eff.Steer = want.Steer && have.Steer
	eff.FSWrite = want.FSWrite && have.FSWrite
	eff.Multimodal = want.Multimodal && have.Multimodal
	// Ambient is a property of the host grant, not something the guest chooses:
	// if the host offers native execution, the run is ambient regardless of
	// what the guest wanted.
	eff.Ambient = have.Exec
	return eff
}
