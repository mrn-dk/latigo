package guest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// This file implements the structured editing built-ins (spec 06): edit_file
// and multi_edit. Both operate purely on the VFS (guest/vfs.go) and return a
// unified-diff snippet of the change so a host can render/approve it. Neither
// issues a hostcall, so they are pure guest computation over recorded inputs
// (the VFS state itself lives in the checkpoint snapshot) and reconstruct
// exactly on replay.
//
// The diff rendering below is a small from-scratch line differ. The module
// intentionally carries no third-party diff dependency (see go.mod: four
// direct deps, and this must not become a fifth).

// editOp is one {old,new} step of a multi_edit request.
type editOp struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// registerEditTools adds edit_file and multi_edit to the registry.
func (a *Agent) registerEditTools() {
	r := a.tools

	r.Add(Tool{
		Name: "edit_file",
		Description: "Replace an exact string in a file on the virtual filesystem with another. " +
			"`old` must match exactly once unless `all` is set, in which case every occurrence is " +
			"replaced. Empty `old` on a file that does not yet exist creates it with `new` as its " +
			"content; empty `new` deletes the matched region. Ambiguous matches (more than one " +
			"occurrence without `all`) and zero matches are errors, never a guess. Returns a unified " +
			"diff of the change.",
		Schema: json.RawMessage(`{"type":"object","properties":{` +
			`"path":{"type":"string"},` +
			`"old":{"type":"string","description":"exact text to find; empty to create a new file"},` +
			`"new":{"type":"string","description":"replacement text; empty to delete the matched region"},` +
			`"all":{"type":"boolean","description":"replace every occurrence instead of requiring exactly one match"}` +
			`},"required":["path","old","new"]}`),
		Invoke: func(_ context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Path string `json:"path"`
				Old  string `json:"old"`
				New  string `json:"new"`
				All  bool   `json:"all"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			return a.editFile(in.Path, in.Old, in.New, in.All)
		},
	})

	r.Add(Tool{
		Name: "multi_edit",
		Description: "Apply an ordered list of exact-match {old,new} edits to a single file on the " +
			"virtual filesystem atomically: each edit is applied in order to the result of the " +
			"previous one, and every edit's `old` must match exactly once. If any edit fails (zero or " +
			"ambiguous matches), none of the edits are written — the file is left completely untouched. " +
			"The first edit's `old` may be empty to create a new file. Returns a unified diff of the net " +
			"change.",
		Schema: json.RawMessage(`{"type":"object","properties":{` +
			`"path":{"type":"string"},` +
			`"edits":{"type":"array","items":{"type":"object","properties":{` +
			`"old":{"type":"string"},"new":{"type":"string"}},"required":["old","new"]}}` +
			`},"required":["path","edits"]}`),
		Invoke: func(_ context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Path  string   `json:"path"`
				Edits []editOp `json:"edits"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			if len(in.Edits) == 0 {
				return "", errors.New("multi_edit: edits must be non-empty")
			}
			return a.multiEdit(in.Path, in.Edits)
		},
	})
}

// applyEdit performs one exact-match replace against content. old must match
// exactly once unless all is true (every occurrence is replaced). Ambiguity
// (more than one match without all) and absence (zero matches) are both
// errors — never a guess.
func applyEdit(content, old, new string, all bool) (string, error) {
	if old == "" {
		return "", errors.New("old must be non-empty to edit existing content")
	}
	n := strings.Count(content, old)
	if n == 0 {
		return "", errors.New("old string not found")
	}
	if !all && n > 1 {
		return "", fmt.Errorf("old string matches %d times; add more context to make it unique, or set all=true", n)
	}
	if all {
		return strings.ReplaceAll(content, old, new), nil
	}
	return strings.Replace(content, old, new, 1), nil
}

