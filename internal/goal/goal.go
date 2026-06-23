// Package goal implements code-agent's /goal autonomous-pursuit capability:
// set a verifiable completion condition, then loop plan→act→check until it is
// met, paused, over budget, or stuck — without a per-round human nudge.
//
// Forms (locked in docs/p9-code-agent-goal.md §11):
//   - Judge separation (Claude Code): the worker never grades itself; a separate
//     cheap model reads only the surfaced Evidence and never runs commands —
//     commands are always the worker's job, on its own sandbox/approval path.
//   - Thread state (Codex): a goal is the session's single current state (a
//     gauge, not a log), persisted last-write-wins in session.Metadata.
//   - Two token lines: Spend.Tokens is a COUNTER (agent.TurnResult.TokensUsed);
//     compaction stays on the gauge (session.PromptTokens). Shared source, not
//     shared accumulator.
package goal

import (
	"encoding/json"
	"fmt"
	"time"

	"code-agent/internal/session"
)

// Status is the goal lifecycle state. Exactly one value holds at any moment —
// this is what makes a goal a gauge (suited to a metadata blob), not an event log.
type Status string

const (
	StatusActive        Status = "active"
	StatusAchieved      Status = "achieved"
	StatusPaused        Status = "paused"
	StatusBudgetLimited Status = "budget_limited"
	// StatusBlocked is a TASK-level dead end: the judge sees no viable path, or the
	// agent stopped making progress. It needs a human to rethink the task.
	StatusBlocked Status = "blocked"
	// StatusErrored is an INFRASTRUCTURE failure: the model/transport kept failing
	// (auth, repeated transport errors). It is distinct from blocked so a headless
	// driver can map it to a different exit code — "API key is wrong" is not "the
	// task is impossible".
	StatusErrored Status = "errored"
	StatusCleared Status = "cleared"
)

// metadataKey is the session.Metadata key under which the current goal lives.
const metadataKey = "goal"

// Goal is the persistent thread-state. It is JSON-encoded into
// session.Metadata[metadataKey] and saved on the same store.Save the REPL
// already does each turn, so the goal and its session share one lifecycle.
// SessionID is the thread identity; there is deliberately no separate goalID
// (single goal per session in Phase 1 — §9.4), so resume keys off the session.
type Goal struct {
	SessionID string `json:"session_id"`
	Objective string `json:"objective"` // 终态 + 验证 + 约束;建议上限 4000 字符
	Status    Status `json:"status"`
	Budget    Budget `json:"budget"`
	Spent     Spend  `json:"spent"`

	// CheckerNote is the judge's last "why not yet" — the loop's GRADIENT. Only the
	// checker writes it; the continuation prompt feeds it back to the next worker
	// turn. Persisted, so a resume continues with direction rather than re-anchoring.
	CheckerNote string `json:"checker_note,omitempty"`
	// StatusNote is a human-facing terminal/boundary note (paused / budget /
	// blocked reason). settle writes it; it is NEVER fed back as a gradient, so it
	// cannot clobber CheckerNote.
	StatusNote string `json:"status_note,omitempty"`

	// Stall tracking is unexported, so it is NOT persisted: a resumed run gets a
	// fresh no-progress window rather than instantly tripping blocked on the
	// pre-pause fingerprint.
	stallSig   string
	stallCount int

	// diff is this turn's full workspace git diff, captured by the engine before
	// the judge runs and fed to the LLM checker. It is the anti-gaming signal:
	// the judge sees ALL file changes, not just what the worker chose to surface,
	// so objective constraints ("don't modify the tests") are checkable. Unexported
	// → recomputed each turn, never persisted.
	diff string

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Budget is the mandatory stop ceiling — no unbounded runs (cost/runaway guard).
type Budget struct {
	MaxTurns  int           `json:"max_turns,omitempty"`
	MaxTokens int           `json:"max_tokens,omitempty"` // worker tokens only; checker undercount is known debt (§11.6)
	MaxWall   time.Duration `json:"max_wall,omitempty"`
}

// Spend is the accumulated cost. Tokens is a COUNTER fed by TurnResult.TokensUsed.
type Spend struct {
	Turns  int           `json:"turns"`
	Tokens int           `json:"tokens"`
	Wall   time.Duration `json:"wall"`
}

// overBudget reports whether any ceiling is hit, with a human reason for the
// budget_limited note.
func (g *Goal) overBudget() (bool, string) {
	b := g.Budget
	switch {
	case b.MaxTurns > 0 && g.Spent.Turns >= b.MaxTurns:
		return true, fmt.Sprintf("reached MaxTurns (%d)", b.MaxTurns)
	case b.MaxTokens > 0 && g.Spent.Tokens >= b.MaxTokens:
		return true, fmt.Sprintf("reached MaxTokens (%d)", b.MaxTokens)
	case b.MaxWall > 0 && g.Spent.Wall >= b.MaxWall:
		return true, fmt.Sprintf("reached MaxWall (%s)", b.MaxWall)
	}
	return false, ""
}

// IntoSession stores g as the session's current goal (last-write-wins).
func (g *Goal) IntoSession(sess *session.Session) {
	if sess.Metadata == nil {
		sess.Metadata = map[string]any{}
	}
	sess.Metadata[metadataKey] = g
}

// FromSession decodes the session's current goal, or (nil, nil) if there is
// none. After a store Load the value is a map[string]any (JSON round-trip), so
// we re-marshal it back through the typed struct.
func FromSession(sess *session.Session) (*Goal, error) {
	raw, ok := sess.Metadata[metadataKey]
	if !ok || raw == nil {
		return nil, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var g Goal
	if err := json.Unmarshal(b, &g); err != nil {
		return nil, fmt.Errorf("decode goal from session metadata: %w", err)
	}
	// Guard against a goal blob copied into the wrong session (e.g. metadata
	// duplicated across sessions): the goal must belong to the session it rode in on.
	if g.SessionID != "" && sess.ID != "" && g.SessionID != sess.ID {
		return nil, fmt.Errorf("goal session mismatch: goal=%q session=%q", g.SessionID, sess.ID)
	}
	return &g, nil
}

// Clear removes the active goal from the session — the command-layer `/goal clear`.
// The caller persists the session afterwards. (Phase 1 drops the active pointer;
// achieved-goal archiving per §11.2 is a later addition.)
func Clear(sess *session.Session) {
	delete(sess.Metadata, metadataKey)
}
