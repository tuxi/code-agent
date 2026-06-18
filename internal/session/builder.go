package session

import (
	"code-agent/internal/model"
	"code-agent/internal/prompt"
	"crypto/rand"
	"encoding/hex"
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
// defaultContextWindow / defaultCompactThreshold are the model-agnostic fallback
// budget. Callers that know the model (the CLI) override these via WithBudget;
// tests and any caller that does not care keep the defaults.
const (
	defaultContextWindow    = 128000
	defaultCompactThreshold = 90000
)

type Builder struct {
	WorkspaceRoot string

	ContextWindow    int
	CompactThreshold int

	// SkillsIndex is the L1 skill index (names + descriptions only) appended to
	// the system prompt. Tiny by design; bodies are loaded on demand by the model
	// via load_skill, never baked in here (P6).
	SkillsIndex string
}

func NewBuilder(workspaceRoot string) *Builder {
	return &Builder{
		WorkspaceRoot:    workspaceRoot,
		ContextWindow:    defaultContextWindow,
		CompactThreshold: defaultCompactThreshold,
	}
}

// WithBudget sets the session's context window and compaction threshold, e.g.
// from the selected model's config. Non-positive values leave the default in
// place, so a caller can override just one of them.
func (b *Builder) WithBudget(contextWindow, compactThreshold int) *Builder {
	if contextWindow > 0 {
		b.ContextWindow = contextWindow
	}
	if compactThreshold > 0 {
		b.CompactThreshold = compactThreshold
	}
	return b
}

// WithSkillsIndex sets the L1 skill index appended to the system prompt.
func (b *Builder) WithSkillsIndex(index string) *Builder {
	b.SkillsIndex = index
	return b
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

	// Skills index (L1): the model loads a skill's body on demand via load_skill.
	if idx := strings.TrimSpace(b.SkillsIndex); idx != "" {
		systemContent += "\n\n" + idx
	}

	now := time.Now()
	return &Session{
		ID: newSessionID(),
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: systemContent},
		},
		ContextWindow:    b.ContextWindow,
		CompactThreshold: b.CompactThreshold,
		Metadata:         map[string]any{},
		CreatedAt:        now,
		UpdatedAt:        now,
	}, nil
}

// newSessionID returns a sortable, human-readable, collision-resistant id:
// a UTC timestamp prefix for at-a-glance ordering plus random hex for
// uniqueness within the same second.
func newSessionID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
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
