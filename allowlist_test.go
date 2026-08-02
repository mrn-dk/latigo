package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAllowlistFileAndCSV(t *testing.T) {
	// JSON array form.
	p := filepath.Join(t.TempDir(), "allow.json")
	os.WriteFile(p, []byte(`[{"name":"rg","one_line":"ripgrep"},{"name":"pytest","one_line":"test runner","help":"pytest --help"}]`), 0o644)
	a, err := LoadAllowlist(p)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Allowed("rg") || !a.Allowed("pytest") {
		t.Fatalf("expected both allowed")
	}
	if a.Allowed("rm") {
		t.Fatalf("rm should not be allowed")
	}
	e, ok := a.Entry("pytest")
	if !ok || e.Help != "pytest --help" {
		t.Fatalf("help field wrong: %+v", e)
	}

	// Wrapped object form.
	os.WriteFile(p, []byte(`{"commands":[{"name":"go","one_line":"go toolchain"}]}`), 0o644)
	a2, err := LoadAllowlist(p)
	if err != nil {
		t.Fatal(err)
	}
	if !a2.Allowed("go") {
		t.Fatalf("go should be allowed")
	}

	// CSV form.
	a3 := AllowlistFromNames("rg,pytest,, cat")
	if !a3.Allowed("rg") || !a3.Allowed("cat") {
		t.Fatalf("csv parse wrong")
	}
}

func TestAllowlistSortedNames(t *testing.T) {
	a := AllowlistFromNames("pytest,rg,cat")
	got := a.sortedNames()
	want := []string{"cat", "pytest", "rg"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sortedNames[%d]=%q want %q (all=%v)", i, got[i], want[i], got)
		}
	}
}
