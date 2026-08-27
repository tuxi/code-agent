package runtime

import (
	"context"
	"encoding/json"
	"fmt"

	sessionsqlite "code-agent/internal/session/sqlite"
	"code-agent/internal/model"
	"code-agent/internal/tools"
)

// ── Session tool trace (R9 extract input) ───────────────────────────

const (
	traceArgsBudget   = 2048 // per-arg value budget before truncation
	traceResultBudget = 1024 // per tool-result budget
	traceDefaultLimit = 50   // default number of most-recent steps to return
)

// NewSessionTraceFunc returns a tools.SessionTraceFunc that reads the most
// recent tool calls from a session's message history, using the session index
// + per-workspace store (the same wiring check_turn uses). The result is
// truncated so a long phone-automation session does not blow the model's
// context window when R9 extract hands it over for compiling.
func NewSessionTraceFunc() tools.SessionTraceFunc {
	return func(ctx context.Context, sessionID string, limit int) ([]tools.TraceStep, error) {
		if limit <= 0 {
			limit = traceDefaultLimit
		}
		if IndexDB() == nil {
			return nil, fmt.Errorf("session trace: index unavailable")
		}
		entry, err := GetSessionIndex(IndexDB(), sessionID)
		if err != nil || entry == nil {
			return nil, fmt.Errorf("session trace: session %s not found", sessionID)
		}
		storePath := entry.StorePath
		if storePath == "" {
			return nil, fmt.Errorf("session trace: session %s has no store_path", sessionID)
		}
		store, err := sessionsqlite.NewReadOnly(storePath)
		if err != nil {
			return nil, fmt.Errorf("session trace: open store: %w", err)
		}
		defer store.Close()

		sess, err := store.Load(ctx, sessionID)
		if err != nil {
			return nil, fmt.Errorf("session trace: load session: %w", err)
		}
		return extractToolTrace(sess.Messages, limit), nil
	}
}

// extractToolTrace walks a session's messages, pairing each assistant tool
// call with its tool-result message, truncating args/results, and returning
// the most recent `limit` steps.
func extractToolTrace(msgs []model.Message, limit int) []tools.TraceStep {
	results := map[string]string{}
	for _, m := range msgs {
		if m.Role == model.RoleTool && m.ToolCallID != "" {
			results[m.ToolCallID] = truncateText(m.Content, traceResultBudget)
		}
	}

	steps := []tools.TraceStep{}
	for _, m := range msgs {
		if m.Role != model.RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			args := map[string]any{}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				// Provider emitted invalid/hallucinated arguments — keep the raw
				// text so the compiler can still see what was attempted.
				args = map[string]any{"_raw": truncateText(tc.Function.Arguments, traceArgsBudget)}
			}
			steps = append(steps, tools.TraceStep{
				Tool:   tc.Function.Name,
				Args:   truncateArgs(args),
				Result: results[tc.ID],
			})
		}
	}

	if len(steps) > limit {
		steps = steps[len(steps)-limit:]
	}
	return steps
}

func truncateArgs(args map[string]any) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		if s, ok := v.(string); ok {
			out[k] = truncateText(s, traceArgsBudget)
			continue
		}
		out[k] = v
	}
	return out
}

func truncateText(s string, budget int) string {
	if len(s) <= budget {
		return s
	}
	return s[:budget] + "..."
}
