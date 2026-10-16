package session

import (
	"context"
	"reflect"
	"testing"
)

func TestMemoryStoreEventPagesWithInterleavedSessions(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	add := func(sessionID, kind string) int64 {
		t.Helper()
		seq, err := store.RecordEvent(ctx, EventRecord{SessionID: sessionID, Kind: kind, Payload: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		return seq
	}
	add("s", "turn_started")  // 1
	add("other", "thinking")  // 2: gap in s
	add("s", "turn_finished") // 3
	add("s", "turn_started")  // 4
	add("s", "thinking")      // 5
	add("s", "turn_finished") // 6
	check := func(before int64, want []int64, more bool) {
		t.Helper()
		recs, hasMore, truncated, err := store.SessionEventPage(ctx, "s", before, 2, 1024)
		if err != nil || truncated || hasMore != more {
			t.Fatalf("before=%d: more=%v truncated=%v err=%v", before, hasMore, truncated, err)
		}
		var seqs []int64
		for _, rec := range recs {
			seqs = append(seqs, rec.Seq)
		}
		if !reflect.DeepEqual(seqs, want) {
			t.Fatalf("before=%d: seqs=%v, want %v", before, seqs, want)
		}
	}
	check(0, []int64{4, 5, 6}, true)
	check(4, []int64{1, 3}, false)
	check(1, nil, false)
}

func TestBuildEventPageLargeTurnRemainsBoundedAndRecoverable(t *testing.T) {
	const count = 10002
	page := func(before int64) ([]EventRecord, bool, error) {
		i := count - 1
		return BuildEventPage(2, 1<<20, func() (EventRecord, bool, error) {
			for i >= 0 && before > 0 && int64(i+1) >= before {
				i--
			}
			if i < 0 {
				return EventRecord{}, false, nil
			}
			rec := EventRecord{Seq: int64(i + 1), Kind: "thinking", Payload: []byte(`{}`)}
			if i == 0 {
				rec.Kind = "turn_started"
			}
			i--
			return rec, true, nil
		})
	}
	recs, truncated, err := page(0)
	if err != nil || !truncated || len(recs) != 10000 {
		t.Fatalf("large turn first page: rows=%d truncated=%v err=%v", len(recs), truncated, err)
	}
	if recs[0].Seq != 3 {
		t.Fatalf("large turn first page oldest=%d, want 3", recs[0].Seq)
	}
	older, olderTruncated, err := page(recs[0].Seq)
	if err != nil || olderTruncated || len(older) != 2 || older[0].Kind != "turn_started" || older[1].Seq != 2 {
		t.Fatalf("large turn older page: rows=%d truncated=%v err=%v", len(older), olderTruncated, err)
	}
}
