package sqlite

import (
	"context"
	"strings"
	"testing"
	"time"

	"code-agent/internal/session"
)

func TestEventPageRangeUsesSessionSeqIndex(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		sid := "s"
		if i%2 == 0 {
			sid = "other"
		}
		if _, err := store.RecordEvent(ctx, session.EventRecord{SessionID: sid, Kind: "thinking", At: time.Now(), Payload: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{
		`EXPLAIN QUERY PLAN SELECT id, session_id, turn_id, kind, at, COALESCE(payload, '') FROM session_events WHERE session_id=? ORDER BY id DESC`,
		`EXPLAIN QUERY PLAN SELECT id, session_id, turn_id, kind, at, COALESCE(payload, '') FROM session_events WHERE session_id=? AND id<? ORDER BY id DESC`,
	} {
		args := []any{"s"}
		if strings.Contains(query, "id<?") {
			args = append(args, 20)
		}
		rows, err := store.db.QueryContext(ctx, query, args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			plan += detail
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		_ = rows.Close()
		if !strings.Contains(plan, "idx_session_events_session_seq") || strings.Contains(plan, "TEMP B-TREE") {
			t.Fatalf("pagination query plan %q does not use seq index", plan)
		}
	}
}

func TestSessionEventSummaryUsesSeqOrder(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	newerTime := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	olderTime := newerTime.Add(-time.Hour)
	for _, record := range []session.EventRecord{
		{SessionID: "s", Kind: "turn_started", At: newerTime, Payload: []byte(`{}`)},
		{SessionID: "other", Kind: "turn_started", At: newerTime, Payload: []byte(`{}`)},
		{SessionID: "s", Kind: "turn_finished", At: olderTime, Payload: []byte(`{}`)},
	} {
		if _, err := store.RecordEvent(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := store.SessionEventSummary(ctx, "s")
	if err != nil || !summary.HasEvents || summary.TurnCount != 1 || summary.MessageCount != 2 || !summary.FirstAt.Equal(newerTime) || !summary.LastAt.Equal(olderTime) {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
}
