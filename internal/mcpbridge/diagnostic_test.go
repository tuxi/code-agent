package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func requireDiagnostic(t *testing.T, err error, code string) *BridgeError {
	t.Helper()
	var diagnostic *BridgeError
	if !errors.As(err, &diagnostic) || diagnostic.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
	if diagnostic.Retryable || diagnostic.NextAction == "" {
		t.Fatalf("unsafe retry or missing recovery action: %+v", diagnostic)
	}
	return diagnostic
}

func TestBridgeDiagnosticsDoNotReflectCauses(t *testing.T) {
	private := errors.New("PRIVATE token=secret /private/user/file")
	for code := range bridgeDiagnostics {
		t.Run(code, func(t *testing.T) {
			err := bridgeError(code, private)
			if !errors.Is(err, private) {
				t.Fatal("Go cause was lost")
			}
			var out struct{ Error BridgeError }
			if json.Unmarshal([]byte(err.Error()), &out) != nil || out.Error.Code != code || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("unsafe diagnostic: %s", err)
			}
			requireDiagnostic(t, err, code)
		})
	}
	unsafe := admissionError("admission_unknown", "PRIVATE\n", "PRIVATE\n", private)
	if unsafe.SessionID != "" || unsafe.RequestID != "" {
		t.Fatal("invalid references reflected")
	}
}

func TestNativeMCPBridgeErrorUsesStandardToolFailure(t *testing.T) {
	c, _ := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, b := mcp.NewInMemoryTransports()
	server, err := Server(c).Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "diagnostic-test"}, nil).Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "codeagent_send_to_session", Arguments: SendInput{SessionID: "foreign", RequestID: "req", Message: "must not execute"}})
	if err != nil || result == nil || !result.IsError || len(result.Content) != 1 {
		t.Fatalf("business failure should be a tool result, not JSON-RPC failure: %+v %v", result, err)
	}
	content, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatal("missing standard text content")
	}
	var out struct{ Error BridgeError }
	if err = json.Unmarshal([]byte(content.Text), &out); err != nil || out.Error.Code != "session_mismatch" || out.Error.NextAction == "" {
		t.Fatalf("invalid diagnostic: %s %v", content.Text, err)
	}
	// A business failure must not tear down the MCP session.
	if _, err := client.ListTools(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

func TestLostAdmissionEvidenceDoesNotInviteNewSubmission(t *testing.T) {
	root := testRepo(t)
	accepted := make(chan struct{})
	var inputs atomic.Int32
	var loseEvidence atomic.Bool
	loseEvidence.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST":
			_ = json.NewEncoder(w).Encode(SessionRef{ID: "s1", WorkspacePath: root})
		case strings.HasSuffix(r.URL.Path, "/stream"):
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer conn.CloseNow()
			_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"hello"}`))
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
			inputs.Add(1)
			close(accepted)
			for {
				if _, _, err := conn.Read(r.Context()); err != nil {
					return
				}
				inputs.Add(1)
			}
		case strings.HasSuffix(r.URL.Path, "/events"):
			select {
			case <-accepted:
			case <-r.Context().Done():
				return
			}
			if loseEvidence.Load() {
				http.Error(w, "PRIVATE", http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode([]Event{{Kind: "turn_accepted", SessionID: "s1", TurnID: "t1", RequestID: "original", Seq: 10}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := New(ctx, srv.URL, "fixture", root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ref, err := c.Create(ctx, "create")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Send(ctx, ref.ID, "original", "work", "")
	diagnostic := requireDiagnostic(t, err, "admission_unknown")
	if diagnostic.RequestID != "original" || diagnostic.SessionID != "s1" || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("lost exact references or leaked response", err)
	}
	loseEvidence.Store(false)
	admission, err := c.LookupAdmission(ctx, ref.ID, "original")
	if err != nil || admission.Receipt == nil || admission.Receipt.TurnID != "t1" {
		t.Fatalf("original admission not recovered: %+v %v", admission, err)
	}
	_, err = c.Send(ctx, ref.ID, "new-request", "repeat work", "")
	diagnostic = requireDiagnostic(t, err, "admission_unknown")
	if diagnostic.RequestID != "original" {
		t.Fatal("must reconcile the original request, not the blocked new one")
	}
	_, err = c.Send(ctx, ref.ID, "original", "changed payload", "")
	requireDiagnostic(t, err, "request_conflict")
	if inputs.Load() != 1 {
		t.Fatalf("duplicate input: %d", inputs.Load())
	}
}
