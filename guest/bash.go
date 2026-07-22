package guest

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/spf13/afero"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// Bash is a virtual shell: mvdan/sh parses and interprets, while all I/O is
// routed to a [VFS] through interp handlers. No host process is ever spawned.
type Bash struct {
	vfs *VFS
}

// NewBash returns a virtual shell over vfs.
func NewBash(vfs *VFS) *Bash { return &Bash{vfs: vfs} }

// Result is the outcome of running a script.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Run parses and executes a shell script against the VFS, starting in cwd.
func (b *Bash) Run(ctx context.Context, script, cwd string) (Result, error) {
	if cwd == "" {
		cwd = "/work"
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(script), "")
	if err != nil {
		return Result{ExitCode: 2, Stderr: err.Error() + "\n"}, nil
	}
	var stdout, stderr strings.Builder
	runner, err := interp.New(
		interp.StdIO(strings.NewReader(""), &stdout, &stderr),
		interp.OpenHandler(b.openHandler),
		interp.StatHandler(b.statHandler),
		interp.ReadDirHandler2(b.readDirHandler),
		interp.ExecHandler(b.execHandler),
	)
	if err != nil {
		return Result{}, err
	}
	// Set the working directory directly; interp.Dir would stat the real OS
	// filesystem, but our cwd lives only in the VFS.
	runner.Dir = cwd
	runErr := runner.Run(ctx, file)
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if runErr != nil {
		if status, ok := interp.IsExitStatus(runErr); ok {
			res.ExitCode = int(status)
		} else {
			res.ExitCode = 1
			res.Stderr += runErr.Error() + "\n"
		}
	}
	return res, nil
}

func (b *Bash) openHandler(ctx context.Context, name string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
	hc := interp.HandlerCtx(ctx)
	name = resolve(hc.Dir, name)
	if name == "/dev/null" {
		return devNull{}, nil
	}
	return b.vfs.fs.OpenFile(name, flag, perm)
}

func (b *Bash) statHandler(ctx context.Context, name string, _ bool) (os.FileInfo, error) {
	hc := interp.HandlerCtx(ctx)
	return b.vfs.fs.Stat(resolve(hc.Dir, name))
}

func (b *Bash) readDirHandler(ctx context.Context, name string) ([]os.DirEntry, error) {
	hc := interp.HandlerCtx(ctx)
	infos, err := afero.ReadDir(b.vfs.fs, resolve(hc.Dir, name))
	if err != nil {
		return nil, err
	}
	out := make([]os.DirEntry, len(infos))
	for i, fi := range infos {
		out[i] = fs.FileInfoToDirEntry(fi)
	}
	return out, nil
}

type devNull struct{}

func (devNull) Read([]byte) (int, error)    { return 0, io.EOF }
func (devNull) Write(p []byte) (int, error) { return len(p), nil }
func (devNull) Close() error                { return nil }

// unknownCommand is returned by builtins to signal "not found" (exit 127).
var errNotFound = fmt.Errorf("command not found")
