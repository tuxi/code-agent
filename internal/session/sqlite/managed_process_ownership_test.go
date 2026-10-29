package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"code-agent/internal/collab"
	"code-agent/internal/session"
)

func processParent(t *testing.T, s *Store, id, root string) {
	t.Helper()
	if err := s.Save(context.Background(), &session.Session{ID: id, WorkspacePath: root, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}
func processBracket(t *testing.T, s *Store, partition, kind, child, turn, call string) {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"Kind": kind, "SessionID": child, "TurnID": turn, "CallID": call})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecordEvent(context.Background(), session.EventRecord{SessionID: partition, TurnID: turn, Kind: kind, Payload: raw, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func TestManagedRecoveryProcessOwnership(t *testing.T) {
	for _, name := range []string{"other_job_running", "other_task_running", "other_job_finished", "own_job", "own_nested", "own_symlink", "orphan_finished", "deleted_parent", "wrong_call", "wrong_turn", "wrong_child", "wrong_kind", "ambiguous_owner", "unknown_owner_path", "missing_call"} {
		t.Run(name, func(t *testing.T) {
			h := reviewFixture(t, 1)
			parentRoot := t.TempDir()
			if name == "own_job" {
				parentRoot = h.root
			}
			if name == "own_nested" {
				parentRoot = filepath.Join(h.root, "child")
			}
			if name == "own_symlink" {
				parentRoot = filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(h.root, parentRoot); err != nil {
					t.Fatal(err)
				}
			}
			if name == "unknown_owner_path" {
				parentRoot = ""
			}
			processParent(t, h.s, "other", parentRoot)
			kind := "job_started"
			if name == "other_task_running" {
				kind = "task_started"
			}
			call := "call"
			if name == "missing_call" {
				call = ""
			}
			processBracket(t, h.s, "child", kind, "child", "turn", call)
			parentCall, parentTurn, parentChild, parentKind := call, "turn", "child", kind
			if name == "wrong_call" {
				parentCall = "different"
			}
			if name == "wrong_turn" {
				parentTurn = "different"
			}
			if name == "wrong_child" {
				parentChild = "different"
			}
			if name == "wrong_kind" {
				parentKind = "task_started"
			}
			if name != "orphan_finished" {
				processBracket(t, h.s, "other", parentKind, parentChild, parentTurn, parentCall)
			}
			if name == "deleted_parent" {
				if _, err := h.s.db.Exec(`DELETE FROM sessions WHERE id='other'`); err != nil {
					t.Fatal(err)
				}
			}
			if name == "ambiguous_owner" {
				processParent(t, h.s, "second", t.TempDir())
				processBracket(t, h.s, "second", kind, "child", "turn", call)
			}
			if name == "other_job_finished" || name == "orphan_finished" {
				processBracket(t, h.s, "child", "job_finished", "child", "turn", call)
			}
			check, err := h.r.(collab.RecoveryRepository).ReconcileRun(context.Background(), taskOwner, h.run.RunID)
			wantSafe := name == "other_job_running" || name == "other_task_running" || name == "other_job_finished"
			if err != nil || check.Safe != wantSafe {
				t.Fatal(check, err)
			}
			if !wantSafe && (check.Run.Epoch != "2" || check.Reason != "unproven_background_or_client_process") {
				t.Fatal(check)
			}
		})
	}
}

func TestPrepareRunWithUnrelatedChildHistoryIsIdempotentAndRecoverable(t *testing.T) {
	s, _, m, root, id, in := managedFixture(t)
	cmd := exec.Command("git", "-C", root, "-c", "user.name=Offline", "-c", "user.email=offline@example.invalid", "-c", "core.hooksPath=/dev/null", "commit", "--allow-empty", "-m", "baseline")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	in.ReviewPolicy = &collab.ReviewPolicy{Files: []string{"main.txt"}, ValidationCommands: []string{"git diff --check"}}
	processParent(t, s, "unrelated", t.TempDir())
	processBracket(t, s, "job", "job_started", "job", "old-turn", "old-call")
	processBracket(t, s, "unrelated", "job_started", "job", "old-turn", "old-call")
	ctx := context.Background()
	first, err := m.PrepareRun(ctx, taskOwner, id, in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := m.PrepareRun(ctx, taskOwner, id, in)
	if err != nil || first.ID != again.ID {
		t.Fatal(first, again, err)
	}
	path := s.path
	s.Close()
	reopened, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	r := reopened.TaskRepository("server")
	check, err := r.(collab.RecoveryRepository).ReconcileRun(ctx, taskOwner, first.ID)
	if err != nil || !check.Safe {
		t.Fatal(check, err)
	}
	// Association does not excuse a genuine missing owner, even after reopening.
	processBracket(t, reopened, "unknown", "job_started", "unknown", "turn", "call")
	tx, err := reopened.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = rejectUnprovenWorkspaceProcesses(ctx, tx, root); !errors.Is(err, collab.ErrRecoveryRequired) {
		t.Fatal(err)
	}
}

func TestLegacyFailureTextDoesNotProveNoSideEffectsOrRewriteHistory(t *testing.T) {
	h := reviewFixture(t, 1)
	ctx := context.Background()
	processParent(t, h.s, "legacy", "")
	processBracket(t, h.s, "job_legacy", "job_started", "job_legacy", "old-turn", "old-call")
	processBracket(t, h.s, "legacy", "job_started", "job_legacy", "old-turn", "old-call")
	// Old Runtime diagnostics shared the same stream as arbitrary child stdout.
	// A convincing text line plus exit -1 cannot attest that exec never started.
	for _, record := range []session.EventRecord{
		{SessionID: "job_legacy", Kind: "job_output", Payload: json.RawMessage(`{"Kind":"job_output","SessionID":"job_legacy","Chunk":"failed to start: exec: \"go\": executable file not found in $PATH\n"}`)},
		{SessionID: "job_legacy", TurnID: "old-turn", Kind: "job_finished", Payload: json.RawMessage(`{"Kind":"job_finished","SessionID":"job_legacy","TurnID":"old-turn","CallID":"old-call","Text":"failed","ExitCode":-1}`)},
	} {
		if _, err := h.s.RecordEvent(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	var before int
	if err := h.s.db.QueryRow(`SELECT COUNT(*) FROM session_events`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	originalEvents, err := h.s.SessionEvents(ctx, "job_legacy")
	if err != nil {
		t.Fatal(err)
	}
	check, err := h.r.(collab.RecoveryRepository).ReconcileRun(ctx, taskOwner, h.run.RunID)
	if err != nil || check.Safe || check.Reason != "unproven_background_or_client_process" {
		t.Fatal(check, err)
	}
	var after int
	var workspacePath string
	if err := h.s.db.QueryRow(`SELECT COUNT(*) FROM session_events`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := h.s.db.QueryRow(`SELECT workspace_path FROM sessions WHERE id='legacy'`).Scan(&workspacePath); err != nil {
		t.Fatal(err)
	}
	if before != after || workspacePath != "" {
		t.Fatal("recovery rewrote historical facts", before, after, workspacePath)
	}
	savedEvents, err := h.s.SessionEvents(ctx, "job_legacy")
	if err != nil || !reflect.DeepEqual(originalEvents, savedEvents) {
		t.Fatal("recovery modified original event payloads", err)
	}
}

func TestUnownedConversationWithoutProcessHistoryDoesNotBlockWorkspace(t *testing.T) {
	h := reviewFixture(t, 1)
	processParent(t, h.s, "legacy-chat", "")
	if _, err := h.s.RecordEvent(context.Background(), session.EventRecord{SessionID: "legacy-chat", Kind: "tool_started", Payload: json.RawMessage(`{"ToolName":"read_file","ToolArgs":"{\"path\":\"README.md\"}"}`)}); err != nil {
		t.Fatal(err)
	}
	check, err := h.r.(collab.RecoveryRepository).ReconcileRun(context.Background(), taskOwner, h.run.RunID)
	if err != nil || !check.Safe {
		t.Fatal("ordinary unrelated history blocked execution", check, err)
	}
}
