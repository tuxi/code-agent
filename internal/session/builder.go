package session

import (
	"code-agent/internal/model"
	"code-agent/internal/prompt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Builder assembles the initial context for a session: the system identity plus
// static project context (CODEAGENT.md).
//
// Important boundary: only STATIC context belongs here. Dynamic context that
// changes during a session — git status, workspace summaries — must not be
// baked into the initial messages, because it would go stale within a single
// session. That kind of context is injected per turn or fetched by the model
// through tools.
type Builder struct {
	WorkspaceRoot string
}

func NewBuilder(workspaceRoot string) *Builder {
	return &Builder{WorkspaceRoot: workspaceRoot}
}

func (b *Builder) Build() (*Session, error) {
	systemContent := prompt.AgentSystemPrompt

	memory, err := b.loadProjectMemory()
	if err != nil {
		return nil, err
	}
	if memory != "" {
		systemContent += "\n\n# Project memory (from CODEAGENT.md)\n\n" + memory
	}

	now := time.Now()
	return &Session{
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: systemContent},
		},
		Metadata:  map[string]any{},
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// loadProjectMemory reads CODEAGENT.md from the workspace root if present. A
// missing file is not an error — it just means there is no project memory.
func (b *Builder) loadProjectMemory() (string, error) {
	path := filepath.Join(b.WorkspaceRoot, "CODEAGENT.md")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
