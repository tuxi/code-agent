//go:build unix

package mcpbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func reconnectIdentity(t *testing.T, s *ManualService) ManualReconnectInput {
	t.Helper()
	info, err := s.ConnectionInfo(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return ManualReconnectInput{ServerID: info.ServerID, StoreID: info.StoreID, Workspace: info.Workspace}
}

func TestManualReconnectNativeSameMCPConnection(t *testing.T) {
	s, f, path, grant := manualFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, b := mcp.NewInMemoryTransports()
	server, err := ManualServer(s).Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "reconnect"}, nil).Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	call := func(in manualConnectionInput, want string) ManualConnectionInfo {
		t.Helper()
		out, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "codeagent_connection_info", Arguments: in})
		if err != nil || out == nil {
			t.Fatal(out, err)
		}
		if want != "" {
			if !out.IsError || len(out.Content) != 1 {
				t.Fatal("expected structured tool failure", out)
			}
			var diagnostic struct{ Error BridgeError }
			content, ok := out.Content[0].(*mcp.TextContent)
			if !ok || json.Unmarshal([]byte(content.Text), &diagnostic) != nil || diagnostic.Error.Code != want {
				t.Fatalf("want %s: %+v", want, out)
			}
			return ManualConnectionInfo{}
		}
		if out.IsError {
			t.Fatal(out)
		}
		encoded, _ := json.Marshal(out.StructuredContent)
		var info ManualConnectionInfo
		if err := json.Unmarshal(encoded, &info); err != nil {
			t.Fatal(err)
		}
		return info
	}
	info := call(manualConnectionInput{}, "")
	reconnect := &ManualReconnectInput{ServerID: info.ServerID, StoreID: info.StoreID, Workspace: info.Workspace}
	grant.InstanceID = "renewed-daemon-instance"
	saveGrant(t, path, grant)
	call(manualConnectionInput{}, "grant_replaced")
	call(manualConnectionInput{BindingID: "old", Reconnect: reconnect}, "invalid_tool_arguments")
	renewed := call(manualConnectionInput{Reconnect: reconnect}, "")
	if !renewed.Reconnected || !renewed.IdentityVerified || renewed.ExecutionBound || renewed.BindingsRequireRevalidation || renewed.HeadCommit != info.HeadCommit {
		t.Fatal("unexpected reconnect result", renewed)
	}
	call(manualConnectionInput{}, "")
	call(manualConnectionInput{Reconnect: reconnect}, "") // same grant is idempotent
	list, err := client.ListTools(ctx, nil)
	if err != nil || len(list.Tools) != 13 {
		t.Fatal("reconnect changed protocol/catalog", list, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 0 || f.sessionReads != 0 || len(f.inputs) != 0 {
		t.Fatal("reconnect touched Session execution", f.posts, f.inputs)
	}
}

func TestManualReconnectRejectsInvalidTargetsWithoutRequests(t *testing.T) {
	for _, scenario := range []string{"caller-server", "caller-store", "caller-workspace", "missing-fields", "manifest-server", "manifest-store", "expired", "missing-grant", "workspace-replaced"} {
		t.Run(scenario, func(t *testing.T) {
			s, f, path, grant := manualFixture(t)
			in := reconnectIdentity(t, s)
			want := "identity_mismatch"
			switch scenario {
			case "caller-server":
				in.ServerID = "other"
			case "caller-store":
				in.StoreID = "other"
			case "caller-workspace":
				in.Workspace = filepath.Dir(f.root)
				want = "workspace_mismatch"
			case "missing-fields":
				in.StoreID = ""
				want = "invalid_tool_arguments"
			case "manifest-server":
				grant.ServerID = "other"
			case "manifest-store":
				grant.StoreID = "other"
			case "expired":
				grant.ExpiresAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
				want = "grant_expired"
			case "missing-grant":
				want = "grant_invalid"
			case "workspace-replaced":
				if err := os.Rename(f.root, f.root+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(f.root, 0700); err != nil {
					t.Fatal(err)
				}
				gitRun(t, f.root, "init", "-q")
				want = "workspace_mismatch"
			}
			saveGrant(t, path, grant)
			if scenario == "missing-grant" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			f.mu.Lock()
			before := f.requests
			f.mu.Unlock()
			_, err := s.Reconnect(context.Background(), in)
			requireDiagnostic(t, err, want)
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.requests != before {
				t.Fatal("invalid reconnect sent requests")
			}
		})
	}
}

func TestManualReconnectFailureDoesNotAdoptGrant(t *testing.T) {
	for _, scenario := range []string{"runtime-identity", "grant-rotation", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			s, f, path, grant := manualFixture(t)
			in := reconnectIdentity(t, s)
			original := s.anchor.digest
			grant.InstanceID = "renewed"
			saveGrant(t, path, grant)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var hookErr error
			hookDone := make(chan struct{})
			f.mu.Lock()
			if scenario == "runtime-identity" {
				f.store = "foreign"
			} else {
				f.afterCapabilities = func() {
					defer close(hookDone)
					if scenario == "cancelled" {
						cancel()
					} else {
						grant.InstanceID = "rotated-again"
						data, err := json.Marshal(grant)
						if err == nil {
							err = os.WriteFile(path, data, 0600)
						}
						hookErr = err
					}
				}
			}
			f.mu.Unlock()
			_, err := s.Reconnect(ctx, in)
			if scenario != "runtime-identity" {
				select {
				case <-hookDone:
				case <-time.After(5 * time.Second):
					t.Fatal("identity callback did not complete")
				}
			}
			if err == nil || s.anchor.digest != original {
				t.Fatal("failed reconnect changed anchor", err)
			}
			f.mu.Lock()
			f.store = in.StoreID
			f.mu.Unlock()
			_, err = s.ConnectionInfo(context.Background(), "")
			requireDiagnostic(t, err, "grant_replaced")
			if hookErr != nil {
				t.Fatal(hookErr)
			}
		})
	}
}

