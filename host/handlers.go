package host

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mrn-dk/latigo/abi"
)

// decode/encode helpers ------------------------------------------------------

func decode[T any](args json.RawMessage) (T, error) {
	var v T
	if len(args) == 0 {
		return v, nil
	}
	err := json.Unmarshal(args, &v)
	return v, err
}

func encode(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	return json.RawMessage(b), err
}

func handler[Req any, Resp any](fn func(context.Context, Req) (Resp, error)) Handler {
	return func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		req, err := decode[Req](args)
		if err != nil {
			return nil, Errorf(abi.ErrInvalid, "decode args: %v", err)
		}
		resp, err := fn(ctx, req)
		if err != nil {
			return nil, err
		}
		return encode(resp)
	}
}

// Filesystem -----------------------------------------------------------------

// FS registers sandboxed fs.* handlers rooted at root. All guest paths are
// resolved beneath root; traversal outside it is denied.
func (h *Host) FS(root string, writable bool) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	resolve := func(p string) (string, error) {
		clean := filepath.Join(abs, filepath.Clean("/"+p))
		if clean != abs && !strings.HasPrefix(clean, abs+string(os.PathSeparator)) {
			return "", Errorf(abi.ErrDenied, "path escapes sandbox: %s", p)
		}
		return clean, nil
	}

	h.Handle(abi.OpFSRead, handler(func(_ context.Context, r abi.FSReadRequest) (abi.FSReadResponse, error) {
		p, err := resolve(r.Path)
		if err != nil {
			return abi.FSReadResponse{}, err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return abi.FSReadResponse{}, Errorf(abi.ErrNotFound, "%v", err)
		}
		return abi.FSReadResponse{Data: data}, nil
	}))

	h.Handle(abi.OpFSWrite, handler(func(_ context.Context, r abi.FSWriteRequest) (abi.FSWriteResponse, error) {
		if !writable {
			return abi.FSWriteResponse{}, Errorf(abi.ErrDenied, "filesystem is read-only")
		}
		p, err := resolve(r.Path)
		if err != nil {
			return abi.FSWriteResponse{}, err
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return abi.FSWriteResponse{}, err
		}
		flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
		if r.Append {
			flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
		}
		f, err := os.OpenFile(p, flag, 0o644)
		if err != nil {
			return abi.FSWriteResponse{}, err
		}
		defer f.Close()
		n, err := f.Write(r.Data)
		return abi.FSWriteResponse{Bytes: n}, err
	}))

	h.Handle(abi.OpFSList, handler(func(_ context.Context, r abi.FSListRequest) (abi.FSListResponse, error) {
		p, err := resolve(r.Path)
		if err != nil {
			return abi.FSListResponse{}, err
		}
		ents, err := os.ReadDir(p)
		if err != nil {
			return abi.FSListResponse{}, Errorf(abi.ErrNotFound, "%v", err)
		}
		out := abi.FSListResponse{}
		for _, e := range ents {
			info, _ := e.Info()
			var size int64
			var mode uint32
			if info != nil {
				size = info.Size()
				mode = uint32(info.Mode())
			}
			out.Entries = append(out.Entries, abi.FSDirEntry{Name: e.Name(), IsDir: e.IsDir(), Size: size, Mode: mode})
		}
		return out, nil
	}))

	h.Handle(abi.OpFSStat, handler(func(_ context.Context, r abi.FSStatRequest) (abi.FSStatResponse, error) {
		p, err := resolve(r.Path)
		if err != nil {
			return abi.FSStatResponse{}, err
		}
		info, err := os.Stat(p)
		if err != nil {
			return abi.FSStatResponse{Exists: false}, nil
		}
		return abi.FSStatResponse{Exists: true, Entry: abi.FSDirEntry{
			Name: info.Name(), IsDir: info.IsDir(), Size: info.Size(), Mode: uint32(info.Mode()),
		}}, nil
	}))

	h.Handle(abi.OpFSRemove, handler(func(_ context.Context, r abi.FSRemoveRequest) (abi.FSRemoveResponse, error) {
		if !writable {
			return abi.FSRemoveResponse{}, Errorf(abi.ErrDenied, "filesystem is read-only")
		}
		p, err := resolve(r.Path)
		if err != nil {
			return abi.FSRemoveResponse{}, err
		}
		if r.Recursive {
			return abi.FSRemoveResponse{}, os.RemoveAll(p)
		}
		return abi.FSRemoveResponse{}, os.Remove(p)
	}))

	h.Handle(abi.OpFSMkdir, handler(func(_ context.Context, r abi.FSMkdirRequest) (abi.FSMkdirResponse, error) {
		if !writable {
			return abi.FSMkdirResponse{}, Errorf(abi.ErrDenied, "filesystem is read-only")
		}
		p, err := resolve(r.Path)
		if err != nil {
			return abi.FSMkdirResponse{}, err
		}
		if r.Parents {
			return abi.FSMkdirResponse{}, os.MkdirAll(p, 0o755)
		}
		return abi.FSMkdirResponse{}, os.Mkdir(p, 0o755)
	}))
	return nil
}

