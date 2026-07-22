package guest

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/afero"
	"mvdan.cc/sh/v3/interp"
)

// execHandler implements a useful subset of coreutils against the VFS. Commands
// it does not recognise fall through to a "command not found" (exit 127).
func (b *Bash) execHandler(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return nil
	}
	hc := interp.HandlerCtx(ctx)
	cmd := args[0]
	rest := args[1:]

	fn, ok := coreutils[cmd]
	if !ok {
		fmt.Fprintf(hc.Stderr, "%s: command not found\n", cmd)
		return interp.NewExitStatus(127)
	}
	code := fn(b.vfs, &cmdCtx{
		dir:    hc.Dir,
		stdin:  hc.Stdin,
		stdout: hc.Stdout,
		stderr: hc.Stderr,
		args:   rest,
	})
	if code != 0 {
		return interp.NewExitStatus(uint8(code))
	}
	return nil
}

type cmdCtx struct {
	dir    string
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	args   []string
}

func (c *cmdCtx) errf(format string, a ...any) int {
	fmt.Fprintf(c.stderr, format+"\n", a...)
	return 1
}

type coreFn func(v *VFS, c *cmdCtx) int

var coreutils map[string]coreFn

func init() {
	coreutils = map[string]coreFn{
		"echo": cuEcho, "cat": cuCat, "ls": cuLs, "mkdir": cuMkdir,
		"rm": cuRm, "touch": cuTouch, "cp": cuCp, "mv": cuMv,
		"grep": cuGrep, "head": cuHead, "tail": cuTail, "wc": cuWc,
		"sort": cuSort, "uniq": cuUniq, "find": cuFind, "true": cuTrue,
		"false": cuFalse, "basename": cuBasename, "dirname": cuDirname,
		"tee": cuTee, "sleep": cuTrue, "which": cuWhich, "seq": cuSeq,
	}
}

func cuTrue(*VFS, *cmdCtx) int  { return 0 }
func cuFalse(*VFS, *cmdCtx) int { return 1 }

func cuEcho(_ *VFS, c *cmdCtx) int {
	args := c.args
	nl := true
	if len(args) > 0 && args[0] == "-n" {
		nl = false
		args = args[1:]
	}
	fmt.Fprint(c.stdout, strings.Join(args, " "))
	if nl {
		fmt.Fprintln(c.stdout)
	}
	return 0
}

func cuCat(v *VFS, c *cmdCtx) int {
	if len(c.args) == 0 {
		_, _ = io.Copy(c.stdout, c.stdin)
		return 0
	}
	code := 0
	for _, a := range c.args {
		data, err := afero.ReadFile(v.fs, resolve(c.dir, a))
		if err != nil {
			code = c.errf("cat: %s: No such file or directory", a)
			continue
		}
		c.stdout.Write(data)
	}
	return code
}

func cuLs(v *VFS, c *cmdCtx) int {
	long := false
	var paths []string
	for _, a := range c.args {
		if strings.HasPrefix(a, "-") {
			if strings.Contains(a, "l") {
				long = true
			}
			continue
		}
		paths = append(paths, a)
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}
	code := 0
	for _, p := range paths {
		full := resolve(c.dir, p)
		fi, err := v.fs.Stat(full)
		if err != nil {
			code = c.errf("ls: cannot access '%s': No such file or directory", p)
			continue
		}
		if !fi.IsDir() {
			printLs(c.stdout, fi.Name(), fi.Size(), fi.IsDir(), long)
			continue
		}
		infos, _ := afero.ReadDir(v.fs, full)
		sort.Slice(infos, func(i, j int) bool { return infos[i].Name() < infos[j].Name() })
		for _, e := range infos {
			printLs(c.stdout, e.Name(), e.Size(), e.IsDir(), long)
		}
	}
	return code
}

