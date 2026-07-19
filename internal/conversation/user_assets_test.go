package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/credential"
	"code-agent/internal/model"
	"code-agent/internal/session"
)

type durableInputBuilder struct {
	resolved atomic.Value
	builds   atomic.Int32
}

func newDurableInputBuilder(model string) *durableInputBuilder {
	b := &durableInputBuilder{}
	b.resolved.Store(model)
	return b
}
func (b *durableInputBuilder) ResolveModel(string) (string, error) {
	return b.resolved.Load().(string), nil
}
func (b *durableInputBuilder) Build(ctx RuntimeContext) TurnRunner {
	b.builds.Add(1)
	return durableInputRunner{ctx: ctx}
}

type durableInputRunner struct{ ctx RuntimeContext }

type releaseServiceStub struct {
	err   error
	calls atomic.Int32
}

func (s *releaseServiceStub) CredentialScope(context.Context, credential.Resolver) string {
	return "owner-scope"
}
func (s *releaseServiceStub) ReleaseConversationAssetRefs(context.Context, credential.Resolver, string) error {
	s.calls.Add(1)
	return s.err
}

func (r durableInputRunner) RunTurn(context.Context, *session.Session, string) (agent.TurnResult, error) {
	return agent.TurnResult{}, errors.New("unexpected legacy turn")
}
func (r durableInputRunner) ResumeTurn(context.Context, *session.Session) (agent.TurnResult, error) {
	return agent.TurnResult{TurnID: r.ctx.TurnID}, nil
}
func (r durableInputRunner) RunPreparedTurn(_ context.Context, sess *session.Session) (agent.TurnResult, error) {
	sess.Messages = append(sess.Messages, model.Message{Role: model.RoleAssistant, Content: "done"})
	r.ctx.Publisher.Emit(agent.Event{Kind: agent.EventTurnFinished, SessionID: sess.ID, TurnID: r.ctx.TurnID, Text: "done"})
	return agent.TurnResult{TurnID: r.ctx.TurnID, Final: "done"}, nil
}

