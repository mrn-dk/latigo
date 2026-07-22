package guest

import (
	"io/fs"
	"path"
	"strings"

	"github.com/spf13/afero"
)

// VFS is the guest's in-memory virtual filesystem. The virtual bash and any
// in-guest tooling operate on it; it never touches the host filesystem (that is
// reached only through fs.* hostcalls).
type VFS struct {
	fs afero.Fs
}

// NewVFS returns an empty in-memory VFS with a /work directory created.
func NewVFS() *VFS {
	v := &VFS{fs: afero.NewMemMapFs()}
	_ = v.fs.MkdirAll("/work", 0o755)
	return v
}

// Afero exposes the underlying afero.Fs.
func (v *VFS) Afero() afero.Fs { return v.fs }

// resolve makes p absolute relative to dir.
func resolve(dir, p string) string {
	if p == "" {
		return dir
	}
	if !path.IsAbs(p) {
		p = path.Join(dir, p)
	}
	return path.Clean(p)
}

// WriteFile is a convenience for seeding the VFS.
func (v *VFS) WriteFile(p string, data []byte) error {
	if d := path.Dir(p); d != "." && d != "/" {
		_ = v.fs.MkdirAll(d, 0o755)
	}
	return afero.WriteFile(v.fs, p, data, 0o644)
}

// ReadFile reads a file from the VFS.
func (v *VFS) ReadFile(p string) ([]byte, error) {
	return afero.ReadFile(v.fs, p)
}

// Snapshot returns a deterministic listing of all files (path -> size) for
// checkpointing/diagnostics.
func (v *VFS) Snapshot() map[string]int64 {
	out := map[string]int64{}
	_ = afero.Walk(v.fs, "/", func(p string, info fs.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		out[p] = info.Size()
		return nil
	})
	return out
}

func trimNewline(s string) string { return strings.TrimRight(s, "\n") }
