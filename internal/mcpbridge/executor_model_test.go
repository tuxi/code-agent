package mcpbridge

import (
	"code-agent/internal/collab"
	"context"
	"encoding/json"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestManagedModelCapabilityDoesNotAffectDefaultClients(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tasks/capabilities" {
			t.Error(r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": []string{"task_managed_workflow_v1", "task_review_gate_v1"}})
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := &Client{base: u, http: srv.Client(), token: "offline"}
	if err := c.taskCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.taskCapabilities(context.Background(), "task_executor_model_v1"); !errors.Is(err, collab.ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestManagedMCPModelIsOptionalAndGenerationRemainsPrivate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, b := mcp.NewInMemoryTransports()
	s, err := Server(&Client{}).Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := mcp.NewClient(&mcp.Implementation{Name: "model-contract"}, nil).Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	list, err := c.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range list.Tools {
		if tool.Name != "codeagent_start_task_run" {
			continue
		}
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties map[string]struct {
				Properties map[string]any `json:"properties"`
				Required   []string       `json:"required"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		run := schema.Properties["run"]
		if _, ok := run.Properties["executor_model"]; !ok {
			t.Fatal(string(raw))
		}
		for _, field := range run.Required {
			if field == "executor_model" {
				t.Fatal("model became mandatory")
			}
		}
		for _, field := range []string{"model", "max_tokens", "thinking", "reasoning_effort"} {
			if _, ok := run.Properties[field]; ok {
				t.Fatal("internal control exposed", field)
			}
		}
		return
	}
	t.Fatal("managed start tool missing")
}
