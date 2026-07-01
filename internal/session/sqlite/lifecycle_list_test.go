package sqlite

import (
	"context"
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

func seqField(recs []session.EventRecord) []int64 {
	out := make([]int64, len(recs))
	for i, r := range recs {
		out[i] = r.Seq
	}
	return out
}
