package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

type schemaProbe struct {
	plainTool
	schema json.RawMessage
}

func (s *schemaProbe) InputSchema() json.RawMessage { return s.schema }

func TestValidateInputSchema(t *testing.T) {
	schema := `{"type":"object","properties":{"path":{"type":"string","minLength":1},"mode":{"enum":["read","write"]},"count":{"type":"integer","minimum":1},"targets":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}}},"required":["path"],"additionalProperties":false}`
	for _, tc := range []struct {
		name, input, field string
		valid              bool
	}{
		{"missing", `{}`, "path", false},
		{"syntax", `{"path":`, "", false},
		{"null", `null`, "", false},
		{"wrong root", `[]`, "", false},
		{"wrong type", `{"path":123}`, "", false},
		{"null path", `{"path":null}`, "", false},
		{"empty path", `{"path":""}`, "", false},
		{"enum", `{"path":"a","mode":"remove"}`, "", false},
		{"integer", `{"path":"a","count":1.5}`, "", false},
		{"nested required", `{"path":"a","targets":[{}]}`, "targets.0.id", false},
		{"extra property", `{"path":"a","secret":"do-not-echo"}`, "", false},
		{"valid", `{"path":"a","mode":"write","count":1,"targets":[{"id":"b"}]}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := &schemaProbe{plainTool: plainTool{name: "probe"}, schema: json.RawMessage(schema)}
			err := ValidateInput(tool, json.RawMessage(tc.input))
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || err.Code != "invalid_tool_arguments" || err.Tool != "probe" || err.Field != tc.field || !err.RetryableByModel {
				t.Fatalf("unexpected validation: %+v", err)
			}
			if strings.Contains(err.Error(), "do-not-echo") || !json.Valid(err.JSON()) {
				t.Fatalf("error must be structured and omit argument values: %s", err)
			}
		})
	}
}

func TestValidateInputReferencesAndLegacy(t *testing.T) {
	tool := &schemaProbe{plainTool: plainTool{name: "refs"}, schema: json.RawMessage(`{"type":"object","$defs":{"target":{"type":"string"}},"properties":{"path":{"$ref":"#/$defs/target"}},"required":["path"]}`)}
	if err := ValidateInput(tool, json.RawMessage(`{"path":"file"}`)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateInput(tool, json.RawMessage(`{"path":42}`)); err == nil {
		t.Fatal("local reference was not validated")
	}
	tool.schema = json.RawMessage(`{"$ref":"https://example.invalid/schema"}`)
	if err := ValidateInput(tool, json.RawMessage(`{}`)); err == nil || err.Code != "invalid_tool_schema" || err.RetryableByModel {
		t.Fatalf("remote schema must fail locally as a configuration error: %+v", err)
	}
	tool.schema = json.RawMessage(`{"type":"object","properties":{"value":{"anyOf":[{"type":"integer"},{"type":"null"}]}},"required":["value"]}`)
	for _, input := range []string{`{"value":1}`, `{"value":null}`} {
		if err := ValidateInput(tool, json.RawMessage(input)); err != nil {
			t.Fatal(err)
		}
	}
	if err := ValidateInput(tool, json.RawMessage(`{"value":"bad"}`)); err == nil {
		t.Fatal("anyOf was not validated")
	}
	tool.schema = nil
	if err := ValidateInput(tool, nil); err != nil {
		t.Fatal(err)
	}
	if err := ValidateInput(tool, json.RawMessage(`{`)); err == nil {
		t.Fatal("malformed legacy input accepted")
	}
	tool.schema = json.RawMessage(`{"type":"object"}`)
	if err := ValidateInput(tool, nil); err != nil {
		t.Fatal(err)
	}
}
