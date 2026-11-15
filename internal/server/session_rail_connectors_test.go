package server

import (
	"context"
	"encoding/json"
	"testing"

	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/mcp"
	runtimepkg "code-agent/internal/runtime"
)

type railConnectorSource struct{}

func (railConnectorSource) Prompts() []mcp.PromptSpec { return nil }
func (railConnectorSource) RenderPrompt(context.Context, string, []string) (string, error) {
	return "", nil
}
func (railConnectorSource) ConnectorSnapshot(root string) runtimepkg.ConnectorSnapshot {
	return runtimepkg.ConnectorSnapshot{State: "ready", Items: []runtimepkg.Connector{{ID: root, Kind: "mcp", Name: "scoped", Source: "project", Status: "unknown"}}}
}

func TestRailConnectorRoutesSessionScopeAndClientRegistration(t *testing.T) {
	repo := newFakeConversationRepo()
	a, _ := repo.Create(context.Background(), t.TempDir(), "")
	b, _ := repo.Create(context.Background(), t.TempDir(), "")
	active := conversation.NewActiveTurnRegistry()
	executor := conversation.NewTurnExecutor(repo, &fakeEventStore{}, active, conversation.NewSubscriptionManager(), nil)
	t.Cleanup(executor.Shutdown)
	executor.RegisterTools(a.ID, []agent.ClientToolDef{{Name: "capture_screen", Description: "Actual registered client tool"}})
	h := NewMux(repo, &fakeEventStore{}, executor, MuxOptions{Prompts: railConnectorSource{}})
	path := "/v1/conversations/" + a.ID + "/connectors"
	w := jobRequest(h, "GET", path+"?workspace_path=/another", "")
	var envelope struct {
		Data runtimepkg.ConnectorSnapshot `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(envelope.Data.Items) != 2 || envelope.Data.Items[0].ID != a.WorkspacePath {
		t.Fatal(w.Code, w.Body.String())
	}
	client := envelope.Data.Items[1]
	if client.Kind != "client_capability" || client.Status != "unknown" || client.AvailableToolCount != nil || client.ExposedToolCount != nil || *client.RegisteredToolCount != 1 || client.Tools[0].CatalogVisible != nil {
		t.Fatalf("registration became permission: %+v", client)
	}
	w = jobRequest(h, "GET", "/v1/conversations/"+b.ID+"/connectors", "")
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data.Items) != 1 {
		t.Fatal("client tools leaked across sessions")
	}
	if w = jobRequest(h, "GET", path+"/client:capture_screen", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = jobRequest(h, "GET", "/v1/conversations/"+b.ID+"/connectors/client:capture_screen", ""); w.Code != 404 {
		t.Fatal("foreign client tool exposed", w.Code)
	}
}
