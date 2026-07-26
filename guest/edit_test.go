package guest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func newTestAgent(t *testing.T) *Agent {
	t.Helper()
	return NewAgent(Config{Goal: "g", MaxTurns: 1}, NewClient(&fakeTransport{}))
}

func TestEditFileUniqueMatchReplaces(t *testing.T) {
	a := newTestAgent(t)
	_ = a.vfs.WriteFile("/work/app.go", []byte("func handler() {}\n"))

	diff, err := a.editFile("/work/app.go", "func handler(", "func handler(ctx context.Context, ", false)
	if err != nil {
		t.Fatalf("editFile: %v", err)
	}
	got, _ := a.vfs.ReadFile("/work/app.go")
	want := "func handler(ctx context.Context, ) {}\n"
	if string(got) != want {
		t.Errorf("file content = %q, want %q", got, want)
	}
	if !strings.Contains(diff, "-func handler() {}") || !strings.Contains(diff, "+func handler(ctx context.Context, ) {}") {
		t.Errorf("diff missing expected +/- lines:\n%s", diff)
	}
	if !strings.HasPrefix(diff, "--- a/work/app.go\n+++ b/work/app.go\n") {
		t.Errorf("diff missing unified header:\n%s", diff)
	}
}

func TestEditFileZeroMatchesErrors(t *testing.T) {
	a := newTestAgent(t)
	_ = a.vfs.WriteFile("/work/app.go", []byte("package main\n"))

	if _, err := a.editFile("/work/app.go", "nonexistent string", "x", false); err == nil {
		t.Fatal("expected an error for zero matches")
	}
	// The file must be untouched.
	got, _ := a.vfs.ReadFile("/work/app.go")
	if string(got) != "package main\n" {
		t.Errorf("file mutated on a failed edit: %q", got)
	}
}

func TestEditFileMultipleMatchesErrorsWithoutAll(t *testing.T) {
	a := newTestAgent(t)
	_ = a.vfs.WriteFile("/work/x.txt", []byte("foo\nfoo\nfoo\n"))

	if _, err := a.editFile("/work/x.txt", "foo", "bar", false); err == nil {
		t.Fatal("expected ambiguity error for multiple matches without all")
	}
	got, _ := a.vfs.ReadFile("/work/x.txt")
	if string(got) != "foo\nfoo\nfoo\n" {
		t.Errorf("file mutated on an ambiguous edit: %q", got)
	}
}

func TestEditFileAllReplacesEveryOccurrence(t *testing.T) {
	a := newTestAgent(t)
	_ = a.vfs.WriteFile("/work/x.txt", []byte("foo\nfoo\nfoo\n"))

	if _, err := a.editFile("/work/x.txt", "foo", "bar", true); err != nil {
		t.Fatalf("editFile: %v", err)
	}
	got, _ := a.vfs.ReadFile("/work/x.txt")
	if string(got) != "bar\nbar\nbar\n" {
		t.Errorf("file content = %q, want all three replaced", got)
	}
}

func TestEditFileCreateOnMissing(t *testing.T) {
	a := newTestAgent(t)

	diff, err := a.editFile("/work/new.txt", "", "hello\nworld\n", false)
	if err != nil {
		t.Fatalf("editFile: %v", err)
	}
	got, err := a.vfs.ReadFile("/work/new.txt")
	if err != nil || string(got) != "hello\nworld\n" {
		t.Errorf("file content = %q err=%v, want created content", got, err)
	}
	if !strings.Contains(diff, "+hello") || !strings.Contains(diff, "+world") {
		t.Errorf("diff should show inserted lines for a new file:\n%s", diff)
	}
	if !strings.Contains(diff, "@@ -0,0 +1,2 @@") {
		t.Errorf("diff header should reflect a pure insertion:\n%s", diff)
	}
}

func TestEditFileOldNonEmptyOnMissingFileErrors(t *testing.T) {
	a := newTestAgent(t)
	if _, err := a.editFile("/work/missing.txt", "something", "x", false); err == nil {
		t.Fatal("expected an error editing a nonexistent file with non-empty old")
	}
}

func TestEditFileDeleteRegion(t *testing.T) {
	a := newTestAgent(t)
	_ = a.vfs.WriteFile("/work/x.txt", []byte("keep this\nDELETE ME\nkeep that\n"))

	if _, err := a.editFile("/work/x.txt", "DELETE ME\n", "", false); err != nil {
		t.Fatalf("editFile: %v", err)
	}
	got, _ := a.vfs.ReadFile("/work/x.txt")
	if string(got) != "keep this\nkeep that\n" {
		t.Errorf("file content = %q, want the region deleted", got)
	}
}

func TestEditFileOnExistingFileRequiresNonEmptyOld(t *testing.T) {
	a := newTestAgent(t)
	_ = a.vfs.WriteFile("/work/x.txt", []byte("content\n"))
	if _, err := a.editFile("/work/x.txt", "", "replacement", false); err == nil {
		t.Fatal("expected an error: old must be non-empty when the file already exists")
	}
}

func TestEditFileNoopReturnsEmptyDiffMessage(t *testing.T) {
	a := newTestAgent(t)
	_ = a.vfs.WriteFile("/work/x.txt", []byte("same\n"))
	out, err := a.editFile("/work/x.txt", "same\n", "same\n", false)
	if err != nil {
		t.Fatalf("editFile: %v", err)
	}
	if !strings.Contains(out, "unchanged") {
		t.Errorf("expected an 'unchanged' note for a no-op edit, got %q", out)
	}
}

