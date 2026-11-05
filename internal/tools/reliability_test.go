package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClosedSchemaReportsFieldNamesWithoutValues(t *testing.T) {
	probe := &schemaProbe{plainTool: plainTool{name: "edit"}, schema: Object(map[string]Property{"new": {Type: "string"}}, "new").Closed().JSON()}
	err := ValidateInput(probe, json.RawMessage(`{"new":"ok","new_string":"private-content"}`))
	if err == nil || !strings.Contains(err.Message, "new_string") || !strings.Contains(err.Message, "allowed fields: new") || strings.Contains(err.Error(), "private-content") {
		t.Fatalf("diagnostic=%v", err)
	}
	if err := ValidateInput(probe, json.RawMessage(`{"new":""}`)); err != nil {
		t.Fatalf("explicit empty string rejected: %v", err)
	}
	probe.schema = Object(map[string]Property{"new": {Type: "string"}}, "new").JSON()
	if err := ValidateInput(probe, json.RawMessage(`{"new":"ok","extension":true}`)); err != nil {
		t.Fatal("open external contract was closed")
	}
}
