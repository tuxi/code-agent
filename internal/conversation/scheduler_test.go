package conversation

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTurnScheduler_SerializesSameSession(t *testing.T) {
	s := NewTurnScheduler(2)
	releaseFirst, err := s.Acquire(context.Background(), TurnScheduleRequest{SessionID: "a", WorkspacePath: "/work/a"})
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan func(), 1)
	go func() {
		release, err := s.Acquire(context.Background(), TurnScheduleRequest{SessionID: "a", WorkspacePath: "/work/a"})
		if err != nil {
			t.Errorf("second acquire: %v", err)
			return
		}
		acquired <- release
	}()
	assertNotReady(t, acquired)
	releaseFirst()
	releaseSecond := assertReady(t, acquired)
	releaseSecond()
}

func TestTurnScheduler_LeasesSharedWorkspace(t *testing.T) {
	s := NewTurnScheduler(2)
	releaseA, err := s.Acquire(context.Background(), TurnScheduleRequest{SessionID: "a", WorkspacePath: "/work/shared"})
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan func(), 1)
	go func() {
		release, err := s.Acquire(context.Background(), TurnScheduleRequest{SessionID: "b", WorkspacePath: "/work/shared"})
		if err != nil {
			t.Errorf("second acquire: %v", err)
			return
		}
		acquired <- release
	}()
	assertNotReady(t, acquired)
	releaseA()
	releaseB := assertReady(t, acquired)
	releaseB()
}

func TestTurnScheduler_AllowsDifferentWorkspacesWithinCapacity(t *testing.T) {
	s := NewTurnScheduler(2)
	releaseA, err := s.Acquire(context.Background(), TurnScheduleRequest{SessionID: "a", WorkspacePath: "/work/a"})
	if err != nil {
		t.Fatal(err)
	}
	defer releaseA()

	acquired := make(chan func(), 1)
	go func() {
		release, err := s.Acquire(context.Background(), TurnScheduleRequest{SessionID: "b", WorkspacePath: "/work/b"})
		if err != nil {
			t.Errorf("second acquire: %v", err)
			return
		}
		acquired <- release
	}()
	releaseB := assertReady(t, acquired)
	releaseB()
}

func TestTurnScheduler_AllowsIsolatedWorktreesFromSameBase(t *testing.T) {
	s := NewTurnScheduler(2)
	releaseA, err := s.Acquire(context.Background(), TurnScheduleRequest{SessionID: "a", WorkspacePath: "/repo/worktrees/a", Mode: IsolatedWorktree})
	if err != nil {
		t.Fatal(err)
	}
	defer releaseA()
	acquired := make(chan func(), 1)
	go func() {
		release, err := s.Acquire(context.Background(), TurnScheduleRequest{SessionID: "b", WorkspacePath: "/repo/worktrees/b", Mode: IsolatedWorktree})
		if err != nil {
			t.Errorf("second acquire: %v", err)
			return
		}
		acquired <- release
	}()
	releaseB := assertReady(t, acquired)
	releaseB()
}

func TestTurnScheduler_SerializesSamePathMisdeclaredAsIsolated(t *testing.T) {
	s := NewTurnScheduler(2)
	releaseA, err := s.Acquire(context.Background(), TurnScheduleRequest{SessionID: "a", WorkspacePath: "/repo/worktrees/same", Mode: IsolatedWorktree})
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan func(), 1)
	go func() {
		release, err := s.Acquire(context.Background(), TurnScheduleRequest{SessionID: "b", WorkspacePath: "/repo/worktrees/same", Mode: IsolatedWorktree})
		if err != nil {
			t.Errorf("second acquire: %v", err)
			return
		}
		acquired <- release
	}()
	assertNotReady(t, acquired)
	releaseA()
	releaseB := assertReady(t, acquired)
	releaseB()
}

func TestWorkspaceLeaseKeyCaseFoldsEquivalentPaths(t *testing.T) {
	a := workspaceLeaseKey(TurnScheduleRequest{WorkspacePath: "/Repo/Worktree"})
	b := workspaceLeaseKey(TurnScheduleRequest{WorkspacePath: "/repo/worktree"})
	if a != b {
		t.Fatalf("case variant lease keys differ: %q != %q", a, b)
	}
}

