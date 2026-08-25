package automation

import (
	"context"
	"testing"
	"time"
)

// fakeDispatcher records the automations it was asked to fire.
type fakeDispatcher struct {
	fired []Automation
}

func (f *fakeDispatcher) Dispatch(ctx context.Context, a Automation) (string, error) {
	f.fired = append(f.fired, a)
	return "turn-1", nil
}

// TestSchedulerOnceMarksCompleted verifies a once automation is marked COMPLETED
// after firing (WorkBuddy: "一次性任务（执行一次后自动结束）") and is not re-fired.
func TestSchedulerOnceMarksCompleted(t *testing.T) {
	s := newTestStore(t)
	// once automation scheduled in the past (immediately due)
	once := Automation{
		Name:         "once-task",
		Prompt:       "do it",
		Status:       StatusActive,
		ScheduleType: ScheduleOnce,
		ScheduledAt:  time.Now().Add(-time.Minute),
		Timezone:     "UTC",
		ModeExec:     ModeStandalone,
	}
	created, err := s.Create(context.Background(), once)
	if err != nil {
		t.Fatal(err)
	}
	disp := &fakeDispatcher{}
	sched := NewScheduler(s, disp, time.Hour) // long interval; tick() called manually
	sched.tick(context.Background())

	if len(disp.fired) != 1 {
		t.Fatalf("expected 1 firing, got %d", len(disp.fired))
	}
	got, err := s.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusCompleted {
		t.Fatalf("once automation status = %q, want COMPLETED", got.Status)
	}
	// It must not be due again.
	due, _ := s.NextDueAt(context.Background(), time.Now().Add(time.Hour))
	if len(due) != 0 {
		t.Fatalf("completed once automation should not be due, got %d", len(due))
	}
}

// TestDispatcherPassesPerm verifies the dispatcher builds a Perm from the
// automation and passes it to the submitter.
func TestDispatcherPassesPerm(t *testing.T) {
	var gotPerm Perm
	sub := &recordingSubmitter{onSubmit: func(perm Perm) { gotPerm = perm }}
	creator := &recordingCreator{}
	d := NewTurnDispatcher(sub, creator)

	a := Automation{
		ID:             "auto-1",
		Prompt:         "p",
		ModeExec:       ModeStandalone,
		PermissionMode: "full_access",
		Connectors:     []string{"github"},
		Skills:         []string{"code-review"},
	}
	if _, err := d.Dispatch(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if gotPerm.PermissionMode != "full_access" {
		t.Fatalf("perm.PermissionMode = %q, want full_access", gotPerm.PermissionMode)
	}
	if len(gotPerm.Connectors) != 1 || gotPerm.Connectors[0] != "github" {
		t.Fatalf("perm.Connectors = %v, want [github]", gotPerm.Connectors)
	}
	if len(gotPerm.Skills) != 1 || gotPerm.Skills[0] != "code-review" {
		t.Fatalf("perm.Skills = %v, want [code-review]", gotPerm.Skills)
	}
}

type recordingSubmitter struct {
	onSubmit func(perm Perm)
}

func (r *recordingSubmitter) Submit(ctx context.Context, sessionID, prompt, model string, perm Perm) (string, error) {
	if r.onSubmit != nil {
		r.onSubmit(perm)
	}
	// Mirror the real adapter: return the conversation id, not a turn id.
	return sessionID, nil
}

type recordingCreator struct{}

func (r *recordingCreator) CreateConversation(ctx context.Context, workspacePath string) (string, error) {
	return "sess-1", nil
}

func (r *recordingCreator) DeleteConversation(ctx context.Context, sessionID string) error {
	return nil
}

func (r *recordingCreator) ConversationExists(ctx context.Context, sessionID string) (bool, error) {
	return true, nil
}
