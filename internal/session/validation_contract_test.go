package session

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidationContractStrictDecodeAndStableHash(t *testing.T) {
	var a, b ValidationContract
	for _, tc := range []struct {
		data string
		out  *ValidationContract
	}{{`{"version":1,"validation_commands":["swift test"],"manual_checks":["检查横屏"]}`, &a}, {`{ "manual_checks": ["检查横屏"], "validation_commands": ["swift test"], "version": 1 }`, &b}} {
		if err := json.Unmarshal([]byte(tc.data), tc.out); err != nil {
			t.Fatal(err)
		}
	}
	x, _ := a.Hash()
	y, _ := b.Hash()
	if x != y || len(x) != 64 {
		t.Fatal("formatting changed identity", x, y)
	}
	b.ManualChecks[0] = "检查分屏"
	y, _ = b.Hash()
	if x == y {
		t.Fatal("manual requirement excluded from identity")
	}
	a.ValidationCommands = []string{"one", "two"}
	b = a
	b.ValidationCommands = []string{"two", "one"}
	x, _ = a.Hash()
	y, _ = b.Hash()
	if x == y {
		t.Fatal("command order discarded")
	}
	copy, err := a.CloneValidated()
	if err != nil {
		t.Fatal(err)
	}
	copy.ValidationCommands[0] = "changed"
	if a.ValidationCommands[0] != "one" {
		t.Fatal("copy aliased caller data")
	}
	for _, data := range []string{`null`, `{"version":2}`, `{"version":1,"max_tokens":42}`, `{"version":1,"validation_commands":[""]}`, `{"version":1,"manual_checks":["  "]}`, `{"version":1,"validation_commands":{}}`, `{"version":1} {}`, `{"version":2,"version":1}`, `{"Version":1}`} {
		var bad ValidationContract
		if json.Unmarshal([]byte(data), &bad) == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestValidationContractLimitsAndLegacyMetadata(t *testing.T) {
	for _, c := range []ValidationContract{{Version: 1, ValidationCommands: make([]string, 33)}, {Version: 1, ManualChecks: make([]string, 33)}, {Version: 1, ValidationCommands: []string{strings.Repeat("a", 4097)}}, {Version: 1, ManualChecks: []string{strings.Repeat("a", 1025)}}, {Version: 1, ValidationCommands: []string{string([]byte{255})}}} {
		if _, err := c.CloneValidated(); err == nil {
			t.Fatal("invalid size/encoding accepted")
		}
	}
	for _, payload := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`{"Kind":"turn_accepted"}`)} {
		c, err := AcceptedValidation(EventRecord{Payload: payload}, TurnInput{})
		if err != nil || c != nil {
			t.Fatal("legacy metadata fabricated", c, err)
		}
	}
}

func TestAcceptedValidationChecksIdentityAndContent(t *testing.T) {
	input := TurnInput{SessionID: "s", TurnID: "t", RequestID: "r"}
	c := ValidationContract{Version: 1, ValidationCommands: []string{"go version"}}
	hash, _ := c.Hash()
	fields := map[string]any{"SessionID": "s", "TurnID": "t", "RequestID": "r", "validation_contract": c, "validation_contract_hash": hash}
	record := EventRecord{SessionID: "s", TurnID: "t", Kind: "turn_accepted"}
	record.Payload, _ = json.Marshal(fields)
	if got, err := AcceptedValidation(record, input); err != nil || got.ValidationCommands[0] != "go version" {
		t.Fatal(got, err)
	}
	for _, mutation := range []func(map[string]any){func(m map[string]any) { m["RequestID"] = "other" }, func(m map[string]any) { m["validation_contract_hash"] = "wrong" }, func(m map[string]any) { delete(m, "validation_contract_hash") }, func(m map[string]any) { m["validation_contract"] = nil }, func(m map[string]any) { m["session_id"] = "conflicting" }} {
		copy := map[string]any{}
		for k, v := range fields {
			copy[k] = v
		}
		mutation(copy)
		record.Payload, _ = json.Marshal(copy)
		if _, err := AcceptedValidation(record, input); err == nil {
			t.Fatal("mismatched acceptance trusted", string(record.Payload))
		}
	}
}
