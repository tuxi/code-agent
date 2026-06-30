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
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Meta is the L1 view that lives in the prompt index — tiny, always in context.
type Meta struct {
	Name        string
	Description string
	Version     string // e.g. "1"; carried from day one for skill_loaded telemetry
	License     string // e.g. "Proprietary" or "Apache-2.0"; empty when unspecified
}

// Resource is an L3 file bundled with a skill — a reference document, a script,
// or an asset (template, font, icon). Path is absolute so the model can pass it
// directly to read_file or run_command.
type Resource struct {
	Path string // absolute path on disk
	Kind string // "reference", "script", or "asset"
}

// Skill is the full L2 view: the metadata plus the SKILL.md body and optional L3
// resources. Loaded on demand, not held in the base prompt.
type Skill struct {
	Meta
	Body       string
	Dir        string     // absolute path to the skill directory; empty for bare-file skills
	Resources  []Resource // L3 files (references, scripts, assets); nil if none
	Source     string     // "global" or "project" — which dir it came from
	sourcePath string     // path to the SKILL.md file; empty for bare-file skills
	modTime    time.Time  // mtime of SKILL.md when last loaded
}

// Registry holds the skills loaded from a directory. Skills are reloaded
// transparently on Get() when the source file's mtime is newer than the cached
// value, so editing a SKILL.md takes effect on the next load_skill call without
// restarting the agent. The mutex protects concurrent reloads.
type Registry struct {
	mu         sync.RWMutex
	skills     map[string]Skill
	order      []string // skill names, sorted, for a stable index
	loadCounts map[string]int

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
//     The .md file is the skill directly; the frontmatter MUST declare a "name"
//     field — the filename is never used as a fallback.
//
// Both layouts can coexist in the same directory. globalDir is loaded first and
// supplies skills that are available to every project; projectDir is loaded
// second and can override a global skill by using the same name. A missing
// directory is not an error (an empty step); a malformed skill is skipped and
// recorded in Skipped.
func Load(globalDir, projectDir string) (*Registry, error) {
	r := &Registry{
		skills:     make(map[string]Skill),
		Skipped:    make(map[string]string),
		loadCounts: make(map[string]int),
	}

	// Load global (user-level) skills first, then project skills. Project wins on
	// name conflict because later loads override earlier entries.
	if globalDir != "" {
		if err := loadDir(r, globalDir, "global"); err != nil {
			return nil, err
		}
	}
	if err := loadDir(r, projectDir, "project"); err != nil {
		return nil, err
	}
	sort.Strings(r.order)
	return r, nil
}

// loadDir appends skills from a single directory into r. A missing directory is
// silent (no-op); other errors propagate. Project skills override global ones by
// name because they are loaded second. source is "global" or "project" and is
// recorded on each skill for telemetry.
func loadDir(r *Registry, dir, source string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, e := range entries {
		var content []byte
		var label string    // key for Skipped map and duplicate detection
		var skillDir string // absolute path for directory-style skills; empty for bare files
		var srcPath string  // path to the SKILL.md file; empty for bare files
		var mt time.Time    // mtime of the source file

		if e.IsDir() {
			p := filepath.Join(dir, e.Name(), "SKILL.md")
			info, err := os.Stat(p)
			if err != nil {
				continue
			}
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			content = b
			label = e.Name()
			skillDir = filepath.Join(dir, e.Name())
			srcPath = p
			mt = info.ModTime()
		} else if strings.HasSuffix(e.Name(), ".md") {
			p := filepath.Join(dir, e.Name())
			info, err := os.Stat(p)
			if err != nil {
				continue
			}
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			content = b
			label = strings.TrimSuffix(e.Name(), ".md")
			srcPath = p
			mt = info.ModTime()
		} else {
			continue
		}

		skill, err := parseSkill(string(content))
		if err != nil {
			r.Skipped[label] = err.Error()
			continue
		}
		skill.Dir = skillDir
		skill.Source = source
		skill.sourcePath = srcPath
		skill.modTime = mt
		if skillDir != "" {
			skill.Resources = scanResources(skillDir)
		}
		// Project path (loaded second) deliberately overwrites global entries.
		if existing, dup := r.skills[skill.Name]; dup {
			if label != skill.Name {
				r.Skipped[label] = fmt.Sprintf("duplicate skill name %q (project overrides global)", skill.Name)
			}
			// Warn when a project skill shadows a global skill — this is the most
			// common source of "why is my skill different from what I expected?".
			if existing.Source == "global" && source == "project" {
				fmt.Fprintf(os.Stderr, "skills: project skill %q overrides global skill %q\n", skill.Name, existing.Source)
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

// scanResources walks the references/, scripts/, and assets/ subdirectories of
// skillDir (if they exist) and returns a sorted list of absolute paths tagged by
// kind. It is non-recursive — only direct children of each dir are listed — to
// keep the manifest short enough for the model to scan.
func scanResources(skillDir string) []Resource {
	var out []Resource
	scan := func(sub, kind string) {
		entries, err := os.ReadDir(filepath.Join(skillDir, sub))
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			out = append(out, Resource{
				Path: filepath.Join(skillDir, sub, e.Name()),
				Kind: kind,
			})
		}
	}
	scan("references", "reference")
	scan("scripts", "script")
	scan("assets", "asset")
	// Sort by kind then path for a stable manifest.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// Index returns the L1 metadata for every skill, in stable (sorted) order.
func (r *Registry) Index() []Meta {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Meta, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.skills[name].Meta)
	}
	return out
}

// Get returns the full skill by name. When the skill has a source file on disk and
// its mtime is newer than the cached value, Get re-parses the file transparently
// before returning — so editing a SKILL.md takes effect on the next load_skill call
// without restarting the agent. Bare-file skills and legacy entries without source
// tracking return the cached value unchanged.
func (r *Registry) Get(name string) (Skill, bool) {
	r.mu.RLock()
	s, ok := r.skills[strings.TrimSpace(name)]
	r.mu.RUnlock()
	if !ok {
		return Skill{}, false
	}
	// No source file to check — return cached (bare-file or legacy).
	if s.sourcePath == "" {
		return s, true
	}
	info, err := os.Stat(s.sourcePath)
	if err != nil || !info.ModTime().After(s.modTime) {
		return s, true
	}
	// Reload: the source changed since we loaded it.
	data, err := os.ReadFile(s.sourcePath)
	if err != nil {
		return s, true
	}
	reloaded, err := parseSkill(string(data))
	if err != nil {
		return s, true // keep the cached version on parse failure
	}
	reloaded.Dir = s.Dir
	reloaded.Source = s.Source
	reloaded.sourcePath = s.sourcePath
	reloaded.modTime = info.ModTime()
	if reloaded.Dir != "" {
		reloaded.Resources = scanResources(reloaded.Dir)
	}

	r.mu.Lock()
	oldName := name
	r.skills[reloaded.Name] = reloaded
	// If the skill renamed itself, clean up the old entry and update the order.
	if reloaded.Name != oldName {
		delete(r.skills, oldName)
		for i, n := range r.order {
			if n == oldName {
				r.order[i] = reloaded.Name
				break
			}
		}
		sort.Strings(r.order)
	}
	r.mu.Unlock()
	return reloaded, true
}

// Len reports how many skills loaded.
// Merge adds every skill from other into r that is not already present. First-write
// wins — cached skills already loaded (or since modified+reloaded via Get) are not
// replaced. New discoveries from a file-system re-scan simply appear.
func (r *Registry) Merge(other *Registry) {
	if other == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range other.order {
		if _, exists := r.skills[name]; exists {
			continue
		}
		r.skills[name] = other.skills[name]
		r.order = append(r.order, name)
	}
	sort.Strings(r.order)
}

func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.order)
}

// LoadCount returns how many times a skill has been loaded via load_skill. A skill
// that is rarely loaded usually has a bad description (under-triggering). This is
// an in-memory counter, reset on restart.
func (r *Registry) LoadCount(name string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.loadCounts[name]
}

// RecordLoad increments the in-memory load counter for a skill. Called by
// LoadSkillTool.AnnounceSkill so every load_skill call is counted.
func (r *Registry) RecordLoad(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loadCounts[name]++
}

// PromptIndex renders the L1 index block for the system prompt: one line per
// skill (name + description), and nothing else — never a body. Returns "" when
// no skills are loaded so the caller can omit the section entirely.
//
// This is the guardrail against the static-injection trap: only this tiny index
// ever enters the base prompt; bodies are pulled on demand via load_skill.
func (r *Registry) PromptIndex() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
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
		line := fmt.Sprintf("- %s: %s", m.Name, m.Description)
		if m.License != "" {
			line += fmt.Sprintf(" [%s]", m.License)
		}
		fmt.Fprintln(&b, line)
	}
	return strings.TrimRight(b.String(), "\n")
}

// frontmatter is the YAML header of a SKILL.md.
type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Version     string `yaml:"version"`
	License     string `yaml:"license"`
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
	// Both name and description are required — a skill without either is useless to
	// the model and incompatible with the Claude Code skill format.
	if strings.TrimSpace(fm.Name) == "" {
		return Skill{}, fmt.Errorf("frontmatter missing required field 'name'")
	}
	if strings.TrimSpace(fm.Description) == "" {
		return Skill{}, fmt.Errorf("frontmatter missing required field 'description'")
	}

	return Skill{
		Meta: Meta{
			Name:        strings.TrimSpace(fm.Name),
			Description: strings.TrimSpace(fm.Description),
			Version:     strings.TrimSpace(fm.Version),
			License:     strings.TrimSpace(fm.License),
		},
		Body: strings.TrimSpace(strings.Join(lines[end+1:], "\n")),
	}, nil
}
