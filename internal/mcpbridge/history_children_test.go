//go:build unix

package mcpbridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestHistoryParentChildBrackets(t *testing.T) {
	for _, kind := range []string{"job_started", "job_finished", "task_started", "task_finished"} {
		for _, stamped := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/legacy", true: "/stamped"}[stamped], func(t *testing.T) {
				s, f, _, _ := historyFixture(t)
				f.events[1] = map[string]any{"seq": int64(2), "kind": kind, "session_id": "child-1", "turn_id": "t1", "call_id": "call1"}
				if stamped {
					f.events[1]["parent_session_id"] = "recent"
				}
				page, err := s.ReadHistory(context.Background(), HistoryReadInput{Workspace: f.readRoot, SessionID: "recent"})
				if err != nil {
					t.Fatal(err)
				}
				event := page.Events[1]
				if event.SessionID != "child-1" || event.ParentSessionID != "recent" || event.Seq != 2 {
					t.Fatal(event)
				}
				if f.writes != 0 || f.requests != 7 {
					t.Fatalf("unexpected requests: %d writes: %d", f.requests, f.writes)
				}
			})
		}
	}
}

func TestHistoryRejectsUnrelatedChildEvents(t *testing.T) {
	for _, kind := range []string{"job_output", "tool_started", "turn_finished", "unknown_kind"} {
		t.Run(kind, func(t *testing.T) {
			s, f, _, _ := historyFixture(t)
			f.events[1]["session_id"] = "foreign"
			f.events[1]["kind"] = kind
			_, err := s.ReadHistory(context.Background(), HistoryReadInput{Workspace: f.readRoot, SessionID: "recent"})
			requireDiagnostic(t, err, "evidence_incomplete")
		})
	}
	for _, value := range []any{"foreign", 123} {
		s, f, _, _ := historyFixture(t)
		f.events[1]["kind"] = "job_started"
		f.events[1]["session_id"] = "child-1"
		f.events[1]["parent_session_id"] = value
		_, err := s.ReadHistory(context.Background(), HistoryReadInput{Workspace: f.readRoot, SessionID: "recent"})
		requireDiagnostic(t, err, "evidence_incomplete")
	}
}

func TestHistoryContinuationFacts(t *testing.T) {
	for _, state := range []string{"waiting", "resuming", "needs_attention"} {
		t.Run(state, func(t *testing.T) {
			s, f, _, _ := historyFixture(t)
			f.events[1]["background"] = map[string]any{"turn_id": "t1", "state": state, "revision": 7, "job_ids": []string{"job_1", "job_2"}, "reason": strings.Repeat("界", 1000), "checkpoint": "private-data"}
			page, err := s.ReadHistory(context.Background(), HistoryReadInput{Workspace: f.readRoot, SessionID: "recent"})
			if err != nil {
				t.Fatal(err)
			}
			b := page.Events[1].Background
			if b == nil || b.TurnID != "t1" || b.State != state || b.Revision != 7 || len(b.JobIDs) != 2 || !b.ReasonTruncated || len(b.Reason) > 2048 || !page.Truncated {
				t.Fatal(b)
			}
			raw, _ := json.Marshal(page)
			if strings.Contains(string(raw), "private-data") {
				t.Fatal("leaked unprojected continuation data")
			}
		})
	}
	for _, bad := range []map[string]any{
		{"turn_id": "foreign", "state": "waiting", "revision": 1},
		{"turn_id": "t1", "state": "waiting", "revision": 0},
		{"turn_id": "t1", "state": "waiting", "revision": 1, "job_ids": []string{"../bad"}},
		{"turn_id": "t1", "state": "waiting", "revision": 1, "job_ids": make([]string, 65)},
	} {
		s, f, _, _ := historyFixture(t)
		f.events[1]["background"] = bad
		_, err := s.ReadHistory(context.Background(), HistoryReadInput{Workspace: f.readRoot, SessionID: "recent"})
		requireDiagnostic(t, err, "evidence_incomplete")
	}
}
