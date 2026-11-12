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
	"crypto/sha256"
	"encoding/hex"
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
	Body        string
	Dir         string     // absolute path to the skill directory; empty for bare-file skills
	Resources   []Resource // L3 files (references, scripts, assets); nil if none
	Scope       Scope      // system, user, project, or plugin
	Source      string     // human-readable source: system/user/project/plugin:<name>/legacy global
	Path        string     // absolute path to SKILL.md or the bare .md file
	ContentHash string     // SHA-256 of the source bytes
	Enabled     bool       // false entries are not returned by the loader
	sourcePath  string     // path to the SKILL.md file; empty for bare-file skills
	modTime     time.Time  // mtime of SKILL.md when last loaded
	namespace   string     // plugin namespace, already included in Meta.Name
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

	// Errors keeps the same parse/duplicate failures as Skipped, with the scope
	// and source attached for diagnostics and future management UIs.
	Errors []LoadError
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
	return loadRoots([]Root{
		{Dir: globalDir, Scope: ScopeUser, Source: "global"},
		{Dir: projectDir, Scope: ScopeProject, Source: "project"},
	})
}

// loadRoots loads roots in array order. Callers should order ordinary roots from
// lowest to highest precedence; equal-scope roots loaded later win.
func loadRoots(roots []Root) (*Registry, error) {
	r := &Registry{
		skills:     make(map[string]Skill),
		Skipped:    make(map[string]string),
		loadCounts: make(map[string]int),
	}

	for _, root := range roots {
		if root.Dir == "" {
			continue
		}
		if err := loadDir(r, root); err != nil {
			return nil, err
		}
	}
	sort.Strings(r.order)
	return r, nil
}

// loadDir appends skills from a single scoped directory into r. A missing
// directory is silent (no-op); other errors propagate. Higher-precedence scopes
// override lower-precedence ones by name.
func loadDir(r *Registry, root Root) error {
	entries, err := os.ReadDir(root.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, e := range entries {
		if e.Name() == ".system" && root.Scope == ScopeUser {
			continue // system skills are loaded from their own root
		}
		var content []byte
		var label string    // key for Skipped map and duplicate detection
		var skillDir string // absolute path for directory-style skills; empty for bare files
		var srcPath string  // path to the SKILL.md file; empty for bare files
		var mt time.Time    // mtime of the source file

		if e.IsDir() {
			p := filepath.Join(root.Dir, e.Name(), "SKILL.md")
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
			skillDir = filepath.Join(root.Dir, e.Name())
			srcPath = p
			mt = info.ModTime()
		} else if strings.HasSuffix(e.Name(), ".md") {
			p := filepath.Join(root.Dir, e.Name())
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
			r.Errors = append(r.Errors, LoadError{
				Label: label, Scope: root.Scope, Source: root.Source,
				Path: srcPath, Err: err.Error(),
			})
			continue
		}
		if root.PluginName != "" {
			skill.namespace = root.PluginName
			skill.Meta.Name = root.PluginName + ":" + skill.Meta.Name
		}
		skill.Dir = skillDir
		skill.Scope = root.Scope
		skill.Source = root.Source
		skill.Path = srcPath
		sum := sha256.Sum256(content)
		skill.ContentHash = hex.EncodeToString(sum[:])
		skill.Enabled = true
		skill.sourcePath = srcPath
		skill.modTime = mt
		if skillDir != "" {
			skill.Resources = scanResources(skillDir)
		}
		if existing, dup := r.skills[skill.Name]; dup {
			if root.Scope == ScopePlugin && existing.Scope != ScopePlugin {
				reason := fmt.Sprintf("plugin skill %q cannot override existing %s skill", skill.Name, existing.Scope)
				r.Skipped[label] = reason
				r.Errors = append(r.Errors, LoadError{
					Label: label, Scope: root.Scope, Source: root.Source,
					Path: srcPath, Err: reason,
				})
				continue
			}
			if scopeRank(root.Scope) < scopeRank(existing.Scope) {
				reason := fmt.Sprintf("lower-precedence %s skill %q ignored; %s wins", root.Scope, skill.Name, existing.Scope)
				r.Skipped[label] = reason
				r.Errors = append(r.Errors, LoadError{
					Label: label, Scope: root.Scope, Source: root.Source,
					Path: srcPath, Err: reason,
				})
				continue
			}
			if scopeRank(root.Scope) > scopeRank(existing.Scope) {
				fmt.Fprintf(os.Stderr, "skills: %s skill %q overrides %s skill\n", root.Scope, skill.Name, existing.Scope)
				r.Errors = append(r.Errors, LoadError{
					Label: label, Scope: root.Scope, Source: root.Source,
					Path: srcPath,
					Err:  fmt.Sprintf("%s skill %q shadows %s skill", root.Scope, skill.Name, existing.Scope),
				})
			} else if existing.Path != skill.Path {
				fmt.Fprintf(os.Stderr, "skills: %s skill %q at %s overrides %s\n", root.Scope, skill.Name, skill.Path, existing.Path)
			}
			if label != skill.Name {
				r.Skipped[label] = fmt.Sprintf("duplicate skill name %q (%s overrides %s)", skill.Name, root.Scope, existing.Scope)
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

func scopeRank(scope Scope) int {
	switch scope {
	case ScopeSystem:
		return 0
	case ScopeUser:
		return 1
	case ScopeProject:
		return 2
	case ScopePlugin:
		return 3
	default:
		return 0
	}
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
			if e.Type()&os.ModeSymlink != 0 {
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
	reloaded.Scope = s.Scope
	reloaded.Source = s.Source
	reloaded.Path = s.Path
	reloaded.Enabled = s.Enabled
	reloaded.namespace = s.namespace
	reloaded.sourcePath = s.sourcePath
	reloaded.modTime = info.ModTime()
	sum := sha256.Sum256(data)
	reloaded.ContentHash = hex.EncodeToString(sum[:])
	if reloaded.namespace != "" {
		reloaded.Meta.Name = reloaded.namespace + ":" + reloaded.Meta.Name
	}
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
	Metadata    struct {
		Version string `yaml:"version"`
	} `yaml:"metadata"`
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

	version := strings.TrimSpace(fm.Version)
	if version == "" {
		version = strings.TrimSpace(fm.Metadata.Version)
	}
	return Skill{
		Meta: Meta{
			Name:        strings.TrimSpace(fm.Name),
			Description: strings.TrimSpace(fm.Description),
			Version:     version,
			License:     strings.TrimSpace(fm.License),
		},
		Body: strings.TrimSpace(strings.Join(lines[end+1:], "\n")),
	}, nil
}
