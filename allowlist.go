// Package main: allowlist.go is the command allow-list (spec §2.3, §2.4).
//
// A tool is a CLI binary in the host image plus an allow-list entry. The
// allow-list governs *which binaries may run*; the security boundary is the
// WASM sandbox and per-instance quotas, not this list (spec §2.4 "stated
// limitation"). Anything not granted does not exist from inside the sandbox.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// AllowEntry is one tool in the command allow-list.
type AllowEntry struct {
	// Name is the binary name as it appears on $PATH in the host image, or an
	// alias the image maps to a binary.
	Name string `json:"name"`
	// OneLine is the short description shown by tool_list. Keep it terse; the
	// model fetches full usage on demand with "<name> --help" via the shell.
	OneLine string `json:"one_line"`
	// Help is an optional in-band help string. When empty, discovery falls
	// back to running "<name> --help" through the shell tool.
	Help string `json:"help,omitempty"`
}

// Allowlist is the set of binaries Latigo is permitted to spawn.
type Allowlist struct {
	commands map[string]AllowEntry
	order    []string
}

// NewAllowlist builds an empty allow-list.
func NewAllowlist() *Allowlist { return &Allowlist{commands: map[string]AllowEntry{}} }

// Add inserts an entry. Duplicates by name are ignored (first wins).
func (a *Allowlist) Add(e AllowEntry) {
	if e.Name == "" {
		return
	}
	if _, ok := a.commands[e.Name]; ok {
		return
	}
	a.commands[e.Name] = e
	a.order = append(a.order, e.Name)
}

// Allowed reports whether name is on the allow-list.
func (a *Allowlist) Allowed(name string) bool {
	_, ok := a.commands[name]
	return ok
}

// Entry returns the entry for name and whether it was present.
func (a *Allowlist) Entry(name string) (AllowEntry, bool) {
	e, ok := a.commands[name]
	return e, ok
}

// Entries returns the allow-list in insertion order.
func (a *Allowlist) Entries() []AllowEntry {
	out := make([]AllowEntry, 0, len(a.order))
	for _, n := range a.order {
		out = append(out, a.commands[n])
	}
	return out
}

// shellBuiltins are shell-language builtins interpreted by mvdan/sh's interp
// directly — they are not spawned binaries, so they are permitted regardless of
// the allow-list. This set is deliberately small and side-effect-bounded
// (shell state only): cd, pwd, true, false, :, set, export, unset, alias,
// unalias, echo, printf, read, type, hash. Allowing "eval" would be a hole, so
// it is excluded and hard-rejected in the AST walk.
var shellBuiltins = map[string]bool{
	"cd": true, "pwd": true, "true": true, "false": true, ":": true,
	"set": true, "export": true, "unset": true, "alias": true, "unalias": true,
	"echo": true, "printf": true, "read": true, "type": true, "hash": true,
	"return": true, "break": true, "continue": true, "trap": true, "umask": true,
	"ulimit": true, "getopts": true, "shift": true, "wait": true, "jobs": true,
	"bg": true, "fg": true, "kill": true, "times": true,
}

// isShellBuiltin reports whether name is an interp-handled builtin (not a
// spawned binary), and so exempt from the binary allow-list.
func isShellBuiltin(name string) bool { return shellBuiltins[name] }

// LoadAllowlist reads an allow-list from a JSON file. The file is either an
// object {"commands":[...]} or a bare array [...]. An empty/missing file
// yields an empty allow-list (Latigo then has only its built-in tools).
func LoadAllowlist(path string) (*Allowlist, error) {
	a := NewAllowlist()
	if path == "" {
		return a, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []AllowEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		var wrap struct {
			Commands []AllowEntry `json:"commands"`
		}
		if err2 := json.Unmarshal(data, &wrap); err2 == nil {
			entries = wrap.Commands
		} else {
			return nil, fmt.Errorf("allowlist: parse %s: %w", path, err)
		}
	}
	for _, e := range entries {
		a.Add(e)
	}
	return a, nil
}

// AllowlistFromNames builds an allow-list from a comma-separated list of names
// (one_line left blank; the model discovers usage via --help).
func AllowlistFromNames(csv string) *Allowlist {
	a := NewAllowlist()
	for _, n := range strings.Split(csv, ",") {
		n = strings.TrimSpace(n)
		if n != "" {
			a.Add(AllowEntry{Name: n, OneLine: ""})
		}
	}
	return a
}

// sortedNames returns entry names sorted, for stable tool_list output.
func (a *Allowlist) sortedNames() []string {
	names := make([]string, 0, len(a.order))
	names = append(names, a.order...)
	sort.Strings(names)
	return names
}
