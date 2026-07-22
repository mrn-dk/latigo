package host

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"

	"github.com/mrn-dk/latigo/abi"
)

// LocalExec returns an exec.run implementation that runs native processes with
// their working directory constrained to dir. This is the optional exec.run
// capability; hosts that do not want native execution simply omit it.
func LocalExec(dir string) func(ctx context.Context, req abi.ExecRunRequest) (abi.ExecRunResponse, error) {
	abs, _ := filepath.Abs(dir)
	return func(ctx context.Context, req abi.ExecRunRequest) (abi.ExecRunResponse, error) {
		if len(req.Cmd) == 0 {
			return abi.ExecRunResponse{}, Errorf(abi.ErrInvalid, "empty command")
		}
		cmd := exec.CommandContext(ctx, req.Cmd[0], req.Cmd[1:]...)
		cmd.Dir = abs
		if req.Dir != "" {
			cmd.Dir = filepath.Join(abs, req.Dir)
		}
		cmd.Env = req.Env
		if len(req.Stdin) > 0 {
			cmd.Stdin = bytes.NewReader(req.Stdin)
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
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