// editFile implements the edit_file tool. It handles the create-on-missing
// special case (empty old + missing file), delegates the replace itself to
// applyEdit, writes the result back to the VFS, and returns a unified diff of
// the change.
func (a *Agent) editFile(path, old, new string, all bool) (string, error) {
	if path == "" {
		return "", errors.New("edit_file: path is required")
	}
	existing, rerr := a.vfs.ReadFile(path)
	missing := rerr != nil

	var before, after string
	switch {
	case missing && old == "":
		// Create: the file does not exist yet and old is empty, so new is the
		// whole initial content (possibly empty, for an empty file).
		before = ""
		after = new
	case missing:
		return "", fmt.Errorf("edit_file: %s does not exist (old must be empty to create it)", path)
	default:
		before = string(existing)
		var aerr error
		after, aerr = applyEdit(before, old, new, all)
		if aerr != nil {
			return "", fmt.Errorf("edit_file: %s: %w", path, aerr)
		}
	}

	if err := a.vfs.WriteFile(path, []byte(after)); err != nil {
		return "", err
	}
	diff := unifiedDiff(path, before, after)
	if diff == "" {
		return fmt.Sprintf("edit_file: %s unchanged (old and new produce identical content)", path), nil
	}
	return diff, nil
}

// multiEdit implements the multi_edit tool: it applies edits, in order, to an
// in-memory copy of the file's content and only writes the result back to the
// VFS once every edit has applied cleanly. Any failure aborts before the VFS
// is touched at all, so the file is left byte-for-byte as it was — atomic,
// all-or-nothing, no partial writes to roll back.
func (a *Agent) multiEdit(path string, edits []editOp) (string, error) {
	if path == "" {
		return "", errors.New("multi_edit: path is required")
	}
	existing, rerr := a.vfs.ReadFile(path)
	missing := rerr != nil
	var before string
	if !missing {
		before = string(existing)
	}

	cur := before
	for i, e := range edits {
		if missing {
			if i != 0 || e.Old != "" {
				return "", fmt.Errorf("multi_edit: %s does not exist (first edit's old must be empty to create it)", path)
			}
			cur = e.New
			missing = false
			continue
		}
		next, aerr := applyEdit(cur, e.Old, e.New, false)
		if aerr != nil {
			return "", fmt.Errorf("multi_edit: %s: edit %d of %d: %w (rolled back, no changes written)", path, i+1, len(edits), aerr)
		}
		cur = next
	}

	if err := a.vfs.WriteFile(path, []byte(cur)); err != nil {
		return "", err
	}
	diff := unifiedDiff(path, before, cur)
	if diff == "" {
		return fmt.Sprintf("multi_edit: %s unchanged (net no-op)", path), nil
	}
	return diff, nil
}

// ----- unified diff rendering (no third-party dependency) -----

// diffLine is one line of a computed diff: a context line (kept in both
// before and after), a deletion (only in before), or an insertion (only in
// after).
type diffLine struct {
	kind byte // ' ', '-', '+'
	text string
}

// maxDiffCells bounds the O(n*m) LCS table used to align lines. Above this,
// diffLines falls back to a coarse (unaligned) diff so a huge edit can't
// exhaust memory/time; correctness (a valid, applyable-looking diff) is kept,
// minimality is not.
const maxDiffCells = 4_000_000

