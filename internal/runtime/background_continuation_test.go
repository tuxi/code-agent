package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/jobs"
	"code-agent/internal/model"
	"code-agent/internal/session/sqlite"
	"code-agent/internal/settings"
	"code-agent/internal/tools"
	"code-agent/internal/tools/shell"
)

type backgroundJobProvider struct {
	mu    sync.Mutex
	calls int
	jobID string
}

func (p *backgroundJobProvider) Complete(_ context.Context, req model.Request) (model.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	name, args := "run_command", `{"command":"sleep 30","background":true}`
	if p.calls == 2 {
		for _, m := range req.Messages {
			if m.Role == model.RoleTool {
				var result struct {
					JobID string `json:"job_id"`
				}
				body := m.Content
				if start := strings.IndexByte(body, '{'); start >= 0 {
					body = body[start:]
				}
				if json.Unmarshal([]byte(body), &result) == nil && result.JobID != "" {
					p.jobID = result.JobID
				}
			}
		}
		name = "job_wait"
		data, _ := json.Marshal(map[string]any{"job_id": p.jobID, "yield": true})
		args = string(data)
	}
	if p.calls > 2 {
		return model.Response{Content: "reviewed terminal status", FinishReason: "stop"}, nil
	}
	return model.Response{ToolCalls: []model.ToolCall{{ID: name, Type: "function", Function: model.FunctionCall{Name: name, Arguments: args}}}, FinishReason: "tool_calls"}, nil
}

func TestBackgroundContinuationRealJobThroughServe(t *testing.T) {
	SetStoreBaseDir(t.TempDir())
	ctx := context.Background()
	store, err := sqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sink := NewJobEventSink(ctx, store)
	registry := jobs.NewRegistry()
	registry.Sink = sink
	defer func() {
		for _, snap := range registry.List() {
			_ = registry.Cancel(snap.ID)
			if job, ok := registry.Get(snap.ID); ok {
				job.Wait()
			}
		}
	}()
	command := shell.NewRunCommandTool()
	command.Jobs = registry
	reg := tools.NewRegistry()
	if err = reg.Register(command); err != nil {
		t.Fatal(err)
	}
	if err = reg.Register(&shell.JobWaitTool{Jobs: registry}); err != nil {
		t.Fatal(err)
	}
	provider := &backgroundJobProvider{}
	cfg := settings.Settings{Agent: settings.AgentConfig{BackgroundJobNotifications: bptr(true), MaxSteps: 5}}
	builder := NewServeRunBuilder(cfg, settings.ModelConfig{Model: "wire"}, provider, nil, reg, NewWorkspaceRegistry(""), nil)
	repo := conversation.NewSQLiteRepository(store, 128000, 90000, "wire", "", nil)
	sess, err := repo.Create(ctx, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	subs := conversation.NewSubscriptionManager()
	e := conversation.NewTurnExecutor(repo, &conversation.StoreEventAdapter{Store: store}, conversation.NewActiveTurnRegistry(), subs, builder)
	defer e.Shutdown()
	e.SetApprover(sess.ID, runtimeTestApprover{})
	e.SetBackgroundContinuationSupport(sess.ID, true)
	sink.OnCompletion = e.NotifyBackgroundCompletion
	sink.SetLiveResolver(subs.Emitter)
	res, err := e.ExecuteWithRequestID(ctx, sess.ID, "request", "start", "", "")
	if err != nil || !res.WaitingBackground {
		t.Fatal(res, err)
	}
	provider.mu.Lock()
	id := provider.jobID
	provider.mu.Unlock()
	job, ok := registry.Get(id)
	if !ok {
		t.Fatal("background job missing", id)
	}
	defer func() { _ = registry.Cancel(id); job.Wait() }()
	if !job.Snapshot().Owner.Notify {
		t.Fatal("production runner did not opt job into completion delivery")
	}
	if err = registry.Cancel(id); err != nil {
		t.Fatal(err)
	}
	job.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, readErr := store.Continuation(ctx, sess.ID)
		if readErr == nil && c.State == "finished" {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	c, err := store.Continuation(ctx, sess.ID)
	if err != nil || c.State != "finished" {
		t.Fatal("job completion failed to resume", c, err)
	}
	provider.mu.Lock()
	calls := provider.calls
	provider.mu.Unlock()
	if calls != 3 {
		t.Fatal("unexpected model calls", calls)
	}
	// The per-turn binding must not mutate the process-wide legacy schema.
	tool, _ := reg.Get("job_wait")
	if tool.(*shell.JobWaitTool).AllowYield {
		t.Fatal("yield leaked to legacy registry")
	}
	records, err := store.SessionEvents(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	starts, finishes, waits, resumes := 0, 0, 0, 0
	for _, record := range records {
		if record.TurnID != res.TurnID {
			t.Fatal("turn identity changed", record)
		}
		if record.Kind == string(agent.EventTurnStarted) {
			starts++
		}
		if record.Kind == string(agent.EventTurnFinished) {
			finishes++
		}
		if record.Kind == string(agent.EventTurnWaitingBackground) || record.Kind == string(agent.EventTurnBackgroundResumed) {
			var ev agent.Event
			if err := json.Unmarshal(record.Payload, &ev); err != nil {
				t.Fatal(err)
			}
			if ev.Background == nil || ev.Background.Revision != c.Revision || ev.Background.TurnID != res.TurnID || len(ev.Background.JobIDs) != 1 || ev.Background.JobIDs[0] != id {
				t.Fatalf("missing durable client state: %+v", ev.Background)
			}
			if record.Kind == string(agent.EventTurnWaitingBackground) {
				waits++
			} else {
				resumes++
			}
		}
	}
	if starts != 1 || finishes != 1 || waits != 1 || resumes != 1 {
		t.Fatal("unexpected lifecycle", starts, finishes, waits, resumes)
	}
}
