package conversation

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"code-agent/internal/agent"
	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/session/sqlite"
	"code-agent/internal/settings"
)

type contractTestBuilder struct {
	runs     atomic.Int32
	resolved string
}

func (b *contractTestBuilder) ResolveModel(string) (*settings.ModelConfig, error) {
	return &settings.ModelConfig{Name: b.resolved, Model: b.resolved}, nil
}
func (b *contractTestBuilder) Build(ctx RuntimeContext) TurnRunner {
	return &contractTestRunner{builder: b, ctx: ctx}
}

type contractTestRunner struct {
	builder *contractTestBuilder
	ctx     RuntimeContext
}

func (r *contractTestRunner) RunTurn(ctx context.Context, s *session.Session, _ string) (agent.TurnResult, error) {
	return r.RunPreparedTurn(ctx, s)
}
func (r *contractTestRunner) ResumeTurn(ctx context.Context, s *session.Session) (agent.TurnResult, error) {
	return r.RunPreparedTurn(ctx, s)
}
func (r *contractTestRunner) RunPreparedTurn(_ context.Context, s *session.Session) (agent.TurnResult, error) {
	r.builder.runs.Add(1)
	r.ctx.Publisher.Emit(agent.Event{Kind: agent.EventTurnFinished, SessionID: s.ID, TurnID: r.ctx.TurnID})
	return agent.TurnResult{TurnID: r.ctx.TurnID}, nil
}

