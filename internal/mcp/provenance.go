package mcp

import "code-agent/internal/tools"

func (t *remoteTool) ToolOrigin() tools.ToolOrigin {
	return tools.ToolOrigin{Kind: "mcp", ConnectorID: "mcp:" + t.server}
}
func (t *resourceListTool) ToolOrigin() tools.ToolOrigin {
	return tools.ToolOrigin{Kind: "mcp", ConnectorID: "mcp:" + t.server}
}
func (t *resourceReadTool) ToolOrigin() tools.ToolOrigin {
	return tools.ToolOrigin{Kind: "mcp", ConnectorID: "mcp:" + t.server}
}