func printLs(w io.Writer, name string, size int64, isDir, long bool) {
	if long {
		typ := "-"
		if isDir {
			typ = "d"
		}
		fmt.Fprintf(w, "%s %8d %s\n", typ, size, name)
	} else {
		fmt.Fprintln(w, name)
	}
}

func cuMkdir(v *VFS, c *cmdCtx) int {
	parents := false
	var dirs []string
	for _, a := range c.args {
		if a == "-p" {
			parents = true
			continue
		}
		dirs = append(dirs, a)
	}
	code := 0
	for _, d := range dirs {
		full := resolve(c.dir, d)
		var err error
		if parents {
			err = v.fs.MkdirAll(full, 0o755)
		} else {
			err = v.fs.Mkdir(full, 0o755)
		}
		if err != nil {
			code = c.errf("mkdir: cannot create directory '%s': %v", d, err)
		}
	}
	return code
}

func cuRm(v *VFS, c *cmdCtx) int {
	recursive := false
	var targets []string
	for _, a := range c.args {
		if strings.HasPrefix(a, "-") {
			if strings.ContainsAny(a, "rR") {
				recursive = true
			}
			continue
		}
		targets = append(targets, a)
	}
	code := 0
	for _, t := range targets {
		full := resolve(c.dir, t)
		var err error
		if recursive {
			err = v.fs.RemoveAll(full)
		} else {
			err = v.fs.Remove(full)
		}
		if err != nil {
			code = c.errf("rm: cannot remove '%s': %v", t, err)
		}
	}
	return code
}

func cuTouch(v *VFS, c *cmdCtx) int {
	code := 0
	for _, a := range c.args {
		full := resolve(c.dir, a)
		if exists, _ := afero.Exists(v.fs, full); exists {
			continue
		}
		if err := afero.WriteFile(v.fs, full, nil, 0o644); err != nil {
			code = c.errf("touch: cannot touch '%s': %v", a, err)
		}
	}
	return code
}

func cuCp(v *VFS, c *cmdCtx) int {
	args := stripFlags(c.args)
	if len(args) < 2 {
		return c.errf("cp: missing operand")
	}
	dst := resolve(c.dir, args[len(args)-1])
	for _, src := range args[:len(args)-1] {
		data, err := afero.ReadFile(v.fs, resolve(c.dir, src))
		if err != nil {
			return c.errf("cp: cannot stat '%s': No such file or directory", src)
		}
		target := dst
		if isDir, _ := afero.DirExists(v.fs, dst); isDir {
			target = path.Join(dst, path.Base(src))
		}
		if err := afero.WriteFile(v.fs, target, data, 0o644); err != nil {
			return c.errf("cp: cannot create '%s': %v", target, err)
		}
	}
	return 0
}

func cuMv(v *VFS, c *cmdCtx) int {
	args := stripFlags(c.args)
	if len(args) < 2 {
		return c.errf("mv: missing operand")
	}
	dst := resolve(c.dir, args[len(args)-1])
	for _, src := range args[:len(args)-1] {
		s := resolve(c.dir, src)
		target := dst
		if isDir, _ := afero.DirExists(v.fs, dst); isDir {
			target = path.Join(dst, path.Base(src))
		}
		if err := v.fs.Rename(s, target); err != nil {
			return c.errf("mv: cannot move '%s': %v", src, err)
		}
	}
	return 0
}

