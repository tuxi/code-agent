package session

import (
	"code-agent/internal/model"
	"code-agent/internal/prompt"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Builder assembles the initial context for a session: the system identity plus
// static project context (AGENTS.md / CLAUDE.md).
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

// contextFile is a loaded project context file with its absolute path and content.
type contextFile struct {
	Path    string
	Content string
}

// contextFileCandidates lists the filenames to search for, in priority order.
// AGENTS.md is the community convention; CLAUDE.md is Claude Code's native format.
var contextFileCandidates = []string{"AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD"}

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
	// When empty, auto-discovered from .codeagent/SYSTEM.md and ~/.codeagent/SYSTEM.md.
	SystemPrompt string

	// AppendSystemPrompt, when non-empty, is appended to the system prompt after
	// the base prompt and context files, but before the skills index. It is never
	// a replacement — even when SystemPrompt overrides the default identity,
	// AppendSystemPrompt still follows. Set programmatically via WithAppendSystemPrompt;
	// unlike SYSTEM.md there is no auto-discovery for this field.
	AppendSystemPrompt string

	// NoContextFiles disables AGENTS.md/CLAUDE.md discovery entirely. Set by the
	// --no-context-files / -nc CLI flag.
	NoContextFiles bool
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

// WithAppendSystemPrompt appends text to the system prompt after the base prompt
// and context files, but before the skills index. Even when WithSystemPrompt is set,
// the append text still follows. Use this for project-specific guidelines that
// should never replace the agent's core identity and tool awareness.
func (b *Builder) WithAppendSystemPrompt(p string) *Builder {
	b.AppendSystemPrompt = p
	return b
}

// WithNoContextFiles disables AGENTS.md/CLAUDE.md discovery. When true, no project
// context files are loaded — not from the workspace, not from ancestors, not from
// the global ~/.codeagent/ directory.
func (b *Builder) WithNoContextFiles(v bool) *Builder {
	b.NoContextFiles = v
	return b
}

func (b *Builder) Build() (*Session, error) {
	// Auto-discover SYSTEM.md / APPEND_SYSTEM.md when not explicitly set.
	b.applyAutoDiscoveredPrompts()

	systemContent := prompt.AgentSystemPrompt
	if b.SystemPrompt != "" {
		systemContent = b.SystemPrompt
	}

	// AppendSystemPrompt always follows the base prompt, even when SystemPrompt
	// overrides it — it adds, never replaces.
	if b.AppendSystemPrompt != "" {
		systemContent += "\n\n" + b.AppendSystemPrompt
	}

	contextFiles, err := b.loadContextFiles()
	if err != nil {
		return nil, err
	}
	if len(contextFiles) > 0 {
		systemContent += "\n\n<project_context>\n\n"
		systemContent += "Project-specific instructions and guidelines:\n\n"
		for _, cf := range contextFiles {
			systemContent += fmt.Sprintf("<project_instructions path=\"%s\">\n%s\n</project_instructions>\n\n", cf.Path, cf.Content)
		}
		systemContent += "</project_context>\n"
	}

	// Skills index (L1): the model loads a skill's body on demand via load_skill.
	if idx := strings.TrimSpace(b.SkillsIndex); idx != "" {
		systemContent += "\n\n" + idx
	}

	// Environment info: workspace path, OS, shell. Git status is injected
	// per-request by the agent loop so it never goes stale mid-session.
	systemContent += b.buildEnvInfo()

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

// loadContextFiles discovers AGENTS.md / CLAUDE.md files from three sources:
//  1. Global: ~/.codeagent/<candidate> (user-level, shared across all projects)
//  2. Ancestor chain: walk up from WorkspaceRoot to the filesystem root, looking
//     for a candidate file in each directory (closest ancestor first)
//
// Deduplication ensures the same absolute path is never loaded twice. A missing
// file at any level is silent — it just means there is no context there.
// When NoContextFiles is true, returns an empty slice immediately.
// Worktree shadow detection prevents double-loading in nested git worktrees:
// when the workspace is a linked worktree under the main repo, the main repo's
// AGENTS.md is skipped because it is already copied into the worktree.
func (b *Builder) loadContextFiles() ([]contextFile, error) {
	if b.NoContextFiles {
		return nil, nil
	}

	var files []contextFile
	seen := make(map[string]bool)

	// 1. Global context file (~/.codeagent/<candidate>).
	if home, err := os.UserHomeDir(); err == nil {
		globalDir := filepath.Join(home, ".codeagent")
		if cf, ok := loadContextFileFromDir(globalDir); ok {
			files = append(files, cf)
			seen[cf.Path] = true
		}
	}

	// 2. Walk ancestor directories from WorkspaceRoot up to the filesystem root.
	// collectAncestors returns [start, parent, grandparent, ..., /].
	// Load closest ancestors first (iterate forward: start → parent → ...).
	// When in a nested git worktree, skip the main repo's AGENTS.md to avoid
	// double-loading: the worktree already has a copy of the same file.
	shadowedPath := findShadowedContextFile(b.WorkspaceRoot)
	ancestors := collectAncestors(b.WorkspaceRoot)
	for _, dir := range ancestors {
		cf, ok := loadContextFileFromDir(dir)
		if !ok || seen[cf.Path] {
			continue
		}
		if shadowedPath != "" && cf.Path == shadowedPath {
			continue
		}
		files = append(files, cf)
		seen[cf.Path] = true
	}

	return files, nil
}

// loadContextFileFromDir looks for the first matching context file in dir.
// Candidates are tried in order (AGENTS.md, CLAUDE.md, etc.); the first one
// that exists as a regular file wins.
func loadContextFileFromDir(dir string) (contextFile, bool) {
	for _, name := range contextFileCandidates {
		p := filepath.Join(dir, name)
		info, err := os.Stat(p)
		if err != nil || info.IsDir() {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		return contextFile{Path: p, Content: strings.TrimSpace(string(data))}, true
	}
	return contextFile{}, false
}

// collectAncestors returns all ancestor directories from start up to the
// filesystem root. start itself is included.
func collectAncestors(start string) []string {
	var dirs []string
	cur := filepath.Clean(start)
	for {
		dirs = append(dirs, cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			break // reached filesystem root
		}
		cur = parent
	}
	return dirs
}

// findShadowedContextFile detects when the workspace is a linked git worktree
// nested under the main repo, and returns the main repo's context file path
// that should be skipped during ancestor traversal.
//
// In a nested worktree scenario:
//
//	/path/to/main/          ← main repo, has AGENTS.md
//	/path/to/main/.git      ← main repo's git directory
//	/path/to/main/worktrees/feat/  ← linked worktree (cwd)
//
// The worktree's .git file points to the main .git, and both the worktree root
// and the main repo root contain the same tracked AGENTS.md. Without shadow
// detection, the ancestor walk would load it twice — once from the worktree
// root and once from the main repo root.
//
// Returns the absolute path of the shadowed context file, or "" when not in a
// nested worktree. Sibling worktrees (git worktree add ../feat) are not
// shadowed because the main repo is not an ancestor.
func findShadowedContextFile(cwd string) string {
	gitPath := filepath.Join(cwd, ".git")
	info, err := os.Stat(gitPath)
	if err != nil || info.IsDir() {
		// Not a linked worktree: .git is a directory (normal repo) or absent.
		return ""
	}

	data, err := os.ReadFile(gitPath)
	if err != nil {
		return ""
	}
	content := strings.TrimSpace(string(data))
	const prefix = "gitdir: "
	if !strings.HasPrefix(content, prefix) {
		return ""
	}
	gitDir := strings.TrimSpace(strings.TrimPrefix(content, prefix))

	// Resolve the common git directory. The gitdir points to something like
	// /path/to/main/.git/worktrees/feat. Read the commondir file to find
	// the actual shared .git directory.
	commonGitDir := resolveCommonGitDir(gitDir)

	// The main repo root is the parent of the common git directory.
	mainRepoRoot := filepath.Dir(commonGitDir)

	// Only shadow when the worktree is nested UNDER the main repo (not a sibling).
	cleanCwd := filepath.Clean(cwd)
	if !strings.HasPrefix(cleanCwd, mainRepoRoot+string(filepath.Separator)) {
		return ""
	}

	// Verify this is truly the main repo's .git directory, not a bare repo or
	// submodule. mainRepoRoot/.git must equal commonGitDir.
	if filepath.Join(mainRepoRoot, ".git") != commonGitDir {
		return ""
	}

	// Return the main repo's context file path if it exists.
	if cf, ok := loadContextFileFromDir(mainRepoRoot); ok {
		return cf.Path
	}
	return ""
}

// resolveCommonGitDir resolves the real shared .git directory from a worktree's
// gitdir. The gitdir (e.g. /main/.git/worktrees/feat) may contain a commondir
// file whose content is the relative path to the real .git directory (e.g. "../.."
// meaning /main/.git). When commondir is absent, the gitdir itself is the
// common directory (only in the main worktree, not a linked one).
func resolveCommonGitDir(gitDir string) string {
	commondirPath := filepath.Join(gitDir, "commondir")
	data, err := os.ReadFile(commondirPath)
	if err != nil {
		// No commondir file: the gitdir IS the common .git directory.
		// But wait — if we're here, we already detected a linked worktree
		// (.git is a file), so this is a linked worktree. The gitdir is
		// something like /main/.git/worktrees/feat, which always has
		// a commondir file. Fall back to the gitdir itself just in case.
		return gitDir
	}
	// commondir contains a relative path like "../.." or "../../.." —
	// resolve it relative to the gitdir.
	rel := strings.TrimSpace(string(data))
	return filepath.Clean(filepath.Join(gitDir, rel))
}

// applyAutoDiscoveredPrompts discovers SYSTEM.md from conventional paths,
// populating SystemPrompt only when it hasn't been explicitly set by the caller.
//
// Discovery order (first found wins):
//
//	.codeagent/SYSTEM.md (project-level)
//	~/.codeagent/SYSTEM.md (global)
//
// Explicit WithSystemPrompt calls always take precedence over auto-discovery.
// When no file is found, the built-in prompt.AgentSystemPrompt is used unchanged.
func (b *Builder) applyAutoDiscoveredPrompts() {
	if b.SystemPrompt == "" {
		b.SystemPrompt = discoverSystemPromptFile(b.WorkspaceRoot)
	}
}

// discoverSystemPromptFile looks for SYSTEM.md at conventional paths.
// Returns the file content as a string, or "" when no file is found.
func discoverSystemPromptFile(workspaceRoot string) string {
	// Project-level first.
	projectPath := filepath.Join(workspaceRoot, ".codeagent", "SYSTEM.md")
	if data, err := os.ReadFile(projectPath); err == nil {
		return strings.TrimSpace(string(data))
	}
	// Global-level fallback.
	if home, err := os.UserHomeDir(); err == nil {
		globalPath := filepath.Join(home, ".codeagent", "SYSTEM.md")
		if data, err := os.ReadFile(globalPath); err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}

// SubAgentPrompt returns the system prompt for the subagent. It looks for
// SUBAGENT_SYSTEM.md at conventional paths; when no file is found it falls
// back to the built-in prompt.SubAgentSystemPrompt.
func SubAgentPrompt(workspaceRoot string) string {
	// Project-level first.
	projectPath := filepath.Join(workspaceRoot, ".codeagent", "SUBAGENT_SYSTEM.md")
	if data, err := os.ReadFile(projectPath); err == nil {
		return strings.TrimSpace(string(data))
	}
	// Global-level fallback.
	if home, err := os.UserHomeDir(); err == nil {
		globalPath := filepath.Join(home, ".codeagent", "SUBAGENT_SYSTEM.md")
		if data, err := os.ReadFile(globalPath); err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	// Built-in default.
	return prompt.SubAgentSystemPrompt
}

// buildEnvInfo returns a concise, static environment snapshot appended to the
// system prompt: workspace path, OS, and shell. Git status is deliberately
// excluded — it is injected per-request by the agent loop (withGitInfo) so it
// never goes stale mid-session.
func (b *Builder) buildEnvInfo() string {
	absRoot, err := filepath.Abs(b.WorkspaceRoot)
	if err != nil {
		absRoot = b.WorkspaceRoot
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("\n\nCurrent workspace: %s", absRoot))
	sb.WriteString(fmt.Sprintf("\nOS: %s", runtime.GOOS))
	if shell := os.Getenv("SHELL"); shell != "" {
		sb.WriteString(fmt.Sprintf(" | Shell: %s", filepath.Base(shell)))
	}
	return sb.String()
}

// GitInfo returns a snapshot of the git repository state at workspaceRoot.
// It runs with a 5-second timeout per command so a hung git never blocks the
// caller. Returns "" when the workspace is not a git repository or git is not
// installed.
func GitInfo(workspaceRoot string) string {
	gitDir := filepath.Join(workspaceRoot, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		return ""
	}

	var lines []string

	if out, err := runGitCmd(workspaceRoot, "rev-parse", "--abbrev-ref", "HEAD"); err == nil && out != "" {
		lines = append(lines, fmt.Sprintf("Git branch: %s", out))
	}
	if out, err := runGitCmd(workspaceRoot, "status", "--porcelain"); err == nil {
		if out == "" {
			lines = append(lines, "Git status: clean")
		} else {
			lines = append(lines, "Git status: modified")
		}
	}
	if out, err := runGitCmd(workspaceRoot, "log", "--oneline", "-5"); err == nil && out != "" {
		lines = append(lines, "Recent commits:")
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			lines = append(lines, "  "+line)
		}
	}

	return strings.Join(lines, "\n")
}

// runGitCmd runs a git command in dir with a 5-second timeout. Errors (non-zero
// exit, timeout, git not installed) return an empty string.
func runGitCmd(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
