package tools

import "encoding/json"

// Schema is a small, ergonomic builder for the subset of JSON Schema that tool
// inputs need: an object with typed, described properties.
//
// It is intentionally NOT a full JSON Schema implementation. JSON Schema is
// large and recursive; modelling all of it (oneOf/anyOf/$ref/format/...) would
// be a project of its own. For anything this builder cannot express, a tool may
// return a raw json.RawMessage from InputSchema() directly — the interface
// boundary stays bytes precisely to keep that escape hatch open.
type Schema struct {
	Type        string              `json:"type"`
	Description string              `json:"description,omitempty"`
	Properties  map[string]Property `json:"properties,omitempty"`
	Required    []string            `json:"required,omitempty"`
	Items       *Property           `json:"items,omitempty"` // when Type == "array"
}

// Property describes a single field of an object schema. Type is one of
// "string", "integer", "number", "boolean", "array", "object".
type Property struct {
	Type        string              `json:"type"`
	Description string              `json:"description,omitempty"`
	Enum        []string            `json:"enum,omitempty"`       // closed set of string values
	Items       *Property           `json:"items,omitempty"`      // when Type == "array"
	Properties  map[string]Property `json:"properties,omitempty"` // when Type == "object"
	Required    []string            `json:"required,omitempty"`   // when Type == "object"
}

// JSON marshals the schema to the json.RawMessage that InputSchema() returns.
// A schema is static, author-defined data, so a marshal failure is a
// programming bug that should surface immediately (and is caught by the tool
// definition tests). In practice these structs cannot fail to marshal.
func (s Schema) JSON() json.RawMessage {
	data, err := json.Marshal(s)
	if err != nil {
		panic("tools: invalid schema: " + err.Error())
	}
	return data
}

// Object is the convenience constructor for the common
// "object with properties" case, which covers essentially every tool input.
//
//	tools.Object(map[string]tools.Property{
//	    "path": {Type: "string", Description: "..."},
//	}, "path")
func Object(properties map[string]Property, required ...string) Schema {
	return Schema{
		Type:       "object",
		Properties: properties,
		Required:   required,
	}
}
