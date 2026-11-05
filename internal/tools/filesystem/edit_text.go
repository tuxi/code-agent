package filesystem

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"code-agent/internal/tools"
	"code-agent/internal/workspace"
)

// editLFView maps byte boundaries in the read view back to the original file.
func editLFView(raw string) (string, []int) {
	var b strings.Builder
	offsets := make([]int, 0, len(raw)+1)
	for i := 0; i < len(raw); i++ {
		offsets = append(offsets, i)
		if raw[i] == '\r' && i+1 < len(raw) && raw[i+1] == '\n' {
			b.WriteByte('\n')
			i++
		} else {
			b.WriteByte(raw[i])
		}
	}
	return b.String(), append(offsets, len(raw))
}

func boundedEditLine(s string) string {
	runes := []rune(s)
	if len(runes) > 300 {
		return string(runes[:300]) + "…"
	}
	return s
}

func editDiagnostic(ec tools.ExecutionContext, input json.RawMessage, content, old, replacement string) string {
	if NewReadFileTool().Inspect(input, ec.WorkspaceRoot) != nil {
		return ""
	}
	var args struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(input, &args) != nil {
		return ""
	}
	root, err := filepath.Abs(ec.WorkspaceRoot)
	if err != nil || workspace.ClassifyPath(root, workspace.ResolveToolPath(root, args.Path)) != workspace.PathInsideWorkspace {
		return ""
	}
	if replacement != "" && strings.Contains(content, replacement) {
		return "The replacement text is already present. Re-read the target before editing again."
	}
	return nearMissDiagnostic(content, old)
}
