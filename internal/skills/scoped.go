package skills

import (
	"path/filepath"
	"strings"
)

// Scope identifies who owns a skill and which precedence tier it belongs to.
type Scope string

const (
	ScopeSystem  Scope = "system"
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
	ScopePlugin  Scope = "plugin"
)

// Root is one on-disk skill root. PluginName is set only for plugin roots and
// makes every skill name <pluginName>:<frontmatter-name>.
type Root struct {
	Dir        string
	Scope      Scope
	Source     string
	PluginName string
}

// LoadError describes a skill that could not be loaded or was shadowed.
type LoadError struct {
	Label  string
	Scope  Scope
	Source string
	Path   string
	Err    string
}

// LoadOptions is the scoped loader contract used by runtime hosts and the plugin
// foundation. Ordinary roots are applied system -> user -> project. ProjectDirs
// are applied in caller order, so later entries win within the same scope.
type LoadOptions struct {
	SystemDir   string
	UserDir     string
	ProjectDirs []string
	Plugins     []Root
}

// WorkspaceLoadOptions applies the CodeAgent path contract shared by the CLI,
// daemon, and embedded hosts. The user root remains ~/.codeagent/skills for
// compatibility; code-agent-managed content lives only below .system.
func WorkspaceLoadOptions(globalSkillsDir, workspaceRoot string) LoadOptions {
	opts := LoadOptions{}
	if globalSkillsDir != "" {
		opts.SystemDir = filepath.Join(globalSkillsDir, ".system")
		opts.UserDir = globalSkillsDir
	}
	if workspaceRoot != "" {
		opts.ProjectDirs = []string{
			filepath.Join(workspaceRoot, "skills"),
			filepath.Join(workspaceRoot, ".agents", "skills"),
		}
	}
	return opts
}

// LoadScoped loads all configured scopes into one registry. Plugin skills never
// replace ordinary skills: their public names are namespaced by plugin name.
func LoadScoped(opts LoadOptions) (*Registry, error) {
	return loadRoots(scopedRoots(opts))
}

func scopedRoots(opts LoadOptions) []Root {
	roots := make([]Root, 0, 3+len(opts.ProjectDirs)+len(opts.Plugins))
	if opts.SystemDir != "" {
		roots = append(roots, Root{Dir: opts.SystemDir, Scope: ScopeSystem, Source: "system"})
	}
	if opts.UserDir != "" {
		roots = append(roots, Root{Dir: opts.UserDir, Scope: ScopeUser, Source: "user"})
	}
	for _, dir := range opts.ProjectDirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		roots = append(roots, Root{Dir: dir, Scope: ScopeProject, Source: "project"})
	}
	for _, plugin := range opts.Plugins {
		if plugin.Dir == "" || strings.TrimSpace(plugin.PluginName) == "" {
			continue
		}
		plugin.Scope = ScopePlugin
		plugin.Source = "plugin:" + plugin.PluginName
		roots = append(roots, plugin)
	}
	return roots
}
