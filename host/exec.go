package host

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/mrn-dk/latigo/abi"
)

// ExecPolicy governs the optional, ambient exec.run capability. exec.run is the
// one Latigo capability that runs native code with the host's own OS authority,
// so it is deny-by-default and this policy exists to keep it from becoming an
// ungoverned second network egress (which would make HTTPPolicy meaningless).
//
// The safe default (zero value) denies every command: AllowCommands is empty,
// Network is false, and no host environment is inherited.
type ExecPolicy struct {
	// AllowCommands is the allowlist of permitted argv[0] basenames (e.g.
	// "python3", "make"). Empty denies all execution. A path is rejected unless
	// its base name is listed; ambient PATH lookup of arbitrary binaries is not
	// permitted.
	AllowCommands []string
	// Network, when false (the default), runs the child with no network access
	// so it cannot bypass http.fetch. On platforms where the reference host
	// cannot guarantee isolation, execution fails closed. Set true only to
	// deliberately (and unsafely) allow networked native execution.
	Network bool
	// Env is the exact environment handed to the child. The guest-supplied
	// req.Env is always ignored, so the guest can never leak or inject host
	// environment (e.g. secrets).
	Env []string
	// Dir is the working directory root for the child.
	Dir string
	// Timeout bounds each command. Zero defaults to 30s.
	Timeout time.Duration
}

// LocalExec returns an exec.run implementation governed by p. Register it with
// (*Host).Exec. It enforces a command allowlist, strips the guest-supplied
// environment, bounds runtime, and (unless p.Network is set) isolates the child
// from the network, failing closed where isolation is unavailable.
func LocalExec(p ExecPolicy) func(ctx context.Context, req abi.ExecRunRequest) (abi.ExecRunResponse, error) {
	abs, _ := filepath.Abs(p.Dir)
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return func(ctx context.Context, req abi.ExecRunRequest) (abi.ExecRunResponse, error) {
		if len(req.Cmd) == 0 {
			return abi.ExecRunResponse{}, Errorf(abi.ErrInvalid, "empty command")
		}
		if !commandAllowed(p.AllowCommands, req.Cmd[0]) {
			return abi.ExecRunResponse{}, Errorf(abi.ErrDenied, "command not allowed: %s", req.Cmd[0])
		}

		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		cmd := exec.CommandContext(ctx, req.Cmd[0], req.Cmd[1:]...)
		cmd.Dir = abs
		if req.Dir != "" {
			cmd.Dir = filepath.Join(abs, req.Dir)
		}
		// Never inherit or accept guest-supplied environment: a nil Env would
		// otherwise make Go inherit the host's, leaking secrets.
		cmd.Env = append([]string{}, p.Env...)
		if len(req.Stdin) > 0 {
			cmd.Stdin = bytes.NewReader(req.Stdin)
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		// Deny-by-default network isolation. Fails closed if the platform cannot
		// guarantee it and the operator did not opt into networked exec.
		if err := networkIsolate(cmd, p.Network); err != nil {
			return abi.ExecRunResponse{}, Errorf(abi.ErrDenied, "exec isolation: %v", err)
		}

		err := cmd.Run()
		resp := abi.ExecRunResponse{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				resp.ExitCode = ee.ExitCode()
				return resp, nil
			}
			return resp, Errorf(abi.ErrInternal, "exec: %v", err)
		}
		return resp, nil
	}
}

func commandAllowed(allow []string, cmd string) bool {
	base := filepath.Base(cmd)
	for _, a := range allow {
		if a == base || a == cmd {
			return true
		}
	}
	return false
}
