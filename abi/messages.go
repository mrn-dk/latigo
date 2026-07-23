package abi

// This file defines the per-operation request/response payloads carried in
// Request.Args and Response.Result. They are plain JSON structs.

// ----- fs.* -----

type FSReadRequest struct {
	Path string `json:"path"`
}

type FSReadResponse struct {
	// Data is the file contents. It is base64-encoded by encoding/json since the
	// field is []byte.
	Data []byte `json:"data"`
}

type FSWriteRequest struct {
	Path   string `json:"path"`
	Data   []byte `json:"data"`
	Append bool   `json:"append,omitempty"`
}

type FSWriteResponse struct {
	Bytes int `json:"bytes"`
}

type FSListRequest struct {
	Path string `json:"path"`
}

type FSDirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
	Mode  uint32 `json:"mode"`
}

type FSListResponse struct {
	Entries []FSDirEntry `json:"entries"`
}

type FSStatRequest struct {
	Path string `json:"path"`
}

type FSStatResponse struct {
	Entry  FSDirEntry `json:"entry"`
	Exists bool       `json:"exists"`
}

type FSRemoveRequest struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

type FSRemoveResponse struct{}

type FSMkdirRequest struct {
	Path    string `json:"path"`
	Parents bool   `json:"parents,omitempty"`
}

type FSMkdirResponse struct{}

// ----- llm.call -----

// LLMMessage is one chat message in an OpenAI-compatible exchange.
type LLMMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content"`
	Name       string        `json:"name,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	ToolCalls  []LLMToolCall `json:"tool_calls,omitempty"`
}

// LLMToolCall is a model-requested tool invocation.
type LLMToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded arguments
}

// LLMToolSpec advertises a callable tool to the model.
type LLMToolSpec struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Parameters  RawJSON `json:"parameters"` // JSON Schema
}

type LLMCallRequest struct {
	Model       string        `json:"model,omitempty"`
	Messages    []LLMMessage  `json:"messages"`
	Tools       []LLMToolSpec `json:"tools,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
}

type LLMCallResponse struct {
	Message      LLMMessage `json:"message"`
	FinishReason string     `json:"finish_reason"`
	InputTokens  int        `json:"input_tokens"`
	OutputTokens int        `json:"output_tokens"`
}

// ----- tool.* -----

type ToolListRequest struct{}

type ToolSpec struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Parameters  RawJSON `json:"parameters"` // JSON Schema
}

type ToolListResponse struct {
	Tools []ToolSpec `json:"tools"`
	// Epoch increments whenever the catalog changes; the guest uses it to know
	// when to re-read the catalog. Catalog changes arrive as events, so this is
	// replay-safe.
	Epoch int `json:"epoch"`
}

type ToolInvokeRequest struct {
	Name string  `json:"name"`
	Args RawJSON `json:"args"`
}

type ToolInvokeResponse struct {
	Result  RawJSON `json:"result"`
	IsError bool    `json:"is_error"`
}

// ----- exec.run (optional) -----

type ExecRunRequest struct {
	Cmd   []string `json:"cmd"`
	Stdin []byte   `json:"stdin,omitempty"`
	Dir   string   `json:"dir,omitempty"`
	Env   []string `json:"env,omitempty"`
}

type ExecRunResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   []byte `json:"stdout"`
	Stderr   []byte `json:"stderr"`
}

// ----- http.fetch (optional, governed egress) -----

// HTTPFetchRequest is a single HTTP transaction the guest asks the host to
// perform on its behalf. The host is free to clamp MaxBytes/Timeout down to its
// own policy limits; the guest's values are hints, never overrides.
type HTTPFetchRequest struct {
	Method  string            `json:"method,omitempty"` // default GET
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	// Body is the request body (base64-encoded by encoding/json).
	Body []byte `json:"body,omitempty"`
	// MaxBytes caps the response body the guest is willing to receive; the host
	// applies its own hard cap regardless.
	MaxBytes int `json:"max_bytes,omitempty"`
	// TimeoutMS is an advisory per-request timeout; the host clamps it.
	TimeoutMS int `json:"timeout_ms,omitempty"`
	// FollowRedirect asks the host to follow 3xx redirects (each hop is
	// re-checked against policy). Default false.
	FollowRedirect bool `json:"follow_redirect,omitempty"`
}

// HTTPFetchResponse is the host's recorded result of a fetch. Because it is a
// hostcall, it is captured in the event log and returned verbatim on replay.
type HTTPFetchResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	// Body is the (possibly truncated) response body.
	Body []byte `json:"body"`
	// Truncated is true when the body hit the host's MaxBytes cap.
	Truncated bool `json:"truncated,omitempty"`
	// FinalURL is the URL after any redirects were followed.
	FinalURL string `json:"final_url,omitempty"`
}

// ----- msg.* -----

type MsgSendRequest struct {
	Channel string `json:"channel,omitempty"`
	Content string `json:"content"`
}

type MsgSendResponse struct{}

type MsgRecvRequest struct {
	Channel string `json:"channel,omitempty"`
	// Blocking asks the host to wait for a message. When false, the host may
	// return HasMessage=false immediately.
	Blocking bool `json:"blocking,omitempty"`
}

type MsgRecvResponse struct {
	HasMessage bool   `json:"has_message"`
	Content    string `json:"content"`
	Channel    string `json:"channel,omitempty"`
}

// ----- approval.await -----

type ApprovalAwaitRequest struct {
	Action  string  `json:"action"`
	Details RawJSON `json:"details,omitempty"`
}

type ApprovalAwaitResponse struct {
	Approved bool   `json:"approved"`
	Reason   string `json:"reason,omitempty"`
}

// ----- log.append -----

type LogAppendRequest struct {
	Level   string  `json:"level"`
	Message string  `json:"message"`
	Fields  RawJSON `json:"fields,omitempty"`
}

type LogAppendResponse struct{}

// ----- state.checkpoint / state.restore (optional) -----

type StateCheckpointRequest struct {
	// State is an opaque, guest-defined snapshot blob.
	State RawJSON `json:"state"`
}

type StateCheckpointResponse struct{}

type StateRestoreRequest struct{}

type StateRestoreResponse struct {
	// Found reports whether a snapshot was available to restore from.
	Found bool `json:"found"`
	// State is the most recent snapshot blob when Found is true.
	State RawJSON `json:"state,omitempty"`
}

// ----- clock.now / rand.bytes -----

type ClockNowRequest struct{}

type ClockNowResponse struct {
	UnixNano int64 `json:"unix_nano"`
}

type RandBytesRequest struct {
	N int `json:"n"`
}

type RandBytesResponse struct {
	Bytes []byte `json:"bytes"`
}
