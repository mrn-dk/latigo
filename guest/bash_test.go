package guest

import (
	"context"
	"strings"
	"testing"
)

func run(t *testing.T, b *Bash, script string) Result {
	t.Helper()
	res, err := b.Run(context.Background(), script, "/work")
	if err != nil {
		t.Fatalf("bash run: %v", err)
	}
	return res
}

func TestBashCoreutils(t *testing.T) {
	vfs := NewVFS()
	b := NewBash(vfs, nil)

	cases := []struct {
		name, script, wantOut string
		wantCode              int
	}{
		{"echo", `echo hello world`, "hello world\n", 0},
		{"pipe-grep", "printf 'a\\nb\\nc\\n' | grep b", "b\n", 0},
		{"redirect-and-cat", `echo data > /work/f.txt; cat /work/f.txt`, "data\n", 0},
		{"wc-l", "printf 'x\\ny\\nz\\n' | wc -l", "3\n", 0},
		{"sort", "printf 'c\\na\\nb\\n' | sort", "a\nb\nc\n", 0},
		{"for-loop", `for i in 1 2 3; do echo $i; done`, "1\n2\n3\n", 0},
		{"missing-cmd", `nonesuch`, "", 127},
		{"head", "seq 5 | head -n 2", "1\n2\n", 0},
		{"append", `echo a > /work/g; echo b >> /work/g; cat /work/g`, "a\nb\n", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := run(t, b, c.script)
			if c.wantOut != "" && res.Stdout != c.wantOut {
				t.Errorf("stdout = %q, want %q (stderr %q)", res.Stdout, c.wantOut, res.Stderr)
			}
			if res.ExitCode != c.wantCode {
				t.Errorf("exit = %d, want %d (stderr %q)", res.ExitCode, c.wantCode, res.Stderr)
			}
		})
	}
}

func TestBashPersistsToVFS(t *testing.T) {
	vfs := NewVFS()
	b := NewBash(vfs, nil)
	run(t, b, `mkdir -p /work/sub && echo persisted > /work/sub/file`)
	data, err := vfs.ReadFile("/work/sub/file")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "persisted" {
		t.Errorf("got %q", data)
	}
}

func TestStarlarkBudget(t *testing.T) {
	r := NewScriptRunner(ScriptBudget{MaxSteps: 100})
	// An unbounded loop must be stopped by the step budget.
	_, err := r.Run("loop", "for i in range(100000000): pass")
	if err == nil {
		t.Fatal("expected step budget to abort the script")
	}
}

func TestStarlarkMemory(t *testing.T) {
	r := NewScriptRunner(ScriptBudget{})
	if _, err := r.Run("s1", `memory["k"] = 42`); err != nil {
		t.Fatal(err)
	}
	out, err := r.Run("s2", `print(memory["k"])`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "42" {
		t.Errorf("memory not persisted across runs: %q", out)
	}
}
