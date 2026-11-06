package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/collab"
	"code-agent/internal/conversation"
	"code-agent/internal/jobs"
	"code-agent/internal/session"
	"code-agent/internal/session/sqlite"
	"code-agent/internal/settings"
	"code-agent/internal/tools"
)

func completionFixture(t *testing.T) (*sqlite.Store, *session.Session) {
	t.Helper()
	s, err := sqlite.New(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	sess := &session.Session{ID: testOwner.SessionID, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := s.Save(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	return s, sess
}

func TestJobCompletionRealProcessDurableAndDefaultOff(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[enabled], func(t *testing.T) {
			store, sess := completionFixture(t)
			sink := NewJobEventSink(context.Background(), store)
			sink.OnCompletionError = func(err error) { t.Error(err) }
			reg := jobs.NewRegistry()
			reg.Sink = sink
			owner := testOwner
			owner.Notify = enabled
			job := reg.Start(".", "echo completion-marker", []string{"echo", "completion-marker"}, owner)
			job.Wait()
			for _, partition := range []string{job.Snapshot().ID, owner.SessionID} {
				events, err := store.SessionEvents(context.Background(), partition)
				if err != nil || events[len(events)-1].Kind != "job_finished" {
					t.Fatalf("terminal event: %v %v", events, err)
				}
			}
			ids, err := store.CheckpointRuntimeInputs(context.Background(), sess, session.RuntimeInputBoundary{TurnID: "consumer", Deliver: true})
			if err != nil {
				t.Fatal(err)
			}
			if enabled {
				if len(ids) != 1 || !strings.Contains(sess.Messages[0].Content, "completion-marker") {
					t.Fatal("lost process result", ids, sess.Messages)
				}
			} else if len(ids) != 0 {
				t.Fatal("feature off produced model input")
			}
		})
	}
}

type retryCompletionStore struct {
	*sqlite.Store
	failures, calls int
}

func (s *retryCompletionStore) RecordRuntimeInput(ctx context.Context, in session.RuntimeInput, ev []session.EventRecord) ([]int64, bool, error) {
	s.calls++
	if s.calls <= s.failures {
		return nil, false, errors.New("fixture persistence failure")
	}
	return s.Store.RecordRuntimeInput(ctx, in, ev)
}

func TestJobCompletionRetryDedupAndFailureReporting(t *testing.T) {
	for _, failures := range []int{1, 3} {
		t.Run(map[int]string{1: "retry", 3: "exhausted"}[failures], func(t *testing.T) {
			store, sess := completionFixture(t)
			wrapped := &retryCompletionStore{Store: store, failures: failures}
			sink := NewJobEventSink(context.Background(), wrapped)
			var reported []error
			sink.OnCompletionError = func(err error) { reported = append(reported, err) }
			live := &recordingEmitter{}
			sink.SetLiveResolver(func(string) agent.Emitter { return live })
			owner := testOwner
			owner.Notify = true
			snap := jobs.Snapshot{ID: "job_test", Status: jobs.Failed, ExitCode: 2, Owner: owner, OutputTail: strings.Repeat("\x00", 2000)}
			sink.JobFinished(snap)
			if failures == 3 {
				if len(reported) != 1 || wrapped.calls != 3 || len(live.snapshot()) != 0 {
					t.Fatalf("false success: %v calls=%d live=%v", reported, wrapped.calls, live.snapshot())
				}
				ids, err := store.CheckpointRuntimeInputs(context.Background(), sess, session.RuntimeInputBoundary{TurnID: "consumer", Deliver: true})
				if err != nil || len(ids) != 0 {
					t.Fatal("failed write delivered", ids, err)
				}
				for _, partition := range []string{snap.ID, owner.SessionID} {
					events, err := store.SessionEvents(context.Background(), partition)
					if err != nil || len(events) != 0 {
						t.Fatal("failed terminal half-commit", events, err)
					}
				}
				// Registry retained the terminal value; retry storage, never the command.
				sink.JobFinished(snap)
			} else if len(reported) != 0 || wrapped.calls != 2 {
				t.Fatal(reported, wrapped.calls)
			}
			sink.JobFinished(snap)
			if len(live.snapshot()) != 2 {
				t.Fatal("duplicate live terminal", live.snapshot())
			}
			for _, partition := range []string{snap.ID, owner.SessionID} {
				events, err := store.SessionEvents(context.Background(), partition)
				if err != nil || len(events) != 1 || events[0].Kind != "job_finished" {
					t.Fatal(events, err)
				}
			}
			ids, err := store.CheckpointRuntimeInputs(context.Background(), sess, session.RuntimeInputBoundary{TurnID: "consumer", Deliver: true})
			if err != nil || len(ids) != 1 {
				t.Fatal("bounded binary tail failed delivery", ids, err)
			}
		})
	}
}

func TestServeRunBuilderRuntimeInputGate(t *testing.T) {
	SetStoreBaseDir(t.TempDir())
	store, sess := completionFixture(t)
	sess.WorkspacePath = t.TempDir()
	for _, value := range []*bool{nil, bptr(false), bptr(true)} {
		cfg := settings.Settings{Agent: settings.AgentConfig{BackgroundJobNotifications: value}}
		builder := NewServeRunBuilder(cfg, settings.ModelConfig{}, nil, nil, tools.NewRegistry(), NewWorkspaceRegistry(""), nil)
		runner := builder.Build(conversation.RuntimeContext{Session: sess, RuntimeInputs: store}).(*agent.Runner)
		want := value == nil || *value
		if (runner.RuntimeInputs != nil) != want {
			t.Fatalf("notification gate: %v -> %v", value, runner.RuntimeInputs)
		}
		if builder.BackgroundContinuationConfigured(sess.WorkspacePath) != want {
			t.Fatal("capability and runner configuration disagree")
		}
	}
	cfg := settings.Settings{Agent: settings.AgentConfig{BackgroundJobNotifications: bptr(true)}}
	builder := NewServeRunBuilder(cfg, settings.ModelConfig{}, nil, nil, tools.NewRegistry(), NewWorkspaceRegistry(""), nil)
	runner := builder.Build(conversation.RuntimeContext{Session: sess, RuntimeInputs: store, ManagedTask: &collab.ManagedTurnRef{}}).(*agent.Runner)
	if runner.RuntimeInputs != nil {
		t.Fatal("managed execution gained notifications")
	}
	runner = builder.Build(conversation.RuntimeContext{Session: sess}).(*agent.Runner)
	if runner.RuntimeInputs != nil {
		t.Fatal("host without durable port gained notifications")
	}
	if err := os.MkdirAll(filepath.Join(sess.WorkspacePath, ".codeagent"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings.ProjectLocalPath(sess.WorkspacePath), []byte(`{"agent":{"background_job_notifications":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	runner = builder.Build(conversation.RuntimeContext{Session: sess, RuntimeInputs: store}).(*agent.Runner)
	if runner.RuntimeInputs != nil {
		t.Fatal("project explicit false ignored")
	}
	if builder.BackgroundContinuationConfigured(sess.WorkspacePath) {
		t.Fatal("project explicit false still advertises capability")
	}
}