func TestWorkspaceLeaseKeyResolvesSymlinks(t *testing.T) {
	realPath := t.TempDir()
	linkPath := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}
	realKey := workspaceLeaseKey(TurnScheduleRequest{WorkspacePath: realPath})
	linkKey := workspaceLeaseKey(TurnScheduleRequest{WorkspacePath: linkPath, Mode: IsolatedWorktree})
	if realKey != linkKey {
		t.Fatalf("real key %q != symlink key %q", realKey, linkKey)
	}
}

func TestTurnScheduler_CancelQueuedTurn(t *testing.T) {
	s := NewTurnScheduler(1)
	release, err := s.Acquire(context.Background(), TurnScheduleRequest{SessionID: "a", WorkspacePath: "/work/a"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	result := make(chan error, 1)
	go func() {
		_, err := s.Acquire(context.Background(), TurnScheduleRequest{SessionID: "b", WorkspacePath: "/work/b"})
		result <- err
	}()
	deadline := time.Now().Add(time.Second)
	for s.Snapshot().Queued != 1 {
		if time.Now().After(deadline) {
			t.Fatal("turn was not queued")
		}
		time.Sleep(time.Millisecond)
	}
	if !s.Cancel("b") {
		t.Fatal("Cancel returned false")
	}
	if err := assertReady(t, result); err != context.Canceled {
		t.Fatalf("queued acquire error = %v, want context.Canceled", err)
	}
}

func TestTurnScheduler_ActivityReportsRunningAndQueuePosition(t *testing.T) {
	s := NewTurnScheduler(1)
	release, err := s.Acquire(context.Background(), TurnScheduleRequest{SessionID: "running", WorkspacePath: "/work/a"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	queued := make(chan error, 1)
	go func() {
		_, err := s.Acquire(context.Background(), TurnScheduleRequest{SessionID: "queued", WorkspacePath: "/work/b"})
		queued <- err
	}()
	deadline := time.Now().Add(time.Second)
	for s.Snapshot().Queued != 1 {
		if time.Now().After(deadline) {
			t.Fatal("turn was not queued")
		}
		time.Sleep(time.Millisecond)
	}
	activity := s.Activity()
	if len(activity) != 2 || activity[0] != (ScheduledTurnActivity{SessionID: "running", State: "running"}) || activity[1] != (ScheduledTurnActivity{SessionID: "queued", State: "queued", QueuePosition: 1}) {
		t.Fatalf("Activity() = %#v", activity)
	}
	s.Cancel("queued")
	if err := assertReady(t, queued); err != context.Canceled {
		t.Fatalf("queued acquire error = %v", err)
	}
}

func TestTurnScheduler_RoundRobinAcrossSessions(t *testing.T) {
	s := NewTurnScheduler(1)
	releaseA1, err := s.Acquire(context.Background(), TurnScheduleRequest{SessionID: "a", WorkspacePath: "/work/a"})
	if err != nil {
		t.Fatal(err)
	}
	a2 := make(chan func(), 1)
	b1 := make(chan func(), 1)
	go acquireInto(t, s, TurnScheduleRequest{SessionID: "a", WorkspacePath: "/work/a"}, a2)
	go acquireInto(t, s, TurnScheduleRequest{SessionID: "b", WorkspacePath: "/work/b"}, b1)
	deadline := time.Now().Add(time.Second)
	for s.Snapshot().Queued != 2 {
		if time.Now().After(deadline) {
			t.Fatal("turns were not queued")
		}
		time.Sleep(time.Millisecond)
	}
	releaseA1()
	releaseB := assertReady(t, b1)
	assertNotReady(t, a2)
	releaseB()
	releaseA2 := assertReady(t, a2)
	releaseA2()
}

func acquireInto(t *testing.T, s *TurnScheduler, req TurnScheduleRequest, out chan<- func()) {
	t.Helper()
	release, err := s.Acquire(context.Background(), req)
	if err != nil {
		t.Errorf("Acquire: %v", err)
		return
	}
	out <- release
}

func assertNotReady[T any](t *testing.T, ch <-chan T) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("operation completed before its permit was released")
	case <-time.After(20 * time.Millisecond):
	}
}

func assertReady[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for scheduler")
		var zero T
		return zero
	}
}