// unifiedDiff renders a minimal unified diff between before and after,
// labelled with path, in the conventional `--- a/<path>` / `+++ b/<path>`
// plus `@@ -oldStart,oldCount +newStart,newCount @@` hunk format with 3 lines
// of context. Returns "" when before == after. Deterministic, so replay
// reproduces byte-identical diff text.
func unifiedDiff(path, before, after string) string {
	if before == after {
		return ""
	}
	ops := diffLines(splitLines(before), splitLines(after))
	hunks := groupHunks(ops, 3)
	if len(hunks) == 0 {
		return ""
	}
	label := strings.TrimPrefix(path, "/")
	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n", label)
	fmt.Fprintf(&b, "+++ b/%s\n", label)
	for _, h := range hunks {
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", h.oldStart, h.oldCount, h.newStart, h.newCount)
		for _, l := range h.lines {
			b.WriteByte(l.kind)
			b.WriteString(l.text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// splitLines splits s into lines without a trailing empty element for a final
// newline (so "a\nb\n" and "a\nb" both split as two lines; the presence of
// the newline itself is a formatting nuance this differ does not track).
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// diffLines computes a line-level diff between a and b using the classic
// LCS-backtrack technique: build a suffix-LCS-length table, then walk it
// forward, at each step preferring "equal" and otherwise stepping toward
// whichever side keeps more of the LCS. This yields a minimal edit script.
func diffLines(a, b []string) []diffLine {
	n, m := len(a), len(b)
	if n*m > maxDiffCells {
		return coarseDiff(a, b)
	}
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				dp[i][j] = dp[i+1][j+1] + 1
			case dp[i+1][j] >= dp[i][j+1]:
				dp[i][j] = dp[i+1][j]
			default:
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	out := make([]diffLine, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, diffLine{' ', a[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			out = append(out, diffLine{'-', a[i]})
			i++
		default:
			out = append(out, diffLine{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, diffLine{'-', a[i]})
	}
	for ; j < m; j++ {
		out = append(out, diffLine{'+', b[j]})
	}
	return out
}

// coarseDiff is the large-input fallback: delete everything from a, then
// insert everything from b. Not minimal, but still a valid unified diff.
func coarseDiff(a, b []string) []diffLine {
	out := make([]diffLine, 0, len(a)+len(b))
	for _, l := range a {
		out = append(out, diffLine{'-', l})
	}
	for _, l := range b {
		out = append(out, diffLine{'+', l})
	}
	return out
}

// hunk is one unified-diff hunk: a contiguous run of changed lines padded
// with context, plus the line-number bookkeeping for its "@@ ... @@" header.
type hunk struct {
	oldStart, oldCount int
	newStart, newCount int
	lines              []diffLine
}

// groupHunks groups a flat diff-line op list into unified-diff hunks, each
// padded with up to `context` lines of unchanged text on either side; runs of
// changes closer together than 2*context are merged into one hunk so their
// context windows don't overlap or fragment.
func groupHunks(ops []diffLine, context int) []hunk {
	var changed []int
	for i, op := range ops {
		if op.kind != ' ' {
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		return nil
	}

	type span struct{ lo, hi int } // inclusive index range within ops
	var spans []span
	lo, hi := changed[0], changed[0]
	for _, idx := range changed[1:] {
		if idx-hi <= 2*context {
			hi = idx
			continue
		}
		spans = append(spans, span{lo, hi})
		lo, hi = idx, idx
	}
	spans = append(spans, span{lo, hi})

	// Running old/new line-consumption counts so each hunk's header can
	// report accurate 1-based start line numbers.
	oldPos := make([]int, len(ops)+1)
	newPos := make([]int, len(ops)+1)
	for i, op := range ops {
		oldPos[i+1], newPos[i+1] = oldPos[i], newPos[i]
		if op.kind == ' ' || op.kind == '-' {
			oldPos[i+1]++
		}
		if op.kind == ' ' || op.kind == '+' {
			newPos[i+1]++
		}
	}

	hunks := make([]hunk, 0, len(spans))
	for _, sp := range spans {
		start := sp.lo - context
		if start < 0 {
			start = 0
		}
		end := sp.hi + context
		if end > len(ops)-1 {
			end = len(ops) - 1
		}
		lines := ops[start : end+1]

		oldCount, newCount := 0, 0
		for _, l := range lines {
			if l.kind == ' ' || l.kind == '-' {
				oldCount++
			}
			if l.kind == ' ' || l.kind == '+' {
				newCount++
			}
		}
		oldStart := oldPos[start]
		if oldCount > 0 {
			oldStart++
		}
		newStart := newPos[start]
		if newCount > 0 {
			newStart++
		}
		hunks = append(hunks, hunk{oldStart, oldCount, newStart, newCount, lines})
	}
	return hunks
}
