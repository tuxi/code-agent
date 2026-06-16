package session

import (
	"code-agent/internal/model"
	"code-agent/internal/prompt"
	"time"
)

// Builder assembles the initial context for a session: the system identity and
// (later) static project context such as CODEAGENT.md.
//
// Important boundary: only STATIC context belongs here. Dynamic context that
// changes during a session — git status, workspace summaries — must not be
// baked into the initial messages, because it would go stale within a single
// session. That kind of context is injected per turn or fetched by the model
// through tools, and is handled elsewhere.
type Builder struct {
	WorkspaceRoot string
}

func NewBuilder(workspaceRoot string) *Builder {
	return &Builder{WorkspaceRoot: workspaceRoot}
}

func (b *Builder) Build() (*Session, error) {
	systemContent := prompt.AgentSystemPrompt

	// P2.4: load CODEAGENT.md from the workspace root and append it here as
	// static project memory. No change to this signature is needed.

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
