package mcpbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"code-agent/internal/executionevidence"
)

type ControlRef struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	ToolName string `json:"tool_name,omitempty"`
}

type Attention struct {
	SessionID string       `json:"session_id"`
	TurnID    string       `json:"turn_id"`
	Reason    string       `json:"reason"`
	Controls  []ControlRef `json:"controls"`
}

func attentionFor(r ReadResult) *Attention {
	a := &Attention{SessionID: r.SessionID, TurnID: r.TurnID, Reason: r.Status, Controls: []ControlRef{}}
	for _, pending := range r.PendingControls {
		b, _ := json.Marshal(pending)
		var ref ControlRef
		if json.Unmarshal(b, &ref) == nil && ref.ID != "" {
			a.Controls = append(a.Controls, ref)
		}
	}
	sort.Slice(a.Controls, func(i, j int) bool { return a.Controls[i].ID < a.Controls[j].ID })
	return a
}

// AwaitCompletion is the experiment driver's lifecycle boundary. Attention is
// nonterminal and returns control to the human-facing caller; it never grants
// approval or collects artifacts. Only completed permits final artifact reads.
func AwaitCompletion(ctx context.Context, poll func(context.Context) (ReadResult, error), observe func(ReadResult)) (ReadResult, error) {
	for {
		if err := ctx.Err(); err != nil {
			return ReadResult{}, err
		}
		r, err := poll(ctx)
		if err != nil {
			return r, err
		}
		if observe != nil {
			observe(r)
		}
		if r.AttentionRequired || len(r.PendingControls) > 0 || r.Status == "waiting_for_human" || r.Status == "paused" {
			return r, fmt.Errorf("attention_required: session=%s turn=%s status=%s; execution remains nonterminal", r.SessionID, r.TurnID, r.Status)
		}
		switch r.Status {
		case "completed":
			return r, nil
		case "failed", "cancelled":
			return r, fmt.Errorf("turn %s: session=%s turn=%s", r.Status, r.SessionID, r.TurnID)
		case "running", "queued", "waiting":
		default:
			return r, fmt.Errorf("unknown turn status %q; no final artifacts collected", r.Status)
		}
		select {
		case <-ctx.Done():
			return r, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func developmentEvidence(events []Event, turnID string, expected executionevidence.Expectations, status string, pending bool) executionevidence.Report {
	var recorded []executionevidence.Event
	for _, event := range events {
		if event.TurnID != turnID {
			continue
		}
		var input []byte
		if text, ok := event.ToolArgs.(string); ok {
			input = []byte(text)
		} else if event.ToolArgs != nil {
			input, _ = json.Marshal(event.ToolArgs)
		}
		var output []byte
		if event.Output != nil {
			output, _ = json.Marshal(event.Output)
		}
		recorded = append(recorded, executionevidence.Event{Kind: event.Kind, CallID: event.CallID, ToolName: event.ToolName,
			Input: input, Output: output, Observation: event.Observation, Err: event.Err})
	}
	return executionevidence.Check(recorded, expected, status, pending)
}
