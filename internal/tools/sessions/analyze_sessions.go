package sessions

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AnalyzeSessionsTool asks the agent to first create transcript files (by calling
// `codeagent transcripts` via run_command), then read and analyze them. The tool
// itself is a "gateway" — it provides the analysis instructions and context; the
// agent owns reading the transcripts and producing the suggestions.
type AnalyzeSessionsTool struct{}

func (t *AnalyzeSessionsTool) Name() string        { return "analyze_sessions" }
func (t *AnalyzeSessionsTool) Description() string { return analyzeSessionsDesc }
func (t *AnalyzeSessionsTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"since_days": {Type: "integer", Description: "Only analyze sessions updated in the last N days. Omit for all sessions."},
	}).JSON()
}

func (t *AnalyzeSessionsTool) Execute(_ context.Context, ec tools.ExecutionContext, raw json.RawMessage) (tools.ToolResult, error) {
	var input struct {
		SinceDays int `json:"since_days"`
	}
	// Best-effort parse: model-supplied JSON may differ from schema; zero-value defaults are fine.
	_ = json.Unmarshal(raw, &input)

	// Step 1: Tell the agent to generate transcripts.
	cmd := "codeagent transcripts"
	if input.SinceDays > 0 {
		cmd += fmt.Sprintf(" --since %d", input.SinceDays)
	}

	// Step 2: Build the analysis instructions.
	agentsContent := readAgentsContent(ec.WorkspaceRoot)

	var b strings.Builder
	b.WriteString("Run this command to extract session transcripts:\n\n")
	b.WriteString(fmt.Sprintf("  `%s`\n\n", cmd))
	b.WriteString("Then read ALL the transcript files in `./session-transcripts/` ")
	b.WriteString("(use read_file with offset/limit for large files).\n\n")

	if agentsContent != "" {
		b.WriteString("Existing project rules (AGENTS.md):\n\n")
		b.WriteString(agentsContent)
		b.WriteString("\n\n")
	}

	b.WriteString("---\n\n")
	b.WriteString("Identify recurring patterns where the user repeatedly gives similar ")
	b.WriteString("instructions. For each pattern that appears 2+ times, propose:\n\n")
	b.WriteString("- AGENTS.md entry (coding rules, behavior guidelines)\n")
	b.WriteString("- Skill (multi-step workflows with tool usage)\n")
	b.WriteString("- Prompt template (reusable /<name> slash commands)\n\n")
	b.WriteString("Compare against the existing AGENTS.md above. ")
	b.WriteString("Output one pattern per section:\n\n")
	b.WriteString("PATTERN: <name>\nSTATUS: NEW|EXISTING\nTYPE: agents-md|skill|prompt-template\n")
	b.WriteString("FREQUENCY: <N>\nEVIDENCE: \"<exact quotes>\"\nDRAFT: <ready-to-use content>\n---\n")

	return tools.ToolResult{Content: b.String()}, nil
}

func readAgentsContent(root string) string {
	for _, name := range []string{"AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}

const analyzeSessionsDesc = `Analyze past coding sessions to find recurring user feedback patterns and propose automated rules (AGENTS.md entries, skills, or prompt templates). Call this first — it will tell you to run "codeagent transcripts" to extract the data, then read the transcript files and identify patterns that appear 2+ times. Compare against the existing AGENTS.md to determine what's new vs already covered.`