func cuGrep(v *VFS, c *cmdCtx) int {
	args := c.args
	invert, count, ignoreCase := false, false, false
	var pattern string
	var files []string
	havePattern := false
	for _, a := range args {
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			for _, r := range a[1:] {
				switch r {
				case 'v':
					invert = true
				case 'c':
					count = true
				case 'i':
					ignoreCase = true
				}
			}
			continue
		}
		if !havePattern {
			pattern = a
			havePattern = true
			continue
		}
		files = append(files, a)
	}
	if !havePattern {
		return c.errf("grep: missing pattern")
	}
	match := func(line string) bool {
		l, p := line, pattern
		if ignoreCase {
			l, p = strings.ToLower(l), strings.ToLower(p)
		}
		return strings.Contains(l, p) != invert
	}
	scan := func(name string, r io.Reader) int {
		data, _ := io.ReadAll(r)
		lines := strings.Split(trimNewline(string(data)), "\n")
		n := 0
		for _, line := range lines {
			if match(line) {
				n++
				if !count {
					if len(files) > 1 {
						fmt.Fprintf(c.stdout, "%s:%s\n", name, line)
					} else {
						fmt.Fprintln(c.stdout, line)
					}
				}
			}
		}
		if count {
			fmt.Fprintln(c.stdout, n)
		}
		return n
	}
	total := 0
	if len(files) == 0 {
		total = scan("", c.stdin)
	} else {
		for _, f := range files {
			file, err := v.fs.Open(resolve(c.dir, f))
			if err != nil {
				c.errf("grep: %s: No such file or directory", f)
				continue
			}
			total += scan(f, file)
			file.Close()
		}
	}
	if total == 0 {
		return 1
	}
	return 0
}

func cuHead(v *VFS, c *cmdCtx) int { return headTail(v, c, true) }
func cuTail(v *VFS, c *cmdCtx) int { return headTail(v, c, false) }

func headTail(v *VFS, c *cmdCtx, head bool) int {
	n := 10
	var files []string
	args := c.args
	for i := 0; i < len(args); i++ {
		if args[i] == "-n" && i+1 < len(args) {
			n, _ = strconv.Atoi(args[i+1])
			i++
			continue
		}
		if strings.HasPrefix(args[i], "-") && len(args[i]) > 1 {
			if v, err := strconv.Atoi(args[i][1:]); err == nil {
				n = v
				continue
			}
		}
		files = append(files, args[i])
	}
	emit := func(r io.Reader) {
		data, _ := io.ReadAll(r)
		lines := strings.Split(trimNewline(string(data)), "\n")
		if head {
			if n < len(lines) {
				lines = lines[:n]
			}
		} else {
			if n < len(lines) {
				lines = lines[len(lines)-n:]
			}
		}
		for _, l := range lines {
			fmt.Fprintln(c.stdout, l)
		}
	}
	if len(files) == 0 {
		emit(c.stdin)
		return 0
	}
	for _, f := range files {
		file, err := v.fs.Open(resolve(c.dir, f))
		if err != nil {
			c.errf("%s: cannot open '%s'", map[bool]string{true: "head", false: "tail"}[head], f)
			continue
		}
		emit(file)
		file.Close()
	}
	return 0
}

func cuWc(v *VFS, c *cmdCtx) int {
	lineMode := false
	wordMode := false
	byteMode := false
	var files []string
	for _, a := range c.args {
		switch a {
		case "-l":
			lineMode = true
		case "-w":
			wordMode = true
		case "-c":
			byteMode = true
		default:
			files = append(files, a)
		}
	}
	if !lineMode && !wordMode && !byteMode {
		lineMode, wordMode, byteMode = true, true, true
	}
	report := func(name string, data []byte) {
		s := string(data)
		var parts []string
		if lineMode {
			parts = append(parts, strconv.Itoa(strings.Count(s, "\n")))
		}
		if wordMode {
			parts = append(parts, strconv.Itoa(len(strings.Fields(s))))
		}
		if byteMode {
			parts = append(parts, strconv.Itoa(len(data)))
		}
		line := strings.Join(parts, " ")
		if name != "" {
			line += " " + name
		}
		fmt.Fprintln(c.stdout, line)
	}
	if len(files) == 0 {
		data, _ := io.ReadAll(c.stdin)
		report("", data)
		return 0
	}
	for _, f := range files {
		data, err := afero.ReadFile(v.fs, resolve(c.dir, f))
		if err != nil {
			c.errf("wc: %s: No such file or directory", f)
			continue
		}
		report(f, data)
	}
	return 0
}

