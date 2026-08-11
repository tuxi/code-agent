package sessions

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// SearchSession implements the `search_session` tool — keyword search across
// every session the runtime has indexed, returning the best-matching snippets
// so the model can recall how a previous problem was solved without reading
// whole transcripts. Backed by SessionIndex.Search (LIKE substring scan).
type SearchSession struct{}

func (t *SearchSession) Name() string { return "search_session" }

func (t *SearchSession) Description() string {
	return "Search past sessions for a keyword or phrase (a symptom, a library, " +
		"or an error message) and return the best-matching sessions with text " +
		"snippets. Use this to recall how a previous problem was solved, then " +
		"follow up with read_session to read a matching conversation in full. " +
		"Matches session names, compaction summaries, and message content."
}

func (t *SearchSession) InputSchema() json.RawMessage {
	return json.RawMessage(`{
	"type": "object",
	"properties": {
		"query": {
			"type": "string",
			"description": "Keyword or phrase to search for; every whitespace-separated term must appear"
		},
		"limit": {
			"type": "integer",
			"minimum": 1,
			"maximum": 50,
			"description": "Maximum sessions to return (default 10)"
		}
	},
	"required": ["query"],
	"additionalProperties": false
}`)
}

func (t *SearchSession) Execute(ctx context.Context, ec tools.ExecutionContext, raw json.RawMessage) (tools.ToolResult, error) {
	var in struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return tools.ToolResult{}, fmt.Errorf("search_session: parse input: %w", err)
	}
	if strings.TrimSpace(in.Query) == "" {
		return tools.ToolResult{}, fmt.Errorf("search_session: query is required")
	}
	if ec.SessionIndex == nil {
		return tools.ToolResult{}, fmt.Errorf("search_session: session index is not available (index.db may have failed to open)")
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 10
	} else if limit > 50 {
		limit = 50
	}

	results, err := ec.SessionIndex.Search(ctx, in.Query, limit)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("search_session: %w", err)
	}
	if len(results) == 0 {
		return tools.ToolResult{Content: "No sessions matched."}, nil
	}
	out, err := json.Marshal(results)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("search_session: marshal: %w", err)
	}
	return tools.ToolResult{Content: string(out), Output: out}, nil
}
