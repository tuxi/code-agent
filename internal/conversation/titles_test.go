package conversation

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/credential"
	"code-agent/internal/session"
	"code-agent/internal/session/sqlite"
	"code-agent/internal/settings"
)

type titleGeneratorFunc func(context.Context, string, string, string) (string, error)

func (f titleGeneratorFunc) GenerateTitle(ctx context.Context, id, input, assistant string) (string, error) {
	return f(ctx, id, input, assistant)
}

type titleRunBuilder struct{ runner TurnRunner }

func (b titleRunBuilder) Build(RuntimeContext) TurnRunner { return b.runner }

func (b titleRunBuilder) ResolveModel(alias string) (*settings.ModelConfig, error) {
	return &settings.ModelConfig{Name: alias, Model: "selected-wire-model"}, nil
}

type titleBuilderFunc func(settings.ModelConfig, bool, credential.Resolver) (TitleGenerator, error)

func (f titleBuilderFunc) BuildTitleGenerator(mc settings.ModelConfig, explicit bool, cred credential.Resolver) (TitleGenerator, error) {
	return f(mc, explicit, cred)
}

type pendingTitleTurn struct {
	stubRunner
	finish <-chan struct{}
}

func (r pendingTitleTurn) RunPreparedTurn(ctx context.Context, _ *session.Session) (agent.TurnResult, error) {
	select {
	case <-r.finish:
		return agent.TurnResult{}, errors.New("main model failed")
	case <-ctx.Done():
		return agent.TurnResult{}, ctx.Err()
	}
}

func TestTitleFromAcceptedInputBeforeFirstTurnReturns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := sqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repo := NewSQLiteRepository(store, 1000, 800, "test", "", nil)
	sess, err := repo.Create(ctx, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	finish := make(chan struct{})
	runner := &pendingTitleTurn{finish: finish}
	ex := NewTurnExecutor(repo, &StoreEventAdapter{Store: store}, NewActiveTurnRegistry(), NewSubscriptionManager(), titleRunBuilder{runner})
	defer ex.Shutdown()
	target := credential.Target{Namespace: "llm", Name: "selected"}
	ex.SetSessionCredential(sess.ID, credential.StaticResolver{target: {Type: credential.Bearer, Secret: "test-key"}})
	ex.SetTurnTitleBuilder(titleBuilderFunc(func(mc settings.ModelConfig, explicit bool, cred credential.Resolver) (TitleGenerator, error) {
		if mc.Name != "selected-alias" || mc.Model != "selected-wire-model" || !explicit || cred == nil {
			return nil, errors.New("expected the accepted turn's model and credential")
		}
		resolved, err := cred.Resolve(ctx, target)
		if err != nil || resolved.Secret != "test-key" {
			return nil, errors.New("expected the session credential")
		}
		return titleGeneratorFunc(func(_ context.Context, _, input, assistant string) (string, error) {
			if !strings.Contains(input, "为什么健康检查失败") || assistant != "" {
				return "", errors.New("expected the user's question before the assistant reply")
			}
			return "排查健康检查失败", nil
		}), nil
	}))
	events, unsubscribe := ex.Subscribe(sess.ID)
	defer unsubscribe()
	done := make(chan error, 1)
	go func() {
		input := strings.Repeat("[INFO] 健康检查正在运行\n", 500) + "为什么健康检查失败？"
		_, turnErr := ex.ExecuteWithRequestID(ctx, sess.ID, "first", input, "selected-alias", "")
		done <- turnErr
	}()

	var nameEvent agent.Event
	for nameEvent.Kind != agent.EventConversationNameUpdated {
		select {
		case nameEvent = <-events:
		case turnErr := <-done:
			t.Fatalf("turn returned before naming: %v", turnErr)
		case <-ctx.Done():
			t.Fatal("timed out waiting for title")
		}
	}
	header, err := store.LoadHeader(ctx, sess.ID)
	if err != nil || header.Preview != "为什么健康检查失败？" || header.Name != nameEvent.Name || header.NameSeq != nameEvent.Seq || nameEvent.Seq <= 0 {
		t.Fatalf("header=%+v event=%+v err=%v", header, nameEvent, err)
	}
	seq, err := ex.Rename(ctx, sess.ID, "手动名称")
	if err != nil || seq <= nameEvent.Seq {
		t.Fatalf("rename seq=%d, err=%v", seq, err)
	}
	close(finish)
	if err := <-done; err == nil {
		t.Fatal("expected the main turn to fail")
	}
	header, err = store.LoadHeader(ctx, sess.ID)
	if err != nil || header.Name != "手动名称" || header.NameSeq != seq {
		t.Fatalf("name was lost during the terminal save: %+v, err=%v", header, err)
	}
	if _, changed, err := store.UpdateConversationName(ctx, sess.ID, "迟到的标题", true, session.EventRecord{}); err != nil || changed {
		t.Fatalf("late automatic write changed=%t err=%v", changed, err)
	}
}
