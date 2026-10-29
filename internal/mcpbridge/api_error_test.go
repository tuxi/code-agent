package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"code-agent/internal/collab"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRuntimeAPIErrorAllowlistAndBoundedPrivacy(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
		status           int
		cause            error
	}{
		{"envelope", `{"trace_id":"09c0b3d1-78d1-4d61-8b8b-685a654522d1","code":40900,"msg":"PRIVATE","data":{"code":"recovery_required","debug":"PRIVATE"}}`, "recovery_required", 409, collab.ErrRecoveryRequired},
		{"bare", `{"code":"managed_workspace","message":"PRIVATE"}`, "managed_workspace", 409, collab.ErrManagedWorkspace},
		{"conflict", `{"data":{"code":"task_conflict"}}`, "task_conflict", 409, collab.ErrConflict},
		{"unknown", `{"data":{"code":"PRIVATE"},"trace_id":"PRIVATE"}`, "runtime_http_error", 409, nil},
		{"wrong_status", `{"data":{"code":"recovery_required"}}`, "runtime_http_error", 500, nil},
		{"malformed", `PRIVATE`, "runtime_http_error", 502, nil},
		{"oversized", `{"code":"managed_workspace","padding":"` + strings.Repeat("x", 17000) + `PRIVATE"}`, "runtime_http_error", 409, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := readRuntimeAPIError(tc.status, strings.NewReader(tc.body))
			var api *RuntimeAPIError
			if !errors.As(err, &api) || api.Code != tc.code || api.HTTPStatus != tc.status {
				t.Fatal(err)
			}
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Fatal(err)
			}
			if strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("response reflected private data", err)
			}
			var decoded map[string]any
			if json.Unmarshal([]byte(err.Error()), &decoded) != nil {
				t.Fatal("not structured", err)
			}
			if tc.name == "envelope" && api.TraceID != "09c0b3d1-78d1-4d61-8b8b-685a654522d1" {
				t.Fatal(api)
			}
		})
	}
}

func TestNativeMCPKeepsStructuredRuntimeError(t *testing.T) {
	c, _ := setup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"code":40900,"msg":"PRIVATE","data":{"code":"recovery_required"}}`))
	}))
	defer srv.Close()
	c.base, _ = url.Parse(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, b := mcp.NewInMemoryTransports()
	s, err := Server(c).Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "offline-error-test"}, nil).Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "codeagent_list_models", Arguments: map[string]any{}})
	if err != nil || !result.IsError {
		t.Fatal(result, err)
	}
	content, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatal(result.Content)
	}
	var out struct {
		Error struct {
			Code       string `json:"code"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error"`
	}
	if err = json.Unmarshal([]byte(content.Text), &out); err != nil || out.Error.Code != "recovery_required" || out.Error.HTTPStatus != 409 || strings.Contains(content.Text, "PRIVATE") {
		t.Fatal(content.Text, err)
	}
}
