package tools

import (
	"encoding/json"
	"sort"
	"strconv"

	"github.com/google/jsonschema-go/jsonschema"
)

// ArgumentError is a tool result the model can correct, never an approval
// request. Invalid schemas are runtime configuration errors instead.
type ArgumentError struct {
	Code             string `json:"code"`
	Tool             string `json:"tool"`
	Field            string `json:"field,omitempty"`
	Message          string `json:"message"`
	RetryableByModel bool   `json:"retryable_by_model"`
}

func (e *ArgumentError) JSON() json.RawMessage {
	b, _ := json.Marshal(struct {
		Error *ArgumentError `json:"error"`
	}{e})
	return b
}

func (e *ArgumentError) Error() string { return string(e.JSON()) }

// ValidateInput performs structural validation before hooks, inspection,
// approval or execution. It grants no permission and resolves no paths. Remote
// schema loading is disabled; only embedded/local schema references can resolve.
// Tools without a schema retain syntax-only validation for compatibility.
func ValidateInput(tool Tool, input json.RawMessage) *ArgumentError {
	fail := func(field, message string) *ArgumentError {
		return &ArgumentError{Code: "invalid_tool_arguments", Tool: tool.Name(), Field: field, Message: message, RetryableByModel: true}
	}
	var value any
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	if json.Unmarshal(input, &value) != nil {
		return fail("", "arguments must be valid JSON")
	}
	raw := tool.InputSchema()
	if len(raw) == 0 {
		return nil
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return &ArgumentError{Code: "invalid_tool_schema", Tool: tool.Name(), Message: "tool input schema is invalid"}
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return &ArgumentError{Code: "invalid_tool_schema", Tool: tool.Name(), Message: "tool input schema cannot be resolved locally"}
	}
	if err := resolved.Validate(value); err != nil {
		if field := missingRequired(&schema, value, ""); field != "" {
			return fail(field, "required field is missing")
		}
		// Do not echo validator errors: they can contain entire argument values.
		return fail("", "arguments do not match the tool input schema (check types, enum values and constraints)")
	}
	return nil
}

func missingRequired(schema *jsonschema.Schema, value any, path string) string {
	if schema == nil {
		return ""
	}
	if object, ok := value.(map[string]any); ok {
		for _, name := range schema.Required {
			if _, present := object[name]; !present {
				return path + name
			}
		}
		keys := make([]string, 0, len(schema.Properties))
		for key := range schema.Properties {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if v, present := object[key]; present {
				if field := missingRequired(schema.Properties[key], v, path+key+"."); field != "" {
					return field
				}
			}
		}
	}
	if array, ok := value.([]any); ok {
		for i, v := range array {
			if field := missingRequired(schema.Items, v, path+strconv.Itoa(i)+"."); field != "" {
				return field
			}
		}
	}
	return ""
}
