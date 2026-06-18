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
	"strings"
)

type LoadSkillTool struct {
	Skills *skills.Registry
}

func NewLoadSkillTool(reg *skills.Registry) *LoadSkillTool {
	return &LoadSkillTool{Skills: reg}
}

type loadSkillInput struct {
	Name string `json:"name"`
}

func (t *LoadSkillTool) Name() string { return "load_skill" }

func (t *LoadSkillTool) Description() string {
	return "Load a skill's full instructions by name (see the Skills list in the system prompt). " +
		"Call this when the task matches a skill, before proceeding. Read-only."
}

func (t *LoadSkillTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"name": {Type: "string", Description: "The skill name to load, from the Skills list in the system prompt."},
	}, "name").JSON()
}

func (t *LoadSkillTool) Execute(_ context.Context, input json.RawMessage) (tools.ToolResult, error) {
	name := parseName(input)
	if name == "" {
		return tools.ToolResult{}, fmt.Errorf("name is required")
	}
	s, ok := t.Skills.Get(name)
	if !ok {
		return tools.ToolResult{}, fmt.Errorf("unknown skill %q; available: %s", name, t.available())
	}

	// A visible header so a transcript shows WHAT was loaded ("Loaded skill:
	// verify-change (v1)"), not just an anonymous wall of markdown.
	header := "Loaded skill: " + s.Name
	if s.Version != "" {
		header += " (v" + s.Version + ")"
	}
	return tools.ToolResult{Content: header + "\n\n" + s.Body}, nil
}

// AnnounceSkill lets the agent loop emit a versioned skill_loaded event without
// knowing this tool by name (see tools.SkillAnnouncer).
func (t *LoadSkillTool) AnnounceSkill(input json.RawMessage) (string, string, bool) {
	if s, ok := t.Skills.Get(parseName(input)); ok {
		return s.Name, s.Version, true
	}
	return "", "", false
}

func (t *LoadSkillTool) available() string {
	names := make([]string, 0)
	for _, m := range t.Skills.Index() { // Index() is already sorted
		names = append(names, m.Name)
	}
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

func parseName(input json.RawMessage) string {
	var in loadSkillInput
	if len(input) > 0 {
		_ = json.Unmarshal(input, &in)
	}
	return strings.TrimSpace(in.Name)
}

var (
	_ tools.Tool           = (*LoadSkillTool)(nil)
	_ tools.SkillAnnouncer = (*LoadSkillTool)(nil)
)
