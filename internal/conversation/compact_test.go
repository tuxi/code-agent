package conversation

import (
	"code-agent/internal/session"
	"context"
	"errors"
	"testing"
)

type manualCompactRunner struct {
	stubRunner
	started, release chan struct{}
	fail             bool
}

func (r *manualCompactRunner) CompactContext(ctx context.Context, s *session.Session) (bool, error) {
	if r.started != nil {
		close(r.started)
		<-r.release
	}
	if r.fail {
		return false, errors.New("summary failure")
	}
	s.Summary = "digest"
	s.RecordCompaction(s.PromptTokens, len(s.Summary))
	return true, nil
}

type manualCompactBuilder struct{ r *manualCompactRunner }

func (b manualCompactBuilder) Build(RuntimeContext) TurnRunner { return b.r }
func TestManualCompactionExcludesTurnsAndPreservesFailure(t *testing.T) {
	repo := newFakeRepo()
	repo.sessions["s"] = &session.Session{ID: "s", PromptTokens: 957379}
	r := &manualCompactRunner{started: make(chan struct{}), release: make(chan struct{})}
	e := NewTurnExecutor(repo, &fakeEventStore{}, NewActiveTurnRegistry(), NewSubscriptionManager(), manualCompactBuilder{r})
	done := make(chan error, 1)
	go func() { _, _, err := e.CompactConversation(context.Background(), "s"); done <- err }()
	<-r.started
	if _, err := e.beginSessionTurn("s"); !errors.Is(err, ErrConversationOperationInProgress) {
		t.Fatalf("turn admitted: %v", err)
	}
	close(r.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if repo.sessions["s"].Summary != "digest" || repo.sessions["s"].Compactions[0].Finalized() {
		t.Fatal("manual compaction must persist a pending measurement")
	}
	r.started = nil
	r.fail = true
	saves := repo.saves
	if _, _, err := e.CompactConversation(context.Background(), "s"); err == nil || repo.saves != saves {
		t.Fatal("failed compaction saved context")
	}
	repo.sessions["s"].SetTurnStatus(session.TurnStatusPaused)
	repo.metas = []session.Meta{{ID: "s", TurnStatus: session.TurnStatusPaused}}
	if _, _, err := e.CompactConversation(context.Background(), "s"); !errors.Is(err, ErrConversationInUse) {
		t.Fatal("paused turn was compacted")
	}
}
