// Package skills is the progressive-disclosure layer (Phase 6). It loads
// task-specific playbooks from disk and serves two views: a tiny L1 index
// (name + description + version) that goes into the system prompt, and the full
// L2 body, fetched on demand by the load_skill tool.
//
// The package is pure and read-only: it parses files and answers lookups. It
// decides nothing — the *model* reads the index and chooses to load a skill,
// exactly as it chooses to call any other tool. See docs/p6-skills.md.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Meta is the L1 view that lives in the prompt index — tiny, always in context.
type Meta struct {
	Name        string
	Description string
	Version     string // e.g. "1"; carried from day one for skill_loaded telemetry
}

// Skill is the full L2 view: the metadata plus the SKILL.md body. Loaded on
// demand, not held in the base prompt.
type Skill struct {
	Meta
	Body string
}

// Registry holds the skills loaded from a directory. It is read-only after Load.
type Registry struct {
	skills map[string]Skill
	order  []string // skill names, sorted, for a stable index

	// Skipped records skills that failed to parse (dir name -> reason). A bad
	// skill is skipped, never fatal — one malformed file must not blind the agent
	// to every other skill.
	Skipped map[string]string
}

// Load reads skills from projectDir (and, when set, from globalDir first).
// Two filesystem layouts are supported under each directory:
//
//  1. Directory-style: skills/<name>/SKILL.md
//     The directory name is just a container; the skill's identity comes from the
//     YAML frontmatter inside SKILL.md.
//  2. Bare-file style: skills/<name>.md
//     The .md file is the skill directly; the filename (without .md) is only used
//     as a fallback identity when the frontmatter is missing a "name" field.
//     Frontmatter "name" always takes precedence.
//
// Both layouts can coexist in the same directory. globalDir is loaded first and
// supplies skills that are available to every project; projectDir is loaded
// second and can override a global skill by using the same name. A missing
// directory is not an error (an empty step); a malformed skill is skipped and
// recorded in Skipped.
func Load(globalDir, projectDir string) (*Registry, error) {
	r := &Registry{
		skills:  make(map[string]Skill),
		Skipped: make(map[string]string),
	}

	// Load global (user-level) skills first, then project skills. Project wins on
	// name conflict because later loads override earlier entries.
	if globalDir != "" {
		if err := loadDir(r, globalDir); err != nil {
			return nil, err
		}
	}
	if err := loadDir(r, projectDir); err != nil {
		return nil, err
	}
	sort.Strings(r.order)
	return r, nil
}

// loadDir appends skills from a single directory into r. A missing directory is
// silent (no-op); other errors propagate. Project skills override global ones by
// name because they are loaded second.
func loadDir(r *Registry, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, e := range entries {
		var content []byte
		var label string // key for Skipped map and duplicate detection fallback

		if e.IsDir() {
			b, err := os.ReadFile(filepath.Join(dir, e.Name(), "SKILL.md"))
			if err != nil {
				continue
			}
			content = b
			label = e.Name()
		} else if strings.HasSuffix(e.Name(), ".md") {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			content = b
			label = strings.TrimSuffix(e.Name(), ".md")
		} else {
			continue
		}

		skill, err := parseSkill(string(content))
		if err != nil {
			r.Skipped[label] = err.Error()
			continue
		}
		if skill.Name == "" {
			skill.Name = label
		}
		// Project path (loaded second) deliberately overwrites global entries.
		if _, dup := r.skills[skill.Name]; dup {
			if label != skill.Name {
				r.Skipped[label] = fmt.Sprintf("duplicate skill name %q (project overrides global)", skill.Name)
			}
		}
		r.skills[skill.Name] = skill
		if !contains(r.order, skill.Name) {
			r.order = append(r.order, skill.Name)
		}
	}
	return nil
}

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// Index returns the L1 metadata for every skill, in stable (sorted) order.
func (r *Registry) Index() []Meta {
	out := make([]Meta, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.skills[name].Meta)
	}
	return out
}

// Get returns the full skill by name.
func (r *Registry) Get(name string) (Skill, bool) {
	s, ok := r.skills[strings.TrimSpace(name)]
	return s, ok
}

// Len reports how many skills loaded.
func (r *Registry) Len() int { return len(r.order) }

// PromptIndex renders the L1 index block for the system prompt: one line per
// skill (name + description), and nothing else — never a body. Returns "" when
// no skills are loaded so the caller can omit the section entirely.
//
// This is the guardrail against the static-injection trap: only this tiny index
// ever enters the base prompt; bodies are pulled on demand via load_skill.
func (r *Registry) PromptIndex() string {
	if len(r.order) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Skills — task-specific playbooks for this project. If the task matches a " +
		"skill's description below, call load_skill(name) and follow it BEFORE you start — even " +
		"if the change looks obvious. This is reading project-specific guidance, not " +
		"over-investigation. Do not guess a skill's contents; load it.\n")
	for _, name := range r.order {
		m := r.skills[name].Meta
		fmt.Fprintf(&b, "- %s: %s\n", m.Name, m.Description)
	}
	return strings.TrimRight(b.String(), "\n")
}

// frontmatter is the YAML header of a SKILL.md.
type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Version     string `yaml:"version"`
}

// parseSkill splits a SKILL.md into its YAML frontmatter and markdown body. The
// file must open with a "---" fence, carry a closing "---", and declare at least
// name and description.
func parseSkill(content string) (Skill, error) {
	content = strings.TrimPrefix(content, "\ufeff") // strip a UTF-8 BOM if present
	lines := strings.Split(content, "\n")

	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return Skill{}, fmt.Errorf("missing frontmatter: SKILL.md must start with '---'")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return Skill{}, fmt.Errorf("unterminated frontmatter: missing closing '---'")
	}

	var fm frontmatter
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &fm); err != nil {
		return Skill{}, fmt.Errorf("invalid frontmatter YAML: %w", err)
	}
	// name: when absent, the caller falls back to the directory/filename so a bare
	// .md file without a frontmatter "name" still parses. description is still
	// required — a skill without one is useless to the model.
	if strings.TrimSpace(fm.Description) == "" {
		return Skill{}, fmt.Errorf("frontmatter missing required field 'description'")
	}

	return Skill{
		Meta: Meta{
			Name:        strings.TrimSpace(fm.Name),
			Description: strings.TrimSpace(fm.Description),
			Version:     strings.TrimSpace(fm.Version),
		},
		Body: strings.TrimSpace(strings.Join(lines[end+1:], "\n")),
	}, nil
}
