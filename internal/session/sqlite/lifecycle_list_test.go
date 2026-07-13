package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"code-agent/internal/session"
)

// TestListSurfacesTurnLifecycle proves the List projection reads turn_status and
// paused_at out of the persisted metadata JSON so a host can enumerate paused
// sessions without loading each one (v1.2 §3.2).
func TestListSurfacesTurnLifecycle(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	paused := sampleSession()
	paused.ID = "paused-1"
	pausedAt := time.Unix(1_700_000_500, 0)
	paused.MarkPaused(pausedAt)
	if err := store.Save(ctx, paused); err != nil {
		t.Fatalf("save paused: %v", err)
	}

	normal := sampleSession()
	normal.ID = "normal-1"
	if err := store.Save(ctx, normal); err != nil {
		t.Fatalf("save normal: %v", err)
	}

	metas, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := map[string]session.Meta{}
	for _, m := range metas {
		byID[m.ID] = m
	}

	if got := byID["paused-1"]; got.TurnStatus != session.TurnStatusPaused {
		t.Errorf("paused session TurnStatus=%q want paused", got.TurnStatus)
	}
	if got := byID["paused-1"]; got.PausedAt != pausedAt.Unix() {
		t.Errorf("paused session PausedAt=%d want %d", got.PausedAt, pausedAt.Unix())
	}
	if got := byID["normal-1"]; got.TurnStatus != "" {
		t.Errorf("normal session TurnStatus=%q want empty", got.TurnStatus)
	}
}

// TestSessionEventsSeqAndSince covers v1.2 §4: RecordEvent returns a monotonic
// seq (rowid), SessionEvents reports it, and SessionEventsSince returns only the
// tail after a given seq — the incremental reconnect replay.
func TestSessionEventsSeqAndSince(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	var seqs []int64
	for i := 0; i < 3; i++ {
		seq, err := store.RecordEvent(ctx, session.EventRecord{
			SessionID: "s", TurnID: "t1", Kind: "thinking", At: time.Now(), Payload: []byte(`{}`),
		})
		if err != nil {
			t.Fatalf("record: %v", err)
		}
		seqs = append(seqs, seq)
	}
	// Seqs are strictly increasing.
	if !(seqs[0] < seqs[1] && seqs[1] < seqs[2]) {
		t.Fatalf("seqs not monotonic: %v", seqs)
	}

	all, err := store.SessionEvents(ctx, "s")
	if err != nil {
		t.Fatalf("session events: %v", err)
	}
	if len(all) != 3 || all[0].Seq != seqs[0] || all[2].Seq != seqs[2] {
		t.Fatalf("SessionEvents seqs = %v, want %v", seqField(all), seqs)
	}

	since, err := store.SessionEventsSince(ctx, "s", seqs[0])
	if err != nil {
		t.Fatalf("session events since: %v", err)
	}
	if len(since) != 2 || since[0].Seq != seqs[1] || since[1].Seq != seqs[2] {
		t.Fatalf("SessionEventsSince(%d) = %v, want tail %v", seqs[0], seqField(since), seqs[1:])
	}
}

func TestSessionEventAttentionSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attention.db")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	base := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	for _, record := range []session.EventRecord{
		{SessionID: "a", TurnID: "turn_old", Kind: "turn_failed", At: base},
		{SessionID: "a", TurnID: "turn_new", Kind: "turn_started", At: base.Add(time.Second)},
		{SessionID: "b", TurnID: "turn_b", Kind: "turn_paused", At: base.Add(2 * time.Second)},
	} {
		if _, err := store.RecordEvent(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snapshot, err := store.SessionEventAttention(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	heads := snapshot.Sessions
	if snapshot.LastSequence != 3 {
		t.Fatalf("cursor=%d want 3", snapshot.LastSequence)
	}
	if len(heads) != 2 || heads[0].LastSequence != 2 || heads[0].LatestEvent == nil || heads[0].LatestEvent.TurnID != "turn_new" || heads[0].LatestTerminal == nil || heads[0].LatestTerminal.TurnID != "turn_old" {
		t.Fatalf("heads=%+v", heads)
	}
	if heads[1].LatestTerminal != nil {
		t.Fatalf("turn_paused must not be terminal: %+v", heads[1])
	}
	delta, err := store.SessionEventAttention(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if delta.LastSequence != 3 || len(delta.Sessions) != 1 || delta.Sessions[0].SessionID != "b" {
		t.Fatalf("delta=%+v", delta)
	}
}

func seqField(recs []session.EventRecord) []int64 {
	out := make([]int64, len(recs))
	for i, r := range recs {
		out[i] = r.Seq
	}
	return out
}
