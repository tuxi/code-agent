package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"code-agent/internal/session"
)

func queueFixture(t *testing.T) (*Store, *UserQueue) {
	t.Helper()
	s, _ := runtimeInputFixture(t)
	q, err := NewUserQueue(s)
	if err != nil {
		t.Fatal(err)
	}
	return s, q
}
func queueMessage(id string) session.QueuedMessage {
	return session.QueuedMessage{SessionID: "owner", RequestID: id, Text: "next task", Context: session.QueueExecutionContext{WireModel: "connection/model", ResolvedModel: "model", ReasoningEffort: "high", AuthorizationScope: "local-owner", ApprovalMode: "ask"}}
}

func TestUserQueueIdentityFrozenContextAndReopen(t *testing.T) {
	s, q := queueFixture(t)
	ctx := context.Background()
	first, created, err := q.EnqueueUserMessage(ctx, queueMessage("a"))
	if err != nil || !created || first.State != "queued" || first.Sequence != 1 || first.Revision != 1 {
		t.Fatal(first, created, err)
	}
	second, created, err := q.EnqueueUserMessage(ctx, queueMessage("b"))
	if err != nil || !created || second.Sequence != 2 || second.Revision != 2 {
		t.Fatal(second, created, err)
	}
	duplicate, created, err := q.EnqueueUserMessage(ctx, queueMessage("a"))
	if err != nil || created || !reflect.DeepEqual(first, duplicate) {
		t.Fatal(duplicate, created, err)
	}
	mutations := []func(*session.QueuedMessage){
		func(m *session.QueuedMessage) { m.Text = "different" },
		func(m *session.QueuedMessage) { m.Context.WireModel = "other/model" },
		func(m *session.QueuedMessage) { m.Context.ResolvedModel = "other" },
		func(m *session.QueuedMessage) { m.Context.ReasoningEffort = "low" },
		func(m *session.QueuedMessage) { m.Context.WorkspacePath = "/other" },
		func(m *session.QueuedMessage) { m.Context.AuthorizationScope = "another-owner" },
		func(m *session.QueuedMessage) { m.Context.ApprovalMode = "full" },
	}
	for _, mutate := range mutations {
		m := queueMessage("a")
		mutate(&m)
		if _, _, err = q.EnqueueUserMessage(ctx, m); !errors.Is(err, session.ErrQueuedMessageConflict) {
			t.Fatal(m, err)
		}
	}
	if err = s.reopen(); err != nil {
		t.Fatal(err)
	}
	q, err = NewUserQueue(s)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := q.QueuedUserMessage(ctx, "owner", "a")
	if err != nil || !reflect.DeepEqual(first, recovered) {
		t.Fatal(recovered, err)
	}
	page, err := q.ListUserQueue(ctx, "owner", nil, 0)
	if err != nil || len(page.Items) != 2 || page.Revision != 2 {
		t.Fatal(page, err)
	}
	// Enqueue is independent of both execution admission and prompt history.
	for _, table := range []string{"turn_inputs", "session_events"} {
		var count int
		if err = s.db.QueryRow(`SELECT COUNT(*) FROM ` + table + ` WHERE session_id='owner'`).Scan(&count); err != nil || count != 0 {
			t.Fatal(table, count, err)
		}
	}
	sess, err := s.Load(ctx, "owner")
	if err != nil || len(sess.Messages) != 1 {
		t.Fatal(sess, err)
	}
}

