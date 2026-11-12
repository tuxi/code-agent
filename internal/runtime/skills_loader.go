package runtime

import (
	"code-agent/internal/plugins"
	"code-agent/internal/skills"
)

func loadWorkspaceSkills(globalSkillsDir, workspaceRoot string) (skills.LoadOptions, *skills.Registry, error) {
	opts := skills.WorkspaceLoadOptions(globalSkillsDir, workspaceRoot)
	if globalSkillsDir != "" {
		roots, err := plugins.SkillRootsForSkillsDir(globalSkillsDir)
		if err != nil {
			return opts, nil, err
		}
		opts.Plugins = roots
	}
	registry, err := skills.LoadScoped(opts)
	return opts, registry, err
}
