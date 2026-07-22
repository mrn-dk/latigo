package guest

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mrn-dk/latigo/abi"
)

// Transport carries a single hostcall request/response exchange.
type Transport interface {
	Hostcall(req abi.Request) (abi.Response, error)
}

// Client is a typed wrapper over a Transport implementing the ABI operations.
type Client struct {
	t    Transport
	caps abi.Capabilities
}

// NewClient builds a Client over t. If t is nil the platform default transport
// is used (the imported hostcall on wasm; an error stub elsewhere).
func NewClient(t Transport) *Client {
	if t == nil {
		t = newDefaultTransport()
	}
	return &Client{t: t}
}

// Capabilities returns the negotiated capability set (populated by Bootstrap).
func (c *Client) Capabilities() abi.Capabilities { return c.caps }

func (c *Client) call(op abi.Op, args any, out any) error {
	var raw abi.RawJSON
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return err
		}
		raw = b
	}
	resp, err := c.t.Hostcall(abi.Request{Op: op, Args: raw})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return &HostError{Op: op, Code: resp.Code, Message: resp.Error}
	}
	if out != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("latigo: decode %s result: %w", op, err)
		}
	}
	return nil
}

// HostError is a structured error returned by the host.
type HostError struct {
	Op      abi.Op
	Code    string
	Message string
}

func (e *HostError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Op, e.Message, e.Code)
	}
	return fmt.Sprintf("%s: %s", e.Op, e.Message)
}

// IsUnsupported reports whether err is a host error signalling an unsupported
// capability.
func IsUnsupported(err error) bool {
	var he *HostError
	return errors.As(err, &he) && he.Code == abi.ErrUnsupported
}

// ----- typed operations -----

func (c *Client) FSRead(path string) ([]byte, error) {
	var r abi.FSReadResponse
	err := c.call(abi.OpFSRead, abi.FSReadRequest{Path: path}, &r)
	return r.Data, err
}

func (c *Client) FSWrite(path string, data []byte, appendMode bool) (int, error) {
	var r abi.FSWriteResponse
	err := c.call(abi.OpFSWrite, abi.FSWriteRequest{Path: path, Data: data, Append: appendMode}, &r)
	return r.Bytes, err
}

func (c *Client) FSList(path string) ([]abi.FSDirEntry, error) {
	var r abi.FSListResponse
	err := c.call(abi.OpFSList, abi.FSListRequest{Path: path}, &r)
	return r.Entries, err
}

func (c *Client) FSStat(path string) (abi.FSStatResponse, error) {
	var r abi.FSStatResponse
	err := c.call(abi.OpFSStat, abi.FSStatRequest{Path: path}, &r)
	return r, err
}

func (c *Client) FSRemove(path string, recursive bool) error {
	return c.call(abi.OpFSRemove, abi.FSRemoveRequest{Path: path, Recursive: recursive}, nil)
}

func (c *Client) FSMkdir(path string, parents bool) error {
	return c.call(abi.OpFSMkdir, abi.FSMkdirRequest{Path: path, Parents: parents}, nil)
}

func (c *Client) LLMCall(req abi.LLMCallRequest) (abi.LLMCallResponse, error) {
	var r abi.LLMCallResponse
	err := c.call(abi.OpLLMCall, req, &r)
	return r, err
}

func (c *Client) ToolList() (abi.ToolListResponse, error) {
	var r abi.ToolListResponse
	err := c.call(abi.OpToolList, abi.ToolListRequest{}, &r)
	return r, err
}

func (c *Client) ToolInvoke(name string, args abi.RawJSON) (abi.ToolInvokeResponse, error) {
	var r abi.ToolInvokeResponse
	err := c.call(abi.OpToolInvoke, abi.ToolInvokeRequest{Name: name, Args: args}, &r)
	return r, err
}

func (c *Client) ExecRun(req abi.ExecRunRequest) (abi.ExecRunResponse, error) {
	var r abi.ExecRunResponse
	err := c.call(abi.OpExecRun, req, &r)
	return r, err
}

func (c *Client) MsgSend(channel, content string) error {
	return c.call(abi.OpMsgSend, abi.MsgSendRequest{Channel: channel, Content: content}, nil)
}

func (c *Client) MsgRecv(channel string, blocking bool) (abi.MsgRecvResponse, error) {
	var r abi.MsgRecvResponse
	err := c.call(abi.OpMsgRecv, abi.MsgRecvRequest{Channel: channel, Blocking: blocking}, &r)
	return r, err
}

func (c *Client) ApprovalAwait(action string, details abi.RawJSON) (abi.ApprovalAwaitResponse, error) {
	var r abi.ApprovalAwaitResponse
	err := c.call(abi.OpApprovalAwait, abi.ApprovalAwaitRequest{Action: action, Details: details}, &r)
	if IsUnsupported(err) {
		// Graceful degradation: no approval capability means pre-approved.
		return abi.ApprovalAwaitResponse{Approved: true, Reason: "no approval capability"}, nil
	}
	return r, err
}

func (c *Client) LogAppend(level, message string, fields abi.RawJSON) error {
	err := c.call(abi.OpLogAppend, abi.LogAppendRequest{Level: level, Message: message, Fields: fields}, nil)
	if IsUnsupported(err) {
		return nil
	}
	return err
}

func (c *Client) ClockNow() (int64, error) {
	var r abi.ClockNowResponse
	err := c.call(abi.OpClockNow, abi.ClockNowRequest{}, &r)
	return r.UnixNano, err
}

func (c *Client) RandBytes(n int) ([]byte, error) {
	var r abi.RandBytesResponse
	err := c.call(abi.OpRandBytes, abi.RandBytesRequest{N: n}, &r)
	return r.Bytes, err
}
