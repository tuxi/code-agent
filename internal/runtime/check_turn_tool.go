// Package runtime — check_turn tool for flux fallback_poll.
//
// Implements tools.Tool so it's registered in the code-agent registry and
// automatically wrapped as a flux tool by projectFluxTools. Used by
// cross_workspace_collaboration_v1 child workflows as the AwaitBinding
// fallback_poll tool.

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	fluxtool "github.com/tuxi/flux-workflow/tool"

	sessionsqlite "code-agent/internal/session/sqlite"
	"code-agent/internal/tools"
)

// CheckTurnTool checks whether a dispatched cross-session turn has completed.
// When used as a flux fallback_poll tool it returns an error while the turn
// is still running (causing the poll worker to retry later), and a structured
// result when the turn reaches a terminal state (completing the await binding).
type CheckTurnTool struct{}

func (*CheckTurnTool) Name() string        { return "check_turn" }
func (*CheckTurnTool) Description() string { return "Check whether a dispatched cross-session turn has completed." }
func (*CheckTurnTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
	"type": "object",
	"properties": {
		"session_id": {"type": "string"},
		"turn_id":    {"type": "string"},
		"cursor":     {"type": "integer", "minimum": 0}
	},
	"required": ["session_id", "turn_id", "cursor"],
	"additionalProperties": false
}`)
}

type checkTurnInput struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	Cursor    int64  `json:"cursor"`
}

// fluxToolAdapter wraps CheckTurnTool as a fluxtool.Tool so it can be registered
// in the flux runtime's main tool registry. Unlike codeAgentFluxTool, this does
// not require an ExecutionContext or NestedExecutor — it calls CheckTurnTool
// directly because check_turn reads the event store directly via IndexDB/OpenStore.
type checkTurnFluxAdapter struct{}

func (checkTurnFluxAdapter) Name() string                   { return "check_turn" }
func (checkTurnFluxAdapter) Description() string            { return "Check whether a dispatched cross-session turn has completed." }
func (checkTurnFluxAdapter) Mode() fluxtool.ExecutionMode   { return fluxtool.SyncExecution }

func (checkTurnFluxAdapter) InputSchema() fluxtool.DataSchema {
	return fluxDataSchema(json.RawMessage(`{
	"type": "object",
	"properties": {
		"session_id": {"type": "string"},
		"turn_id":    {"type": "string"},
		"cursor":     {"type": "integer", "minimum": 0}
	},
	"required": ["session_id", "turn_id", "cursor"]
}`))
}

func (checkTurnFluxAdapter) OutputSchema() fluxtool.DataSchema {
	return fluxtool.DataSchema{Fields: map[string]fluxtool.FieldSchema{}}
}

func (checkTurnFluxAdapter) Execute(ctx context.Context, input map[string]any, _ fluxtool.ToolEmitter) (*fluxtool.Result, error) {
	raw, _ := json.Marshal(input)
	fmt.Fprintf(os.Stderr, "[check_turn] input=%s\n", string(raw))
	result, err := (&CheckTurnTool{}).Execute(ctx, tools.ExecutionContext{}, raw)
	if err != nil {
		return fluxtool.Fail(err), nil
	}
	return fluxtool.Success(fluxToolResultData(result)), nil
}

func (*CheckTurnTool) Execute(ctx context.Context, _ tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	var in checkTurnInput
	if err := json.Unmarshal(input, &in); err != nil {
		return tools.ToolResult{}, fmt.Errorf("check_turn: %w", err)
	}
	if in.SessionID == "" || in.TurnID == "" {
		return tools.ToolResult{}, fmt.Errorf("check_turn: session_id and turn_id required")
	}

	db := IndexDB()
	if db == nil {
		return tools.ToolResult{}, fmt.Errorf("check_turn: index unavailable")
	}
	entry, err := GetSessionIndex(db, in.SessionID)
	if err != nil || entry == nil {
		return tools.ToolResult{}, fmt.Errorf("check_turn: session %s not found", in.SessionID)
	}

	// Use the index's store_path — it records the exact sessions.db file
	// where this session was persisted (the owning runtime's store), not
	// a path derived from WorkspacePath (which may resolve to a different
	// DB if the session was routed through the supervisor's process).
	storePath := entry.StorePath
	if storePath == "" {
		return tools.ToolResult{}, fmt.Errorf("check_turn: session %s has no store_path in index", in.SessionID)
	}
	store, err := sessionsqlite.NewReadOnly(storePath)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("check_turn: open store read-only: %w", err)
	}
	defer store.Close()

	records, err := store.SessionEventsSince(ctx, in.SessionID, in.Cursor)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("check_turn: events: %w", err)
	}
	for _, r := range records {
		if r.TurnID != in.TurnID {
			continue
		}
		switch r.Kind {
		case "turn_finished":
			out, _ := json.Marshal(map[string]any{
				"session_id": in.SessionID,
				"turn_id":    in.TurnID,
				"status":     "completed",
				"cursor":     r.Seq,
			})
			return tools.ToolResult{Content: "turn completed", Output: out}, nil
		case "turn_failed", "turn_cancelled":
			return tools.ToolResult{}, fmt.Errorf("turn %s %s", in.TurnID, r.Kind)
		}
	}
	return tools.ToolResult{}, fmt.Errorf("turn %s still running", in.TurnID)
}
