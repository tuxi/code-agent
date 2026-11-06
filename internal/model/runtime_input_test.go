package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRuntimeInputProvenanceIsNotProviderJSON(t *testing.T) {
	data, err := json.Marshal(Message{Role: RoleUser, Content: "runtime data", RuntimeInput: true, RuntimeInputReceipts: []string{"private-receipt"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-receipt") || strings.Contains(string(data), "RuntimeInput") || strings.Contains(string(data), "runtime_input") {
		t.Fatalf("runtime metadata leaked: %s", data)
	}
}
