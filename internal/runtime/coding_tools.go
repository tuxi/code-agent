package runtime

import (
	"code-agent/internal/agent"
	"code-agent/internal/settings"
	"code-agent/internal/tools"
)

// Stable allow-list: adding an extension does not enlarge a coding request.
// Task remains available for the runtime's independent review contract.
var codingToolNames = []string{
	"list_files", "read_file", "grep", "edit_file", "create_file",
	"run_command", "job_status", "job_logs", "job_wait", "job_cancel",
	"load_skill", "recall_memory", "list_memories", "create_memory", "update_memory", "delete_memory",
	"todo_write", "ask_user", "enter_plan_mode", "propose_plan", "task", "git_diff",
}

func applyCodingProfile(runner *agent.Runner, cfg settings.Settings) {
	if cfg.Agent.CodingTools == nil || !*cfg.Agent.CodingTools || runner.Tools == nil {
		return
	}
	runner.Tools = tools.Subset(runner.Tools, codingToolNames...)
	runner.PlanTools = tools.Subset(runner.Tools, PlanModeToolNames...)
}
