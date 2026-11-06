package runtime

import (
	"code-agent/internal/jobs"
	"code-agent/internal/tools"
	"code-agent/internal/tools/shell"
)

// BackgroundJobs returns the same registry used by the assembled shell tools.
// Sandboxed hosts without subprocess tools do not expose job controls.
func BackgroundJobs(registry *tools.Registry) *jobs.Registry {
	if registry != nil {
		if tool, ok := registry.Get("run_command"); ok {
			if command, ok := tool.(*shell.RunCommandTool); ok {
				return command.Jobs
			}
		}
	}
	return nil
}