func TestUserQueuePaginationRejectsChangedRevision(t *testing.T) {
	_, q := queueFixture(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		if _, _, err := q.EnqueueUserMessage(ctx, queueMessage(id)); err != nil {
			t.Fatal(err)
		}
	}
	page, err := q.ListUserQueue(ctx, "owner", nil, 1)
	if err != nil || len(page.Items) != 1 || page.Next == nil || page.Items[0].Input.RequestID != "a" {
		t.Fatal(page, err)
	}
	next, err := q.ListUserQueue(ctx, "owner", page.Next, 1)
	if err != nil || len(next.Items) != 1 || next.Items[0].Input.RequestID != "b" || next.Next == nil {
		t.Fatal(next, err)
	}
	last, err := q.ListUserQueue(ctx, "owner", next.Next, 1)
	if err != nil || len(last.Items) != 1 || last.Items[0].Input.RequestID != "c" || last.Next != nil {
		t.Fatal(last, err)
	}
	// Idempotent retries cannot invalidate an existing page.
	if _, _, err = q.EnqueueUserMessage(ctx, queueMessage("a")); err != nil {
		t.Fatal(err)
	}
	if _, err = q.ListUserQueue(ctx, "owner", page.Next, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err = q.EnqueueUserMessage(ctx, queueMessage("d")); err != nil {
		t.Fatal(err)
	}
	if _, err = q.ListUserQueue(ctx, "owner", page.Next, 1); !errors.Is(err, session.ErrUserQueueRevision) {
		t.Fatal(err)
	}
}

func TestUserQueueConcurrentDedupAndOrder(t *testing.T) {
	_, q := queueFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := q.EnqueueUserMessage(ctx, queueMessage("same")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, _, err := q.EnqueueUserMessage(ctx, queueMessage(fmt.Sprint(i))); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	page, err := q.ListUserQueue(ctx, "owner", nil, 100)
	if err != nil || len(page.Items) != 17 || page.Revision != 17 {
		t.Fatal(page, err)
	}
	for i, item := range page.Items {
		if item.Sequence != int64(i+1) || item.Revision != int64(i+1) {
			t.Fatal(i, item)
		}
	}
}

func TestUserQueueRollbackAndLifecycleIsolation(t *testing.T) {
	s, q := queueFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`CREATE TRIGGER reject_queue_revision BEFORE UPDATE ON user_queue_control BEGIN SELECT RAISE(ABORT,'test revision failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.EnqueueUserMessage(ctx, queueMessage("a")); err == nil {
		t.Fatal("expected failed transaction")
	}
	page, err := q.ListUserQueue(ctx, "owner", nil, 0)
	if err != nil || page.Revision != 0 || len(page.Items) != 0 {
		t.Fatal(page, err)
	}
	if _, err = s.db.Exec(`DROP TRIGGER reject_queue_revision`); err != nil {
		t.Fatal(err)
	}
	first, _, err := q.EnqueueUserMessage(ctx, queueMessage("a"))
	if err != nil || first.Sequence != 1 {
		t.Fatal(first, err)
	}
	if _, err = q.QueuedUserMessage(ctx, "other", "a"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if _, err = q.ListUserQueue(ctx, "other", nil, 0); !errors.Is(err, session.ErrUserQueueUnavailable) {
		t.Fatal(err)
	}
	if _, err = s.Archive(ctx, "owner", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, err = q.EnqueueUserMessage(ctx, queueMessage("new")); !errors.Is(err, session.ErrUserQueueUnavailable) {
		t.Fatal(err)
	}
	// A retry can still reconcile a submission accepted before archive.
	if r, created, e := q.EnqueueUserMessage(ctx, queueMessage("a")); e != nil || created || r.Sequence != first.Sequence {
		t.Fatal(r, created, e)
	}
	if err = s.Delete(ctx, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err = q.QueuedUserMessage(ctx, "owner", "a"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	for _, table := range []string{"user_queue_messages", "user_queue_control"} {
		var count int
		if err = s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatal(table, count, err)
		}
	}
}

func TestUserQueueRejectsInvalidOrChangedWorkspace(t *testing.T) {
	s, q := queueFixture(t)
	ctx := context.Background()
	changes := []func(*session.QueuedMessage){
		func(m *session.QueuedMessage) { m.RequestID = " " },
		func(m *session.QueuedMessage) { m.Text = " " },
		func(m *session.QueuedMessage) { m.Text = strings.Repeat("中", 100001) },
		func(m *session.QueuedMessage) { m.Text = string([]byte{0xff}) },
		func(m *session.QueuedMessage) { m.Context.WireModel = "" },
		func(m *session.QueuedMessage) { m.Context.AuthorizationScope = "" },
		func(m *session.QueuedMessage) { m.Context.ApprovalMode = "" },
		func(m *session.QueuedMessage) { m.Context.WorkspaceRoot = "unknown" },
	}
	for _, change := range changes {
		m := queueMessage("bad")
		change(&m)
		if _, _, err := q.EnqueueUserMessage(ctx, m); !errors.Is(err, session.ErrInvalidQueuedMessage) {
			t.Fatal(m.RequestID, err)
		}
	}
	m := queueMessage("workspace")
	m.Context.WorkspacePath = "/another"
	if _, _, err := q.EnqueueUserMessage(ctx, m); !errors.Is(err, session.ErrUserQueueUnavailable) {
		t.Fatal(err)
	}
	sess, err := s.Load(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	sess.WorkspacePath = "/original"
	sess.Workspace = session.WorkspaceRef{Root: session.RootExternal, ExtID: "stable-id"}
	if err = s.Save(ctx, sess); err != nil {
		t.Fatal(err)
	}
	m = queueMessage("workspace")
	m.Context.WorkspacePath = "/original"
	m.Context.WorkspaceRoot = session.RootExternal
	m.Context.WorkspaceExtID = "stable-id"
	if _, _, err = q.EnqueueUserMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
}