// Clock / Rand ---------------------------------------------------------------

// Clock registers clock.now. now is the time source (nil uses time.Now).
func (h *Host) Clock(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	h.Handle(abi.OpClockNow, handler(func(_ context.Context, _ abi.ClockNowRequest) (abi.ClockNowResponse, error) {
		return abi.ClockNowResponse{UnixNano: now().UnixNano()}, nil
	}))
}

// Rand registers rand.bytes using the provided reader (nil uses crypto/rand).
func (h *Host) Rand(src io.Reader) {
	if src == nil {
		src = rand.Reader
	}
	h.Handle(abi.OpRandBytes, handler(func(_ context.Context, r abi.RandBytesRequest) (abi.RandBytesResponse, error) {
		if r.N < 0 || r.N > 1<<20 {
			return abi.RandBytesResponse{}, Errorf(abi.ErrInvalid, "n out of range")
		}
		buf := make([]byte, r.N)
		if _, err := io.ReadFull(src, buf); err != nil {
			return abi.RandBytesResponse{}, err
		}
		return abi.RandBytesResponse{Bytes: buf}, nil
	}))
}

// Log ------------------------------------------------------------------------

// Log registers log.append, writing structured lines to w.
func (h *Host) Log(w io.Writer) {
	h.Handle(abi.OpLogAppend, handler(func(_ context.Context, r abi.LogAppendRequest) (abi.LogAppendResponse, error) {
		fmt.Fprintf(w, "[%s] %s %s\n", strings.ToUpper(r.Level), r.Message, string(r.Fields))
		return abi.LogAppendResponse{}, nil
	}))
}

// Messaging ------------------------------------------------------------------

// Messenger bridges msg.send / msg.recv.
type Messenger struct {
	// Out receives guest-sent messages.
	Out func(channel, content string)
	// In supplies messages to the guest; return ok=false when none available.
	In func(channel string, blocking bool) (content string, ok bool)
}

// Messaging registers msg.send / msg.recv against m.
func (h *Host) Messaging(m Messenger) {
	h.Handle(abi.OpMsgSend, handler(func(_ context.Context, r abi.MsgSendRequest) (abi.MsgSendResponse, error) {
		if m.Out != nil {
			m.Out(r.Channel, r.Content)
		}
		return abi.MsgSendResponse{}, nil
	}))
	h.Handle(abi.OpMsgRecv, handler(func(_ context.Context, r abi.MsgRecvRequest) (abi.MsgRecvResponse, error) {
		if m.In == nil {
			return abi.MsgRecvResponse{HasMessage: false}, nil
		}
		content, ok := m.In(r.Channel, r.Blocking)
		return abi.MsgRecvResponse{HasMessage: ok, Content: content, Channel: r.Channel}, nil
	}))
}

// Approval -------------------------------------------------------------------

// Approval registers approval.await. Passing nil omits the capability.
func (h *Host) Approval(decide func(action string, details json.RawMessage) (bool, string)) {
	if decide == nil {
		return
	}
	h.caps.Approval = true
	h.Handle(abi.OpApprovalAwait, handler(func(_ context.Context, r abi.ApprovalAwaitRequest) (abi.ApprovalAwaitResponse, error) {
		ok, reason := decide(r.Action, r.Details)
		return abi.ApprovalAwaitResponse{Approved: ok, Reason: reason}, nil
	}))
}

// Exec -----------------------------------------------------------------------

// Exec registers the optional exec.run capability using run. Passing nil omits
// the capability, in which case exec.run returns "unsupported". Enabling exec
// marks the run as Ambient: it runs native code with ungoverned OS authority,
// which is recorded in run_start so the escalation is auditable.
func (h *Host) Exec(run func(ctx context.Context, req abi.ExecRunRequest) (abi.ExecRunResponse, error)) {
	if run == nil {
		return
	}
	h.caps.Exec = true
	h.caps.Ambient = true
	h.Handle(abi.OpExecRun, handler(run))
}