// ----- multi_edit -----

func TestMultiEditAppliesSequentially(t *testing.T) {
	a := newTestAgent(t)
	_ = a.vfs.WriteFile("/work/x.txt", []byte("one two three\n"))

	_, err := a.multiEdit("/work/x.txt", []editOp{
		{Old: "one", New: "1"},
		{Old: "two", New: "2"},
		{Old: "three", New: "3"},
	})
	if err != nil {
		t.Fatalf("multiEdit: %v", err)
	}
	got, _ := a.vfs.ReadFile("/work/x.txt")
	if string(got) != "1 2 3\n" {
		t.Errorf("file content = %q, want all three edits applied", got)
	}
}

func TestMultiEditAtomicRollbackOnFailure(t *testing.T) {
	a := newTestAgent(t)
	original := "one two three\n"
	_ = a.vfs.WriteFile("/work/x.txt", []byte(original))

	_, err := a.multiEdit("/work/x.txt", []editOp{
		{Old: "one", New: "1"},
		{Old: "NOPE NOT THERE", New: "x"}, // fails: zero matches
		{Old: "three", New: "3"},
	})
	if err == nil {
		t.Fatal("expected multiEdit to fail on the second edit")
	}
	got, _ := a.vfs.ReadFile("/work/x.txt")
	if string(got) != original {
		t.Errorf("file content = %q, want the original untouched after rollback", got)
	}
}

func TestMultiEditAtomicRollbackOnAmbiguity(t *testing.T) {
	a := newTestAgent(t)
	original := "foo foo\nbar\n"
	_ = a.vfs.WriteFile("/work/x.txt", []byte(original))

	_, err := a.multiEdit("/work/x.txt", []editOp{
		{Old: "foo", New: "baz"}, // ambiguous: two matches, no "all" in multi_edit
		{Old: "bar", New: "qux"},
	})
	if err == nil {
		t.Fatal("expected multiEdit to fail on an ambiguous edit")
	}
	got, _ := a.vfs.ReadFile("/work/x.txt")
	if string(got) != original {
		t.Errorf("file content = %q, want untouched after rollback", got)
	}
}

func TestMultiEditCreatesOnMissing(t *testing.T) {
	a := newTestAgent(t)
	_, err := a.multiEdit("/work/new.txt", []editOp{
		{Old: "", New: "line1\nline2\n"},
		{Old: "line1", New: "LINE1"},
	})
	if err != nil {
		t.Fatalf("multiEdit: %v", err)
	}
	got, err := a.vfs.ReadFile("/work/new.txt")
	if err != nil || string(got) != "LINE1\nline2\n" {
		t.Errorf("file content = %q err=%v", got, err)
	}
}

func TestMultiEditRequiresNonEmptyEdits(t *testing.T) {
	a := newTestAgent(t)
	args := json.RawMessage(`{"path":"/work/x.txt","edits":[]}`)
	tool, ok := a.tools.local["multi_edit"]
	if !ok {
		t.Fatal("multi_edit not registered")
	}
	if _, err := tool.Invoke(context.Background(), args); err == nil {
		t.Fatal("expected an error for an empty edits list")
	}
}

// ----- diff correctness -----

func TestUnifiedDiffNoChangeIsEmpty(t *testing.T) {
	if d := unifiedDiff("/work/x.txt", "same\n", "same\n"); d != "" {
		t.Errorf("unifiedDiff for identical content = %q, want empty", d)
	}
}

func TestUnifiedDiffLocalizedChangeKeepsContext(t *testing.T) {
	before := "l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\nl10\n"
	after := "l1\nl2\nl3\nl4\nCHANGED\nl6\nl7\nl8\nl9\nl10\n"
	diff := unifiedDiff("/work/x.txt", before, after)
	if !strings.Contains(diff, "-l5") || !strings.Contains(diff, "+CHANGED") {
		t.Errorf("diff missing expected change lines:\n%s", diff)
	}
	// Context lines just around the change should be present...
	if !strings.Contains(diff, " l4") || !strings.Contains(diff, " l6") {
		t.Errorf("diff missing expected context lines:\n%s", diff)
	}
	// ...but a distant unrelated line should not be pulled into the hunk.
	if strings.Contains(diff, "l1\n") {
		t.Errorf("diff pulled in a far-away unchanged line, want a localized snippet:\n%s", diff)
	}
}

// ----- registered as tools / registry dispatch -----

func TestEditFileToolRegisteredAndDispatches(t *testing.T) {
	a := newTestAgent(t)
	_ = a.vfs.WriteFile("/work/x.txt", []byte("hello\n"))
	args, _ := json.Marshal(map[string]any{"path": "/work/x.txt", "old": "hello", "new": "goodbye", "all": false})
	out, parts, isErr := a.tools.Invoke(context.Background(), "edit_file", args)
	if isErr {
		t.Fatalf("edit_file tool call errored: %s", out)
	}
	if len(parts) != 0 {
		t.Errorf("edit_file should return plain text (no rich parts), got %d parts", len(parts))
	}
	if !strings.Contains(out, "-hello") || !strings.Contains(out, "+goodbye") {
		t.Errorf("tool output missing diff:\n%s", out)
	}
}