func newDurableExecutor(t *testing.T) (*TurnExecutor, session.TurnInputStore, ConversationRepository, *durableInputBuilder) {
	t.Helper()
	store := session.NewMemoryStore()
	sess := &session.Session{ID: "s", Messages: []model.Message{{Role: model.RoleSystem, Content: "sys"}}, Metadata: map[string]any{}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := store.Save(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	repo := NewSQLiteRepository(store, 128000, 96000, "default", "", nil)
	builder := newDurableInputBuilder("resolved-A")
	executor := NewTurnExecutor(repo, &StoreEventAdapter{Store: store}, NewActiveTurnRegistry(), NewSubscriptionManager(), builder)
	return executor, store, repo, builder
}

func TestDurableRequestHashUsesWireModelAndDeduplicatesUserMessage(t *testing.T) {
	executor, store, repo, builder := newDurableExecutor(t)
	asset := model.GatewayAssetRef{AssetID: 7, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Kind: "image", MIMEType: "image/png", Filename: "a.png"}
	first, err := executor.ExecuteWithRequestIDAndAssets(context.Background(), "s", "req", "look", "", []model.GatewayAssetRef{asset})
	if err != nil {
		t.Fatal(err)
	}
	builder.resolved.Store("resolved-B")
	live, unsubscribe := executor.subs.Subscribe("s")
	defer unsubscribe()
	duplicate, err := executor.ExecuteWithRequestIDAndAssets(context.Background(), "s", "req", "look", "", []model.GatewayAssetRef{asset})
	if err != nil || !duplicate.Deduplicated || duplicate.TurnID != first.TurnID {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	select {
	case event := <-live:
		if event.Kind != agent.EventTurnAccepted || event.TurnID != first.TurnID || event.RequestID != "req" {
			t.Fatalf("duplicate acknowledgement=%+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("duplicate retry did not re-acknowledge turn_accepted")
	}
	_, err = executor.ExecuteWithRequestIDAndAssets(context.Background(), "s", "req", "changed", "", []model.GatewayAssetRef{asset})
	var conflict RequestConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("conflict err=%v", err)
	}
	input, err := store.TurnInput(context.Background(), "s", "req")
	if err != nil {
		t.Fatal(err)
	}
	if input.WireModel != "" || input.ResolvedModel != "resolved-A" {
		t.Fatalf("models wire=%q resolved=%q", input.WireModel, input.ResolvedModel)
	}
	loaded, err := repo.Load(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	users := 0
	for _, message := range loaded.Messages {
		if message.Role == model.RoleUser {
			users++
			if message.OriginTurnID != first.TurnID {
				t.Fatalf("origin=%q", message.OriginTurnID)
			}
		}
	}
	if users != 1 || builder.builds.Load() != 1 {
		t.Fatalf("users=%d builds=%d", users, builder.builds.Load())
	}
}

func TestRecoverAcceptedTurnKeepsIdentityAndAppendsOnce(t *testing.T) {
	executor, store, repo, builder := newDurableExecutor(t)
	input := session.TurnInput{SessionID: "s", RequestID: "recover", TurnID: "turn_stable", PayloadHash: turnInputPayloadHash("look", "", nil), Text: "look", ResolvedModel: "resolved-A"}
	event := agent.Event{Kind: agent.EventTurnAccepted, SessionID: "s", TurnID: input.TurnID, RequestID: input.RequestID, At: time.Now().UTC()}
	payload, _ := json.Marshal(event)
	if _, created, _, err := store.ReserveTurnInput(context.Background(), input, session.EventRecord{SessionID: "s", TurnID: input.TurnID, Kind: string(event.Kind), At: event.At, Payload: payload}); err != nil || !created {
		t.Fatalf("reserve created=%v err=%v", created, err)
	}
	count, err := executor.RecoverTurnInputs(context.Background())
	if err != nil || count != 1 {
		t.Fatalf("recover count=%d err=%v", count, err)
	}
	deadline := time.Now().Add(time.Second)
	for builder.builds.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	loaded, err := repo.Load(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	users := 0
	for _, message := range loaded.Messages {
		if message.Role == model.RoleUser {
			users++
			if message.OriginTurnID != "turn_stable" {
				t.Fatalf("origin=%q", message.OriginTurnID)
			}
		}
	}
	if users != 1 {
		t.Fatalf("users=%d messages=%+v", users, loaded.Messages)
	}
	if again, err := executor.RecoverTurnInputs(context.Background()); err != nil || again != 0 {
		t.Fatalf("second recover=%d err=%v", again, err)
	}
}

func TestTurnInputPayloadHashNormalizesEmptyAssetsAndPreservesOrder(t *testing.T) {
	if turnInputPayloadHash("look", "", nil) != turnInputPayloadHash("look", "", []model.GatewayAssetRef{}) {
		t.Fatal("omitted and empty assets must have one normalized payload identity")
	}
	a := model.GatewayAssetRef{AssetID: 1, Kind: "image", MIMEType: "image/png", Filename: "a.png"}
	b := model.GatewayAssetRef{AssetID: 2, Kind: "image", MIMEType: "image/jpeg", Filename: "b.jpg"}
	if turnInputPayloadHash("look", "", []model.GatewayAssetRef{a, b}) == turnInputPayloadHash("look", "", []model.GatewayAssetRef{b, a}) {
		t.Fatal("asset order must participate in payload identity")
	}
}

func TestAssetRefReleaseAuthFailureWaitsForCredentialRecovery(t *testing.T) {
	executor, store, _, _ := newDurableExecutor(t)
	outbox := store.(session.AssetRefReleaseStore)
	if err := outbox.EnqueueAssetRefRelease(context.Background(), session.AssetRefRelease{SessionID: "deleted", CredentialScope: "owner-scope"}); err != nil {
		t.Fatal(err)
	}
	service := &releaseServiceStub{err: &model.APIError{StatusCode: 401}}
	executor.SetAssetRefReleaseService(service)
	executor.FlushAssetRefReleases(context.Background(), nil)
	pending, err := outbox.PendingAssetRefReleases(context.Background(), "owner-scope", time.Now().Add(time.Second))
	if err != nil || len(pending) != 1 || pending[0].Attempts != 0 {
		t.Fatalf("paused release=%+v err=%v", pending, err)
	}
	service.err = nil
	executor.FlushAssetRefReleases(context.Background(), nil)
	pending, err = outbox.PendingAssetRefReleases(context.Background(), "owner-scope", time.Now().Add(time.Second))
	if err != nil || len(pending) != 0 || service.calls.Load() != 2 {
		t.Fatalf("recovered release=%+v calls=%d err=%v", pending, service.calls.Load(), err)
	}
}

func TestRecoverRunningTurnRepairsMissingTurnStartedOnce(t *testing.T) {
	executor, store, repo, _ := newDurableExecutor(t)
	asset := model.GatewayAssetRef{AssetID: 9, Kind: "image", MIMEType: "image/png", Filename: "crash.png"}
	input := session.TurnInput{SessionID: "s", RequestID: "crash", TurnID: "turn_crash", PayloadHash: turnInputPayloadHash("inspect", "", []model.GatewayAssetRef{asset}), Text: "inspect", ResolvedModel: "resolved-A", Assets: []model.GatewayAssetRef{asset}}
	accepted := agent.Event{Kind: agent.EventTurnAccepted, SessionID: "s", TurnID: input.TurnID, RequestID: input.RequestID, At: time.Now().UTC()}
	payload, _ := json.Marshal(accepted)
	if _, _, _, err := store.ReserveTurnInput(context.Background(), input, session.EventRecord{SessionID: "s", TurnID: input.TurnID, Kind: string(accepted.Kind), At: accepted.At, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	sess, err := repo.Load(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	sess.Messages = append(sess.Messages, model.Message{Role: model.RoleUser, Content: input.Text, Assets: input.Assets, OriginTurnID: input.TurnID})
	if err := store.StartTurnInput(context.Background(), input, sess); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if count, err := executor.RecoverTurnInputs(context.Background()); err != nil || count != 0 {
			t.Fatalf("recover %d count=%d err=%v", i, count, err)
		}
	}
	eventStore := store.(session.EventStore)
	records, err := eventStore.SessionEvents(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	started := 0
	for _, record := range records {
		if record.TurnID == input.TurnID && record.Kind == string(agent.EventTurnStarted) {
			started++
			var event agent.Event
			if err := json.Unmarshal(record.Payload, &event); err != nil || len(event.UserAssets) != 1 || event.UserAssets[0].AssetID != asset.AssetID {
				t.Fatalf("repaired event=%+v err=%v", event, err)
			}
		}
	}
	if started != 1 {
		t.Fatalf("turn_started count=%d records=%+v", started, records)
	}
}
