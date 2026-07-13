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
	SessionID     string

	ContextWindow    int
	CompactThreshold int

	// SkillsIndex is the L1 skill index (names + descriptions only) appended to
	// the system prompt. Tiny by design; bodies are loaded on demand by the model
	// via load_skill, never baked in here (P6).
	SkillsIndex string

	// SystemPrompt, when non-empty, replaces the default agent identity
	// (prompt.AgentSystemPrompt). A read-only subagent (8.3) uses this to install
	// its own short, strict instructions in place of the full interactive-agent
	// prompt. Project memory and the skills index, if present, are still appended.
	SystemPrompt string
}

// WithID installs a pre-reserved durable identity. Managed worktree creation
// uses it so the reservation, checkout and conversation share one id across
// retries and process restarts.
func (b *Builder) WithID(id string) *Builder {
	b.SessionID = id
	return b
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

// WithSystemPrompt overrides the default agent system identity. Empty leaves the
// default in place. Used by the subagent (8.3) to run with its own focused prompt.
func (b *Builder) WithSystemPrompt(p string) *Builder {
	b.SystemPrompt = p
	return b
}

func (b *Builder) Build() (*Session, error) {
	systemContent := prompt.AgentSystemPrompt
	if b.SystemPrompt != "" {
		systemContent = b.SystemPrompt
	}

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

	// Deliberately NO current date here: the system message is persisted with the
	// session, so a baked-in date goes stale the moment a session spans midnight
	// (or is resumed days later). The agent loop appends today's date ephemerally
	// on every model call instead (agent.withCurrentDate).
	now := time.Now()
	id := b.SessionID
	if id == "" {
		id = NewID()
	}
	return &Session{
		ID: id,
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
func NewID() string {
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