func TestValidationContractExecutorPersistsAndDeduplicatesAfterRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "contract-executor.db")
	open := func(builder *contractTestBuilder) (*TurnExecutor, ConversationRepository, *sqlite.Store) {
		t.Helper()
		store, err := sqlite.New(path)
		if err != nil {
			t.Fatal(err)
		}
		repo := NewSQLiteRepository(store, 128000, 90000, "local", "", nil)
		ex := NewTurnExecutor(repo, &StoreEventAdapter{Store: store}, NewActiveTurnRegistry(), NewSubscriptionManager(), builder)
		t.Cleanup(func() { ex.Shutdown(); _ = repo.Close() })
		return ex, repo, store
	}
	builder := &contractTestBuilder{resolved: "served-first"}
	ex, repo, store := open(builder)
	if !ex.SupportsValidationContracts() {
		t.Fatal("SQLite contract capability missing")
	}
	sess, err := repo.Create(ctx, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	contract := &session.ValidationContract{Version: 1, ValidationCommands: []string{"go version"}, ManualChecks: []string{"检查横屏"}}
	transport := NewTransportSession(sess.ID, ex)
	first, err := transport.SendMessageWithValidationContract(ctx, "original", "task", "", "", nil, nil, contract)
	if err != nil || first.TurnID == "" {
		t.Fatal(first, err)
	}
	input, err := store.TurnInput(ctx, sess.ID, "original")
	if err != nil || input.ValidationContract == nil || input.ValidationContract.ManualChecks[0] != "检查横屏" || input.ResolvedModel != "served-first" {
		t.Fatal(input, err)
	}
	loaded, err := repo.Load(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range loaded.Messages {
		if strings.Contains(message.Content, "检查横屏") {
			t.Fatal("contract metadata leaked into model prompt")
		}
	}
	ex.Shutdown()
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	restartedBuilder := &contractTestBuilder{resolved: "served-after-restart"}
	restarted, _, store := open(restartedBuilder)
	duplicate, err := restarted.ExecuteWithValidationContract(ctx, sess.ID, "original", "task", "", "", nil, nil, contract)
	if err != nil || !duplicate.Deduplicated || duplicate.TurnID != first.TurnID {
		t.Fatal(duplicate, err)
	}
	changed := *contract
	changed.ManualChecks = []string{"different acceptance requirement"}
	_, err = restarted.ExecuteWithValidationContract(ctx, sess.ID, "original", "task", "", "", nil, nil, &changed)
	var conflict RequestConflictError
	if !errors.As(err, &conflict) {
		t.Fatal("changed contract was not rejected", err)
	}
	_, err = restarted.ExecuteWithRequestID(ctx, sess.ID, "original", "task", "", "")
	if !errors.As(err, &conflict) {
		t.Fatal("contract removal was not rejected", err)
	}
	if builder.runs.Load() != 1 || restartedBuilder.runs.Load() != 0 {
		t.Fatal("duplicate/conflict re-executed work")
	}
	rows, err := store.SessionEvents(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	accepts := 0
	for _, row := range rows {
		if row.Kind == "turn_accepted" {
			accepts++
			c, err := session.AcceptedValidation(row, input)
			if err != nil || c == nil {
				t.Fatal(c, err)
			}
		}
	}
	if accepts != 1 {
		t.Fatal("acceptance duplicated", accepts)
	}
}

func TestValidationContractConcurrentExecutorsRejectChangedContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent-executors.db")
	a, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx := context.Background()
	builders := []*contractTestBuilder{{resolved: "local"}, {resolved: "local"}}
	repos := []ConversationRepository{NewSQLiteRepository(a, 128000, 90000, "local", "", nil), NewSQLiteRepository(b, 128000, 90000, "local", "", nil)}
	sess, err := repos[0].Create(ctx, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i, store := range []*sqlite.Store{a, b} {
		ex := NewTurnExecutor(repos[i], &StoreEventAdapter{Store: store}, NewActiveTurnRegistry(), NewSubscriptionManager(), builders[i])
		defer ex.Shutdown()
		contract := &session.ValidationContract{Version: 1, ValidationCommands: []string{"first"}}
		if i == 1 {
			contract.ValidationCommands = []string{"different"}
		}
		wg.Add(1)
		go func(ex *TurnExecutor, c *session.ValidationContract) {
			defer wg.Done()
			<-start
			_, err := ex.ExecuteWithValidationContract(ctx, sess.ID, "same-request", "task", "", "", nil, nil, c)
			results <- err
		}(ex, contract)
	}
	close(start)
	wg.Wait()
	close(results)
	succeeded, conflicted := 0, 0
	for err := range results {
		var conflict RequestConflictError
		if err == nil {
			succeeded++
		} else if errors.As(err, &conflict) {
			conflicted++
		} else {
			t.Fatal(err)
		}
	}
	if succeeded != 1 || conflicted != 1 || builders[0].runs.Load()+builders[1].runs.Load() != 1 {
		t.Fatal("concurrent different contract executed more than once", succeeded, conflicted)
	}
}

func TestValidationContractEntryRejectsBeforeExecution(t *testing.T) {
	valid := &session.ValidationContract{Version: 1, ValidationCommands: []string{"go version"}}
	ex := &TurnExecutor{}
	for _, tc := range []struct {
		request string
		c       *session.ValidationContract
	}{{"", valid}, {"bad request", valid}, {"r", nil}, {"r", &session.ValidationContract{Version: 2}}} {
		res, err := ex.ExecuteWithValidationContract(context.Background(), "unloaded", tc.request, "task", "", "", nil, nil, tc.c)
		var coded ValidationContractError
		if !errors.As(err, &coded) || coded.Unsupported || res.TurnID != "" {
			t.Fatal("invalid input reached execution", res, err)
		}
	}
	_, err := ex.ExecuteWithValidationContract(context.Background(), "unloaded", "r", "task", "", "", nil, nil, valid)
	var coded ValidationContractError
	if !errors.As(err, &coded) || !coded.Unsupported {
		t.Fatal("missing Store capability ignored", err)
	}
	repo := NewSQLiteRepository(session.NewMemoryStore(), 128000, 90000, "local", "", nil)
	ex.repo = repo
	if ex.SupportsValidationContracts() {
		t.Fatal("wrapper advertised unsupported underlying store")
	}
	if got, err := validationPayloadHash("original-legacy-hash", nil); err != nil || got != "original-legacy-hash" {
		t.Fatal("legacy identity changed", got, err)
	}
}

func TestValidationContractDoesNotChangeLegacyAcceptance(t *testing.T) {
	store, err := sqlite.New(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repo := NewSQLiteRepository(store, 128000, 90000, "local", "", nil)
	ex := NewTurnExecutor(repo, &StoreEventAdapter{Store: store}, NewActiveTurnRegistry(), NewSubscriptionManager(), &contractTestBuilder{resolved: "local"})
	defer ex.Shutdown()
	sess, err := repo.Create(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.ExecuteWithRequestID(context.Background(), sess.ID, "legacy", "legacy task", "", ""); err != nil {
		t.Fatal(err)
	}
	input, err := store.TurnInput(context.Background(), sess.ID, "legacy")
	if err != nil || input.ValidationContract != nil || input.PayloadHash != turnInputPayloadHash("legacy task", "", []model.GatewayAssetRef{}) {
		t.Fatal(input, err)
	}
	rows, err := store.SessionEvents(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if strings.Contains(string(row.Payload), "validation_contract") {
			t.Fatal("legacy event was upgraded with invented metadata")
		}
	}
}
