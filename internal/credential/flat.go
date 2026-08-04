package credential

// Flat connection id helpers (design-connection-flattening §8.7).
//
// Flattening maps the {namespace,name} credential space to flat connection ids:
//
//	llm/<name>      → <name>     (BYOK connections)
//	gateway/default → "gateway"  (the special gateway connection)
//	mcp/<name>      → ""         (MCP stays independent, §11)
//
// These helpers are the compatibility bridge: secretsJSON keys arriving as
// {namespace}/{name} are normalized to flat ids here, and flat ids are
// reconstructed back to the canonical Target for resolution. They are pure
// additions — no existing resolver consults them yet.

// ConnectionID returns the flat connection id this target maps to under the
// flattening scheme, or "" when the target is not flattenable (MCP and other
// namespaces stay independent, §11; an undefined gateway/<other> is rejected
// rather than guessed).
func (t Target) ConnectionID() string {
	switch t.Namespace {
	case "llm":
		return t.Name
	case "gateway":
		if t.Name == "default" {
			return "gateway"
		}
		return "" // undefined gateway connection
	default:
		return "" // mcp and others
	}
}

// TargetFromConnectionID reconstructs the canonical Target for a flat
// connection id. The special "gateway" id maps to gateway/default; any other
// id maps to llm/<id> (the BYOK convention).
func TargetFromConnectionID(id string) Target {
	if id == "gateway" {
		return Target{Namespace: "gateway", Name: "default"}
	}
	return Target{Namespace: "llm", Name: id}
}
