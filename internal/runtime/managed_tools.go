package runtime

import (
	"code-agent/internal/tools"
	"code-agent/internal/tools/shell"
)

// Managed v1 cannot prove recovery of arbitrary detached MCP/client/subagent
// work. Keep those execution domains out of this turn-local tool registry,
// preserving the original concrete tools' path checks and approval interfaces.
func managedTurnTools(reg *tools.Registry) *tools.Registry {
	local := tools.Subset(reg, "list_files", "read_file", "create_file", "edit_file", "insert_file", "grep", "load_skill", "todo", "git_diff", "apply_patch", "git_status", "git_log", "run_command")
	if raw, ok := local.Get("run_command"); ok {
		if command, ok := raw.(*shell.RunCommandTool); ok {
			copy := *command
			copy.ForegroundOnly = true
			local.Replace(&copy)
		}
	}
	return local
}
