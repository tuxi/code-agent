package automation

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *sqlStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "automations.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sampleAutomation() Automation {
	return Automation{
		Name:         "daily report",
		Prompt:       "summarize",
		Status:       StatusActive,
		ScheduleType: ScheduleRecurring,
		RRule:        "FREQ=MINUTELY;INTERVAL=5",
		Timezone:     "UTC",
		ModeExec:     ModeStandalone,
	}
}

func TestStoreCreateGet(t *testing.T) {
	s := newTestStore(t)
	a := sampleAutomation()
	created, err := s.Create(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" {
		t.Fatal("expected generated id")
	}
	if created.NextRunAt.IsZero() {
		t.Fatal("expected computed next_run_at")
	}
	got, err := s.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != a.Name || got.RRule != a.RRule {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
}

func TestStoreUpdatePartial(t *testing.T) {
	s := newTestStore(t)
	created, _ := s.Create(context.Background(), sampleAutomation())
	// Update only the name; everything else must stay.
	updated, err := s.Update(context.Background(), created.ID, AutomationPatch{Name: strPtr("renamed")})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "renamed" {
		t.Fatalf("name = %q, want renamed", updated.Name)
	}
	if updated.Prompt != "summarize" {
		t.Fatalf("prompt changed unexpectedly: %q", updated.Prompt)
	}
}

func TestStoreSoftDeleteAndList(t *testing.T) {
	s := newTestStore(t)
	a, _ := s.Create(context.Background(), sampleAutomation())
	if err := s.Delete(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty list after delete, got %d", len(list))
	}
	// Get still returns the soft-deleted row (view can inspect).
	if _, err := s.Get(context.Background(), a.ID); err != nil {
		t.Fatalf("get soft-deleted should succeed: %v", err)
	}
}

func TestStoreNextDueAt(t *testing.T) {
	s := newTestStore(t)
	// Due now.
	a, _ := s.Create(context.Background(), sampleAutomation())
	// Pause it.
	paused := StatusPaused
	_, _ = s.Update(context.Background(), a.ID, AutomationPatch{Status: &paused})
	due, err := s.NextDueAt(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("paused automation should not be due, got %d", len(due))
	}
	// Re-activate.
	active := StatusActive
	_, _ = s.Update(context.Background(), a.ID, AutomationPatch{Status: &active})
	due, _ = s.NextDueAt(context.Background(), time.Now().Add(time.Hour))
	if len(due) != 1 {
		t.Fatalf("active automation should be due, got %d", len(due))
	}
}

func TestStoreSkipDue(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Create(context.Background(), sampleAutomation())
	n, err := s.SkipDue(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("skip count = %d, want 1", n)
	}
	runs, _ := s.ListRuns(context.Background(), "auto_0", 10)
	// The run list is empty because we don't know the id; verify via a fresh query.
	_ = runs
	// After skip, next_run_at advanced so it is no longer due.
	due, _ := s.NextDueAt(context.Background(), time.Now().Add(time.Hour))
	if len(due) != 0 {
		t.Fatalf("after skip, should not be due, got %d", len(due))
	}
}

func TestStoreRuns(t *testing.T) {
	s := newTestStore(t)
	a, _ := s.Create(context.Background(), sampleAutomation())
	if err := s.RecordRun(context.Background(), Run{AutomationID: a.ID, Status: RunRunning, SessionID: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListRuns(context.Background(), a.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].SessionID != "sess-1" {
		t.Fatalf("unexpected runs: %+v", runs)
	}
}