func cuSort(v *VFS, c *cmdCtx) int {
	reverse := false
	var files []string
	for _, a := range c.args {
		if a == "-r" {
			reverse = true
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		files = append(files, a)
	}
	var data []byte
	if len(files) == 0 {
		data, _ = io.ReadAll(c.stdin)
	} else {
		for _, f := range files {
			d, _ := afero.ReadFile(v.fs, resolve(c.dir, f))
			data = append(data, d...)
		}
	}
	lines := strings.Split(trimNewline(string(data)), "\n")
	sort.Strings(lines)
	if reverse {
		for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
			lines[i], lines[j] = lines[j], lines[i]
		}
	}
	for _, l := range lines {
		fmt.Fprintln(c.stdout, l)
	}
	return 0
}

func cuUniq(_ *VFS, c *cmdCtx) int {
	data, _ := io.ReadAll(c.stdin)
	lines := strings.Split(trimNewline(string(data)), "\n")
	var prev *string
	for _, l := range lines {
		if prev != nil && *prev == l {
			continue
		}
		fmt.Fprintln(c.stdout, l)
		cp := l
		prev = &cp
	}
	return 0
}

func cuFind(v *VFS, c *cmdCtx) int {
	root := "."
	var namePat string
	for i := 0; i < len(c.args); i++ {
		a := c.args[i]
		if a == "-name" && i+1 < len(c.args) {
			namePat = c.args[i+1]
			i++
			continue
		}
		if !strings.HasPrefix(a, "-") {
			root = a
		}
	}
	base := resolve(c.dir, root)
	_ = afero.Walk(v.fs, base, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if namePat != "" {
			if ok, _ := path.Match(namePat, path.Base(p)); !ok {
				return nil
			}
		}
		fmt.Fprintln(c.stdout, p)
		return nil
	})
	return 0
}

func cuBasename(_ *VFS, c *cmdCtx) int {
	if len(c.args) == 0 {
		return c.errf("basename: missing operand")
	}
	fmt.Fprintln(c.stdout, path.Base(c.args[0]))
	return 0
}

func cuDirname(_ *VFS, c *cmdCtx) int {
	if len(c.args) == 0 {
		return c.errf("dirname: missing operand")
	}
	fmt.Fprintln(c.stdout, path.Dir(c.args[0]))
	return 0
}

func cuTee(v *VFS, c *cmdCtx) int {
	data, _ := io.ReadAll(c.stdin)
	c.stdout.Write(data)
	for _, f := range stripFlags(c.args) {
		_ = afero.WriteFile(v.fs, resolve(c.dir, f), data, 0o644)
	}
	return 0
}

func cuWhich(_ *VFS, c *cmdCtx) int {
	code := 1
	for _, a := range c.args {
		if _, ok := coreutils[a]; ok {
			fmt.Fprintf(c.stdout, "/vbin/%s\n", a)
			code = 0
		}
	}
	return code
}

func cuSeq(_ *VFS, c *cmdCtx) int {
	if len(c.args) == 0 {
		return c.errf("seq: missing operand")
	}
	start, end, step := 1, 0, 1
	switch len(c.args) {
	case 1:
		end, _ = strconv.Atoi(c.args[0])
	case 2:
		start, _ = strconv.Atoi(c.args[0])
		end, _ = strconv.Atoi(c.args[1])
	default:
		start, _ = strconv.Atoi(c.args[0])
		step, _ = strconv.Atoi(c.args[1])
		end, _ = strconv.Atoi(c.args[2])
	}
	if step == 0 {
		return c.errf("seq: step cannot be zero")
	}
	for i := start; (step > 0 && i <= end) || (step < 0 && i >= end); i += step {
		fmt.Fprintln(c.stdout, i)
	}
	return 0
}

func stripFlags(args []string) []string {
	var out []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			continue
		}
		out = append(out, a)
	}
	return out
}
