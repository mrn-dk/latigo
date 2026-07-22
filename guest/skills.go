package guest

import (
	"path"
	"sort"
	"strings"

	"github.com/spf13/afero"
)

// SkillsDir is the VFS directory where on-demand markdown skills live.
const SkillsDir = "/skills"

// Skill is an on-demand markdown document describing how to do something.
type Skill struct {
	Name        string
	Description string
}

// Skills provides discovery and retrieval of markdown skills from the VFS.
type Skills struct{ vfs *VFS }

// NewSkills returns a skills provider over vfs.
func NewSkills(vfs *VFS) *Skills { return &Skills{vfs: vfs} }

// Seed writes a skill document into the VFS.
func (s *Skills) Seed(name, body string) error {
	return s.vfs.WriteFile(path.Join(SkillsDir, name+".md"), []byte(body))
}

// List returns the available skills with their one-line descriptions (the first
// markdown heading or first non-empty line).
func (s *Skills) List() []Skill {
	infos, err := afero.ReadDir(s.vfs.fs, SkillsDir)
	if err != nil {
		return nil
	}
	var out []Skill
	for _, fi := range infos {
		if fi.IsDir() || !strings.HasSuffix(fi.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(fi.Name(), ".md")
		body, _ := s.vfs.ReadFile(path.Join(SkillsDir, fi.Name()))
		out = append(out, Skill{Name: name, Description: firstLine(string(body))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Read returns the full markdown body of a skill.
func (s *Skills) Read(name string) (string, error) {
	body, err := s.vfs.ReadFile(path.Join(SkillsDir, name+".md"))
	return string(body), err
}

func firstLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		l := strings.TrimSpace(strings.TrimLeft(line, "# "))
		if l != "" {
			return l
		}
	}
	return ""
}
