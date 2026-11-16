package server

import (
	"code-agent/internal/agent"
	"encoding/json"
	"testing"
)

func TestContextMaintenanceWirePreservesUnknownAndCorrelation(t *testing.T) {
	p := &agent.ContextMaintenance{OperationID: "op", Status: "pending", Phase: "measuring", Committed: true, SummaryGenerated: true, MeasurementSource: "unknown"}
	data, err := json.Marshal(toWire(agent.Event{Kind: agent.EventContextMaintenance, ContextMaintenance: p}))
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err = json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	payload := value["context_maintenance"].(map[string]any)
	if _, ok := payload["after_tokens"]; ok {
		t.Fatal("unknown after_tokens became zero")
	}
	if payload["operation_id"] != "op" || payload["status"] != "pending" {
		t.Fatal(string(data))
	}
	legacy := toWire(agent.Event{Kind: agent.EventCompacted, ContextOperationID: "op"})
	if legacy.ContextOperationID != "op" {
		t.Fatal("legacy correlation missing")
	}
}
