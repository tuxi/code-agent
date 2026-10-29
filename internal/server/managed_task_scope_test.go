package server

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-agent/internal/collab"
	"code-agent/internal/conversation"
	"code-agent/internal/session"
	sessionsqlite "code-agent/internal/session/sqlite"
)

func TestTaskSnapshotRejectsBridgeScopeBeforeReturningPlan(t *testing.T) {
	ctx := context.Background()
	s, err := sessionsqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	root := t.TempDir()
	for _, id := range []string{"bound", "other"} {
		if err := s.Save(ctx, &session.Session{ID: id, WorkspacePath: root, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	tasks := s.TaskRepository("server")
	a := collab.Principal{ID: "runtime-owner", LocalSessions: true}
	created, err := tasks.Create(ctx, a, collab.CreateInput{RequestID: "task", WorkspaceSessionID: "bound", Goal: "scope"})
	if err != nil {
		t.Fatal(err)
	}
	ext, err := tasks.RegisterExternal(ctx, a, collab.ExternalInput{RequestID: "external", RuntimeKind: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	part, err := tasks.Participate(ctx, a, created.ID, collab.ParticipationInput{RequestID: "planner", Role: "planner", SessionRef: ext})
	if err != nil {
		t.Fatal(err)
	}
	const privatePlan = "PRIVATE_PLAN_MUST_NOT_LEAK_ACROSS_SCOPE"
	if _, err = tasks.Report(ctx, a, created.ID, "plan", collab.ReportInput{RequestID: "plan", ParticipationID: part.ID, SubmissionID: "plan", Iteration: 1, Content: privatePlan}); err != nil {
		t.Fatal(err)
	}
	snap, err := tasks.Snapshot(ctx, a, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	repo := conversation.NewSQLiteRepository(s, 32000, 24000, "default", "", nil)
	auth := ServerAuth{Enabled: true, Token: "offline-scope-token-0123456789abcdef"}
	h := NewMux(repo, &conversation.StoreEventAdapter{Store: s}, nil, MuxOptions{RuntimeInfo: RuntimeInfo{ServerID: "server"}, ServerAuth: auth})
	get := func(query string, want int) {
		t.Helper()
		r := httptest.NewRequest("GET", "/v1/tasks/"+created.ID+query, nil)
		r.Header.Set("Authorization", "Bearer "+auth.Token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want || want != 200 && strings.Contains(w.Body.String(), privatePlan) {
			t.Fatal("scope response leaked a plan or had unexpected status", w.Code, w.Body.String())
		}
	}
	get("?scope_workspace_ref=foreign&scope_session_id=bound", 403)
	get("?scope_workspace_ref="+snap.WorkspaceRef+"&scope_session_id=bound", 200)
	identity, err := tasks.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tasks.Participate(ctx, a, created.ID, collab.ParticipationInput{RequestID: "foreign-local", Role: "executor", SessionRef: collab.SessionRef{Kind: "local", ServerID: identity.ServerID, StoreID: identity.StoreID, SessionID: "other"}}); err != nil {
		t.Fatal(err)
	}
	get("?scope_workspace_ref="+snap.WorkspaceRef+"&scope_session_id=bound", 403)
	get("", 200) // owner access itself has not been broadened or narrowed
}
