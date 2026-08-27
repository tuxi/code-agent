package runtime

import (
	"context"
	"testing"
	"time"

	"code-agent/internal/sessionfork"
	"code-agent/internal/tools"
)

// fakeHeadlessControl satisfies tools.SessionControl so HeadlessRuntime can
// project session tools in tests without a real control plane.
type fakeHeadlessControl struct{}

func (fakeHeadlessControl) Send(context.Context, tools.SessionSendRequest) (tools.SessionDelivery, error) {
	return tools.SessionDelivery{}, nil
}
func (fakeHeadlessControl) WaitAny(context.Context, []tools.SessionWaitTarget, time.Duration) (tools.SessionWaitResult, error) {
	return tools.SessionWaitResult{}, nil
}
func (fakeHeadlessControl) PollTurn(context.Context, string, string, int64) (*tools.SessionWaitCompletion, int64, error) {
	return nil, 0, nil
}
func (fakeHeadlessControl) CreateSession(context.Context, tools.SessionCreateRequest) (tools.SessionSpawnResult, error) {
	return tools.SessionSpawnResult{}, nil
}
func (fakeHeadlessControl) ForkSession(context.Context, sessionfork.Request) (sessionfork.Result, error) {
	return sessionfork.Result{}, nil
}

// TestEnsureToolsRegisteredProjectsSessionTools verifies the headless tool
// projection is idempotently registered on the workspace runtime, so a
// rebuilt runtime (daemon restart) has send_to_session etc. available for
// resume/retry — the "tool not found: send_to_session" regression.
func TestEnsureToolsRegisteredProjectsSessionTools(t *testing.T) {
	root := t.TempDir()
	seedFluxRun(t, root) // create the flux-workflows.db

	rt := NewHeadlessRuntime(fakeHeadlessControl{})
	if err := rt.EnsureToolsRegistered(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	// Second call must be a no-op (idempotent re-projection).
	if err := rt.EnsureToolsRegistered(context.Background(), root); err != nil {
		t.Fatal(err)
	}

	// The runtime's tool registry must now resolve send_to_session.
	runtime, err := getOrCreateRuntime(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	proj := runtime.ToolRegistry()
	if _, ok := proj.Get("send_to_session"); !ok {
		t.Fatal("send_to_session not registered on the runtime tool registry")
	}
	if _, ok := proj.Get("read_session"); !ok {
		t.Fatal("read_session not registered on the runtime tool registry")
	}
}