func TestManualReconnectRetainsBindingsAndUnknownAdmissions(t *testing.T) {
	s, f, path, grant := manualFixture(t)
	in := reconnectIdentity(t, s)
	ref := manualCreate(t, s, "create")
	f.mu.Lock()
	f.failEvents = true
	f.mu.Unlock()
	_, _, err := s.send(context.Background(), manualSend(ref, "uncertain"))
	requireDiagnostic(t, err, "admission_unknown")
	f.mu.Lock()
	f.failEvents = false
	f.events[ref.ID] = []Event{}
	f.pending = true
	f.mu.Unlock()
	grant.InstanceID = "renewed"
	saveGrant(t, path, grant)
	info, err := s.Reconnect(context.Background(), in)
	if err != nil || !info.BindingsRequireRevalidation || info.ExecutionBound {
		t.Fatal(info, err)
	}
	_, err = s.ConnectionInfo(context.Background(), ref.BindingID)
	requireDiagnostic(t, err, "grant_replaced")
	_, _, err = s.send(context.Background(), manualSend(ref, "new-request"))
	requireDiagnostic(t, err, "grant_replaced")
	rebound, err := s.Bind(context.Background(), ManualBindInput{ManualIdentity: ref.ManualIdentity, SessionID: ref.ID, ReplaceBindingID: ref.BindingID})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.send(context.Background(), manualSend(rebound, "new-request"))
	requireDiagnostic(t, err, "session_busy") // unresolved approval still blocks
	f.mu.Lock()
	f.pending = false
	f.mu.Unlock()
	_, _, err = s.send(context.Background(), manualSend(rebound, "new-request"))
	requireDiagnostic(t, err, "admission_unknown")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 1 || f.inputs[ref.ID] != 1 {
		t.Fatal("reconnection replayed side effects", f.posts, f.inputs)
	}
}

func TestManualReconnectNewEndpointAndLostCreation(t *testing.T) {
	s, f, path, grant := manualFixture(t)
	in := reconnectIdentity(t, s)
	identity := manualIdentity(t, s)
	f.mu.Lock()
	f.loseCreate = true
	f.mu.Unlock()
	_, err := s.Create(context.Background(), ManualCreateInput{ManualIdentity: identity, RequestID: "lost-create"})
	requireDiagnostic(t, err, "create_unknown")
	f.mu.Lock()
	f.loseCreate = false
	f.mu.Unlock()
	// Restart may change both the endpoint and grant instance, while the
	// persistent Runtime/Store identity must remain exactly the original pair.
	restarted := httptest.NewServer(http.HandlerFunc(f.serve))
	defer restarted.Close()
	grant.Endpoint, grant.InstanceID = restarted.URL, "new-process"
	saveGrant(t, path, grant)
	info, err := s.Reconnect(context.Background(), in)
	if err != nil || !info.Reconnected {
		t.Fatal(info, err)
	}
	_, err = s.Create(context.Background(), ManualCreateInput{ManualIdentity: identity, RequestID: "new-create"})
	requireDiagnostic(t, err, "create_unknown")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 1 || len(f.inputs) != 0 {
		t.Fatal("reconnect cleared unknown creation", f.posts, f.inputs)
	}
}
