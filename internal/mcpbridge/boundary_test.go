package mcpbridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Exercise the advertised MCP contract, without constructing a Runtime,
// discovering a provider, or contacting a model.
func TestMCPContractKeepsGenerationControlsInRuntime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, b := mcp.NewInMemoryTransports()
	server, err := Server(&Client{}).Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "boundary-test"}, nil).Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	list, err := client.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	foundSend := false
	for _, tool := range list.Tools {
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"max_tokens", "max_output_tokens", "reasoning_effort", "thinking", "temperature", "max_steps"} {
			if strings.Contains(string(raw), `"`+forbidden+`"`) {
				t.Fatalf("%s exposes internal generation control %q", tool.Name, forbidden)
			}
		}
		if tool.Name != "codeagent_send_to_session" {
			continue
		}
		foundSend = true
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		if _, ok := schema.Properties["model"]; !ok {
			t.Fatal("optional model selection disappeared")
		}
		for _, field := range schema.Required {
			if field == "model" {
				t.Fatal("delegation requires model discovery/selection")
			}
		}
	}
	if !foundSend {
		t.Fatal("delegation tool missing")
	}
}
