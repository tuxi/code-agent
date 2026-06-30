// Package skill provides the load_skill tool — the L2 half of progressive
// disclosure. The model sees the skill index in the system prompt (L1) and calls
// load_skill(name) to pull a skill's full body into context only when the task
// matches it. It is read-only and shares one skills.Registry with the prompt
// index, so a name in the index always resolves here.
package skill

import (
	"code-agent/internal/skills"
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type LoadSkillTool struct {
	Skills     *skills.Registry
	globalDir  string // re-scan fallback: user-level skills directory
	projectDir string // re-scan fallback: project-local skills directory
}

func NewLoadSkillTool(reg *skills.Registry, globalDir, projectDir string) *LoadSkillTool {
	return &LoadSkillTool{Skills: reg, globalDir: globalDir, projectDir: projectDir}
}

type loadSkillInput struct {
	Name     string `json:"name"`
	Resource string `json:"resource"` // optional: relative path to a resource file (e.g. "references/api.md")
}

func (t *LoadSkillTool) Name() string { return "load_skill" }

func (t *LoadSkillTool) Description() string {
	return "Load a skill by name. With only a name, returns the full SKILL.md body. " +
		"Call with an empty name to list all available skills. " +
		"With resource set to a relative path (e.g. \"references/api.md\"), returns that file's content directly. " +
		"Skills are plain Markdown files in the workspace's skills/ directory; you can CREATE a new skill " +
		"yourself by writing a .md file there with create_file. The required format is a YAML frontmatter " +
		"block (---) containing at minimum `name:` and `description:` fields, followed by the markdown body. " +
		"Once written, call load_skill(name) and the new skill is immediately available — no restart needed."
}

func (t *LoadSkillTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"name":     {Type: "string", Description: "The skill name to load. Use an empty string to list available skills."},
		"resource": {Type: "string", Description: "Optional: relative path to a resource file inside the skill (e.g. \"references/api.md\"). When set, returns that file's content instead of the SKILL.md body."},
	}, "name").JSON()
}

func (t *LoadSkillTool) Execute(_ context.Context, _ tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	in := parseInput(input)

	// List mode: empty name → return the available skills index.
	if in.Name == "" {
		return tools.ToolResult{Content: t.availableList()}, nil
	}

	s, ok := t.Skills.Get(in.Name)
	if !ok {
		// Hot-reload: the agent (or user) may have created a new skill file since
		// startup. Re-scan the skills directories and merge any new discoveries into
		// the cached registry so the newly created skill is immediately usable —
		// no restart required. This matches the UX of "create SKILL.md then call
		// load_skill" that Claude Code users expect (though CC achieves it by
		// short-lived processes, not hot-reload).
		if t.globalDir != "" || t.projectDir != "" {
			if fresh, err := skills.Load(t.globalDir, t.projectDir); err == nil {
				t.Skills.Merge(fresh)
				s, ok = t.Skills.Get(in.Name)
			}
		}
	}
	if !ok {
		return tools.ToolResult{}, fmt.Errorf("unknown skill %q; available: %s", in.Name, t.availableNames())
	}

	// Resource mode: return a specific L3 file's content.
	if in.Resource != "" {
		return t.loadResource(s, in.Resource)
	}

	// Body mode: return SKILL.md body + resource manifest.
	return t.loadBody(s), nil
}

// loadBody returns the SKILL.md body with header and optional resource manifest.
func (t *LoadSkillTool) loadBody(s skills.Skill) tools.ToolResult {
	header := "Loaded skill: " + s.Name
	if s.Version != "" {
		header += " (v" + s.Version + ")"
	}
	if s.License != "" {
		header += " [" + s.License + "]"
	}
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n\n")
	b.WriteString(s.Body)

	if len(s.Resources) > 0 {
		b.WriteString("\n\n---\nResources available in this skill:\n")
		for _, res := range s.Resources {
			fmt.Fprintf(&b, "  %s  (%s)\n", res.Path, res.Kind)
		}
	}
	return tools.ToolResult{Content: b.String()}
}

// loadResource reads and returns a single L3 resource file. The resource path is
// resolved relative to the skill directory, and a path-traversal guard rejects any
// result outside the skill root.
func (t *LoadSkillTool) loadResource(s skills.Skill, resource string) (tools.ToolResult, error) {
	if s.Dir == "" {
		return tools.ToolResult{}, fmt.Errorf("skill %q has no resource directory (it is a bare file)", s.Name)
	}
	resolved := filepath.Clean(filepath.Join(s.Dir, resource))
	// Path-traversal guard: the resolved path must stay inside the skill directory.
	if !strings.HasPrefix(resolved, s.Dir+string(filepath.Separator)) {
		return tools.ToolResult{}, fmt.Errorf("resource path %q escapes skill directory", resource)
	}
	content, err := os.ReadFile(resolved)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("resource %q not found in skill %q", resource, s.Name)
	}
	header := fmt.Sprintf("Loaded resource: %s/%s", s.Name, resource)
	return tools.ToolResult{Content: header + "\n\n" + string(content)}, nil
}

// AnnounceSkill lets the agent loop emit a versioned skill_loaded event without
// knowing this tool by name (see tools.SkillAnnouncer).
func (t *LoadSkillTool) AnnounceSkill(input json.RawMessage) (string, string, string, bool) {
	in := parseInput(input)
	if s, ok := t.Skills.Get(in.Name); ok {
		t.Skills.RecordLoad(s.Name)
		return s.Name, s.Version, s.Source, true
	}
	return "", "", "", false
}

func (t *LoadSkillTool) availableNames() string {
	names := make([]string, 0)
	for _, m := range t.Skills.Index() {
		names = append(names, m.Name)
	}
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

// availableList returns a formatted skill index, one line per skill. This is the
// recovery path when the model forgets names — it can call load_skill(name="").
func (t *LoadSkillTool) availableList() string {
	if len(t.Skills.Index()) == 0 {
		return "(no skills loaded)"
	}
	var b strings.Builder
	b.WriteString("Available skills:\n")
	for _, m := range t.Skills.Index() {
		line := fmt.Sprintf("- %s: %s", m.Name, m.Description)
		if m.License != "" {
			line += fmt.Sprintf(" [%s]", m.License)
		}
		fmt.Fprintln(&b, line)
	}
	return strings.TrimRight(b.String(), "\n")
}

func parseInput(input json.RawMessage) loadSkillInput {
	var in loadSkillInput
	if len(input) > 0 {
		_ = json.Unmarshal(input, &in)
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Resource = strings.TrimSpace(in.Resource)
	return in
}

var (
	_ tools.Tool           = (*LoadSkillTool)(nil)
	_ tools.SkillAnnouncer = (*LoadSkillTool)(nil)
)
