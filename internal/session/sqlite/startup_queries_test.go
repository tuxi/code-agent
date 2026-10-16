package sqlite

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"code-agent/internal/session"
)

func TestReopenDoesNotRebuildIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	var before int
	if err = s.db.QueryRow(`PRAGMA schema_version`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var after int
	if err = s.db.QueryRow(`PRAGMA schema_version`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("ordinary reopen changed schema_version: %d -> %d", before, after)
	}
}

func TestMetadataRecoveryAndListPageNeverTouchMessages(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, id := range []string{"a", "b", "c"} {
		sess := sampleSession()
		sess.ID = id
		sess.UpdatedAt = now
		sess.Metadata = map[string]any{"turn_status": "running", "custom": "kept"}
		if err := s.Save(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`CREATE TRIGGER no_message_delete BEFORE DELETE ON messages BEGIN SELECT RAISE(ABORT,'messages must remain untouched'); END`); err != nil {
		t.Fatal(err)
	}
	var changed []string
	s.OnLifecycleUpdate = func(id, status string) {
		if status != "paused" {
			t.Fatalf("status=%s", status)
		}
		changed = append(changed, id)
	}
	if err := s.PauseInterruptedSessions(ctx); err != nil {
		t.Fatal(err)
	}
	if len(changed) != 3 {
		t.Fatalf("recovered=%v", changed)
	}
	// Dropping the message table proves paginated listing/metadata use only sessions.
	if _, err := s.db.Exec(`DROP TABLE messages`); err != nil {
		t.Fatal(err)
	}
	page, next, err := s.ListSessionPage(ctx, false, nil, 2)
	if err != nil || len(page) != 2 || next == nil {
		t.Fatalf("page=%+v next=%+v err=%v", page, next, err)
	}
	if page[0].ID != "c" || page[1].ID != "b" || page[0].TurnStatus != "paused" || page[0].Metadata["custom"] != "kept" {
		t.Fatalf("page=%+v", page)
	}
	last, next, err := s.ListSessionPage(ctx, false, next, 2)
	if err != nil || len(last) != 1 || last[0].ID != "a" || next != nil {
		t.Fatalf("last=%+v next=%+v err=%v", last, next, err)
	}
	metas, err := s.ListMetadata(ctx)
	if err != nil || len(metas) != 3 {
		t.Fatalf("metadata rows=%d err=%v", len(metas), err)
	}
}

func TestAttentionMetadataOnlyBaselineAndDelta(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, r := range []session.EventRecord{
		{SessionID: "a", Kind: "turn_finished", Payload: []byte(`{"text":"large output"}`)},
		{SessionID: "job", Kind: "thinking", Payload: []byte(`{"text":"child output"}`)},
		{SessionID: "a", Kind: "turn_started", Payload: []byte(`{"text":"prompt"}`)},
	} {
		if _, err := s.RecordEvent(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	base, err := s.SessionEventAttention(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Sessions) != 2 || base.Sessions[0].LastSequence != 3 || base.Sessions[0].LatestTerminal.Seq != 1 {
		t.Fatalf("base=%+v", base)
	}
	for _, head := range base.Sessions {
		if len(head.LatestEvent.Payload) != 0 {
			t.Fatal("attention loaded event payload")
		}
	}
	delta, err := s.SessionEventAttention(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta.Sessions) != 1 || !reflect.DeepEqual(delta.Sessions[0], base.Sessions[0]) {
		t.Fatalf("delta=%+v base=%+v", delta, base)
	}
	empty, err := s.SessionEventAttention(ctx, 3)
	if err != nil || len(empty.Sessions) != 0 || empty.LastSequence != 3 {
		t.Fatalf("empty=%+v err=%v", empty, err)
	}
}
