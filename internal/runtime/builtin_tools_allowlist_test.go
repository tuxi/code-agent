package runtime

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/settings"
	"code-agent/internal/tools"
)

// captureProvider records every model request and answers with plain text.
type captureProvider struct{ requests []model.Request }

func (p *captureProvider) Complete(_ context.Context, req model.Request) (model.Response, error) {
	p.requests = append(p.requests, req)
	return model.Response{Content: "done", FinishReason: "stop"}, nil
}

// requestToolNames flattens the advertised tool names for assertions.
func requestToolNames(req model.Request) []string {
	names := make([]string, len(req.Tools))
	for i, def := range req.Tools {
		names[i] = def.Function.Name
	}
	sort.Strings(names)
	return names
}

// captureEmitter records every agent event for request assertions.
type captureEmitter struct{ events []agent.Event }

func (e *captureEmitter) Emit(ev agent.Event) { e.events = append(e.events, ev) }

func visibleToolNames(reg *tools.Registry) []string {
	names := make([]string, 0, len(reg.Visible()))
	for _, tool := range reg.Visible() {
		names = append(names, tool.Name())
	}
	sort.Strings(names)
	return names
}

// TestBuiltinToolsAllowlistGatesBaseRegistry covers the shared daemon, serve and
// embedded assembly path: a non-nil allowlist must leave ONLY the named tools
// registered, including the plan/clarification tools that were previously wired
// unconditionally.
func TestBuiltinToolsAllowlistGatesBaseRegistry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		allow []string
		want  []string
	}{
		{name: "single tool", allow: []string{"read_file"}, want: []string{"read_file"}},
		{name: "empty denies everything", allow: []string{}, want: []string{}},
		{name: "plan tools must be named explicitly", allow: []string{"enter_plan_mode", "ask_user"}, want: []string{"ask_user", "enter_plan_mode"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allow := tc.allow
			cfg := settings.Settings{Agent: settings.AgentConfig{BuiltinTools: &allow}}
			reg, _, _, _, err := BuildBaseRegistry(context.Background(), cfg, settings.ModelConfig{}, nil, nil, nil, t.TempDir(), nil)
			if err != nil {
				t.Fatalf("BuildBaseRegistry: %v", err)
			}
			if got := visibleToolNames(reg); !equalStrings(got, tc.want) {
				t.Fatalf("visible tools = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDaemonSettingsAllowlistReachesModelRequest is the end-to-end regression for
// the isolated daemon: an allowlist in the project settings file must survive
// the daemon's staged (user + project) load, reach the base registry, and be the
// complete tool set advertised on the model request.
func TestDaemonSettingsAllowlistReachesModelRequest(t *testing.T) {
	SetStoreBaseDir(t.TempDir())
	root := t.TempDir()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".codeagent"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings.ProjectSharedPath(root), []byte(`{"agent":{"builtin_tools":["read_file"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, trusted := LoadSettingsWithTrust(context.Background(), root, home, nil, TrustAlways, nil)
	if !trusted {
		t.Fatal("project settings were not trusted")
	}
	if cfg.Agent.BuiltinTools == nil {
		t.Fatal("builtin_tools was dropped by the staged settings load")
	}

	provider := &captureProvider{}
	toolReg, _, planRef, _, err := BuildBaseRegistry(context.Background(), cfg, settings.ModelConfig{}, provider, nil, nil, root, nil)
	if err != nil {
		t.Fatalf("BuildBaseRegistry: %v", err)
	}
	wsReg := NewWorkspaceRegistry("")
	t.Cleanup(func() { _ = wsReg.Close() })
	builder := NewServeRunBuilder(cfg, settings.ModelConfig{}, provider, nil, toolReg, wsReg, planRef)
	runner := builder.Build(conversation.RuntimeContext{
		Session: &session.Session{ID: "allowlist-session", WorkspacePath: root, Metadata: map[string]any{}},
	})
	typed, ok := runner.(*agent.Runner)
	if !ok {
		t.Fatalf("builder returned %T, want *agent.Runner", runner)
	}

	var advertised []string
	emitter := &captureEmitter{}
	typed.Emitter = emitter
	sess := &session.Session{ID: "allowlist-session", WorkspacePath: root, Metadata: map[string]any{}}
	if _, err := typed.RunTurn(context.Background(), sess, "hi"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	for _, e := range emitter.events {
		if e.Kind == agent.EventModelRequest {
			advertised = append([]string(nil), e.ToolNames...)
		}
	}
	if len(provider.requests) != 1 {
		t.Fatalf("model requests = %d, want 1", len(provider.requests))
	}
	got := requestToolNames(provider.requests[0])
	if !equalStrings(got, []string{"read_file"}) {
		t.Fatalf("model request tools = %v, want [read_file]", got)
	}
	if !equalStrings(advertised, []string{"read_file"}) {
		t.Fatalf("model_request event tool_names = %v, want [read_file]", advertised)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
