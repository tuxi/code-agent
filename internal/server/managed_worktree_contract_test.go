package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestManagedWorktreeProtocolFixtures(t *testing.T) {
	requestJSON, err := os.ReadFile("testdata/managed_worktree_create_request.json")
	if err != nil {
		t.Fatal(err)
	}
	var request CreateConversationRequest
	if err := json.Unmarshal(requestJSON, &request); err != nil {
		t.Fatal(err)
	}
	if request.ClientRequestID != "create_01J2EXAMPLE" || request.ExecutionPolicy != "isolated_worktree" || request.Worktree == nil || !request.Worktree.Managed || request.Worktree.BaseRef != "head" {
		t.Fatalf("request=%+v", request)
	}

	responseJSON, err := os.ReadFile("testdata/managed_worktree_create_response.json")
	if err != nil {
		t.Fatal(err)
	}
	var response ConversationRef
	if err := json.Unmarshal(responseJSON, &response); err != nil {
		t.Fatal(err)
	}
	if response.Worktree == nil || !response.Worktree.Managed || response.Worktree.State != "ready" || response.WorkspaceID != "checkout_a31f" || response.BaseWorkspaceID != "workspace_agentkit" {
		t.Fatalf("response=%+v", response)
	}
}

func TestManagedWorktreeRequestFailsClosedBeforeProvisionerWiring(t *testing.T) {
	repo := newFakeConversationRepo()
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/conversations", mustOpenFixture(t, "testdata/managed_worktree_create_request.json"))
	NewMux(repo, &fakeEventStore{}, nil, MuxOptions{}).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if len(repo.sessions) != 0 {
		t.Fatalf("managed request created a session before provisioning: %+v", repo.sessions)
	}
}

func mustOpenFixture(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}
