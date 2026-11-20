package sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"code-agent/internal/session"
)

func queuePriority(revision int64, requestID, operationID string) session.UserQueueControl {
	return session.UserQueueControl{Action: "prioritize", Revision: &revision, RequestID: requestID, OperationID: operationID}
}

func TestUserQueuePriorityPreservesOrderAcrossPagesAndReopen(t *testing.T) {
	s, q, in := queueDeliveryFixture(t)
	ctx := context.Background()
	if _, _, err := q.EnqueueUserMessage(ctx, queueMessage("c")); err != nil {
		t.Fatal(err)
	}
	old, err := q.ListUserQueue(ctx, "owner", nil, 1)
	if err != nil || old.Next == nil {
		t.Fatal(old, err)
	}
	promoteC := queuePriority(old.Revision, "c", "promote-c")
	if err = q.PrioritizeUserQueueMessage(ctx, "owner", "local-owner", promoteC); err != nil {
		t.Fatal(err)
	}
	if _, err = q.ListUserQueue(ctx, "owner", old.Next, 1); !errors.Is(err, session.ErrUserQueueRevision) {
		t.Fatal(err)
	}
	for cursor, i := (*session.UserQueueCursor)(nil), 0; ; i++ {
		page, e := q.ListUserQueue(ctx, "owner", cursor, 1)
		expected := []string{"c", "a", "b"}
		if e != nil || len(page.Items) != 1 || i >= len(expected) || page.Items[0].Input.RequestID != expected[i] || (page.Next == nil) != (i == len(expected)-1) {
			t.Fatal(i, page, e)
		}
		if page.Next == nil {
			break
		}
		cursor = page.Next
	}
	if err = q.PrioritizeUserQueueMessage(ctx, "owner", "local-owner", queuePriority(old.Revision+1, "b", "promote-b")); err != nil {
		t.Fatal(err)
	}
	if err = s.reopen(); err != nil {
		t.Fatal(err)
	}
	if q, err = NewUserQueue(s); err != nil {
		t.Fatal(err)
	}
	if err = q.PrioritizeUserQueueMessage(ctx, "owner", "local-owner", promoteC); err != nil {
		t.Fatal("replay", err)
	}
	if err = q.PrioritizeUserQueueMessage(ctx, "owner", "local-owner", queuePriority(old.Revision, "a", "promote-c")); !errors.Is(err, session.ErrQueuedMessageConflict) {
		t.Fatal(err)
	}
	page, err := q.ListUserQueue(ctx, "owner", nil, 0)
	var ids []string
	var sequences []int64
	for _, item := range page.Items {
		ids = append(ids, item.Input.RequestID)
		sequences = append(sequences, item.Sequence)
	}
	if err != nil || page.Revision != old.Revision+2 || !reflect.DeepEqual(ids, []string{"b", "c", "a"}) ||
		!reflect.DeepEqual(sequences, []int64{2, 3, 1}) {
		t.Fatal(ids, sequences, page.Revision, err)
	}
	waiting, err := q.ReconcileUserQueue(ctx, "owner")
	if err != nil || waiting.Claim != nil || waiting.BlockedReason != "turn_active" {
		t.Fatal(waiting, err)
	}
	advanced, err := q.FinishUserQueueTurn(ctx, queueEnd(in, session.TurnInputCompleted))
	if err != nil || advanced.Claim == nil || advanced.Claim.Message.Input.RequestID != "b" {
		t.Fatal(advanced, err)
	}
	if err = q.PrioritizeUserQueueMessage(ctx, "owner", "local-owner", queuePriority(advanced.Revision, "b", "claimed")); !errors.Is(err, session.ErrUserQueueUnavailable) {
		t.Fatal(err)
	}
	if err = q.PrioritizeUserQueueMessage(ctx, "owner", "local-owner", queuePriority(advanced.Revision, "a", "promote-a")); err != nil {
		t.Fatal(err)
	}
	page, err = q.ListUserQueue(ctx, "owner", nil, 0)
	if err != nil || len(page.Items) != 3 || page.Items[0].State != "claimed" || page.Items[0].Input.RequestID != "b" || page.Items[1].Input.RequestID != "a" {
		t.Fatal(page, err)
	}
}

func TestUserQueuePriorityHonorsPauseAndRevision(t *testing.T) {
	_, q, in := queueDeliveryFixture(t)
	ctx := context.Background()
	if err := q.PrioritizeUserQueueMessage(ctx, "owner", "local-owner", queuePriority(1, "b", "stale")); !errors.Is(err, session.ErrUserQueueRevision) {
		t.Fatal(err)
	}
	if err := q.PrioritizeUserQueueMessage(ctx, "owner", "other-owner", queuePriority(2, "b", "wrong-owner")); !errors.Is(err, session.ErrUserQueueUnavailable) {
		t.Fatal(err)
	}
	paused, err := q.FinishUserQueueTurn(ctx, queueEnd(in, session.TurnInputCancelled))
	if err != nil || paused.State != "paused" {
		t.Fatal(paused, err)
	}
	if err = q.PrioritizeUserQueueMessage(ctx, "owner", "local-owner", queuePriority(paused.Revision, "b", "paused")); !errors.Is(err, session.ErrUserQueueUnavailable) {
		t.Fatal(err)
	}
	page, err := q.ListUserQueue(ctx, "owner", nil, 0)
	if err != nil || page.Revision != paused.Revision || page.Items[0].Input.RequestID != "a" {
		t.Fatal(page, err)
	}
}

func TestUserQueuePriorityAndClaimSerialize(t *testing.T) {
	_, q, in := queueDeliveryFixture(t)
	ctx := context.Background()
	type result struct {
		advance session.UserQueueAdvance
		err     error
	}
	start := make(chan struct{})
	priority := make(chan error, 1)
	finish := make(chan result, 1)
	go func() {
		<-start
		priority <- q.PrioritizeUserQueueMessage(ctx, "owner", "local-owner", queuePriority(2, "b", "priority-race"))
	}()
	go func() {
		<-start
		advance, err := q.FinishUserQueueTurn(ctx, queueEnd(in, session.TurnInputCompleted))
		finish <- result{advance, err}
	}()
	close(start)
	change, finished := <-priority, <-finish
	if finished.err != nil || finished.advance.Claim == nil {
		t.Fatal(finished)
	}
	claimed := finished.advance.Claim.Message.Input.RequestID
	switch {
	case change == nil && claimed == "b":
	case errors.Is(change, session.ErrUserQueueRevision) && claimed == "a":
	default:
		t.Fatal(change, claimed)
	}
}
