package tools

// OriginProvider supplies runtime-owned provenance without coupling the agent
// loop to a tool family or guessing provenance from a tool's name.
type ToolOrigin struct{ Kind, ConnectorID string }
type OriginProvider interface{ ToolOrigin() ToolOrigin }
