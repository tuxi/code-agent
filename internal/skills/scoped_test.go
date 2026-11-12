package skills

import "testing"

func TestLoadScopedPrecedence(t *testing.T) {
	systemDir, userDir, projectDir := t.TempDir(), t.TempDir(), t.TempDir()
	writeSkill(t, systemDir, "same", "---\nname: same\ndescription: system\n---\nsystem")
	writeSkill(t, userDir, "same", "---\nname: same\ndescription: user\n---\nuser")
	writeSkill(t, projectDir, "same", "---\nname: same\ndescription: project\n---\nproject")
	writeSkill(t, systemDir, "system-only", "---\nname: system-only\ndescription: system only\n---\nsystem")

	r, err := LoadScoped(LoadOptions{SystemDir: systemDir, UserDir: userDir, ProjectDirs: []string{projectDir}})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := r.Get("same")
	if !ok || got.Body != "project" || got.Scope != ScopeProject {
		t.Fatalf("same = %#v, want project scope/body", got)
	}
	got, ok = r.Get("system-only")
	if !ok || got.Scope != ScopeSystem || !got.Enabled || got.Path == "" || got.ContentHash == "" {
		t.Fatalf("system-only = %#v", got)
	}
}

func TestLoadScopedPluginNamespaceAndProjectIsolation(t *testing.T) {
	pluginDir := t.TempDir()
	writeSkill(t, pluginDir, "search", "---\nname: search\ndescription: plugin search\n---\nplugin")
	projectA, projectB := t.TempDir(), t.TempDir()
	writeSkill(t, projectA, "only-a", "---\nname: only-a\ndescription: only A\n---\na")

	r, err := LoadScoped(LoadOptions{
		ProjectDirs: []string{projectA},
		Plugins:     []Root{{Dir: pluginDir, PluginName: "docs"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("search"); ok {
		t.Fatal("plugin skill must not be exposed unqualified")
	}
	if got, ok := r.Get("docs:search"); !ok || got.Scope != ScopePlugin || got.Source != "plugin:docs" {
		t.Fatalf("docs:search = %#v", got)
	}
	if _, ok := r.Get("only-a"); !ok {
		t.Fatal("project skill missing")
	}

	other, err := LoadScoped(LoadOptions{ProjectDirs: []string{projectB}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := other.Get("only-a"); ok {
		t.Fatal("project skill leaked into another workspace")
	}
}

func TestWorkspaceLoadOptionsSupportsAgentsCompatibilityPath(t *testing.T) {
	global, workspace := t.TempDir(), t.TempDir()
	legacy := workspace + "/skills"
	agents := workspace + "/.agents/skills"
	writeSkill(t, legacy, "same", "---\nname: same\ndescription: legacy\n---\nlegacy")
	writeSkill(t, agents, "same", "---\nname: same\ndescription: agents\n---\nagents")

	opts := WorkspaceLoadOptions(global, workspace)
	if len(opts.ProjectDirs) != 2 || opts.ProjectDirs[0] != legacy || opts.ProjectDirs[1] != agents {
		t.Fatalf("project dirs = %#v", opts.ProjectDirs)
	}
	r, err := LoadScoped(opts)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := r.Get("same")
	if !ok || got.Body != "agents" {
		t.Fatalf("same = %#v, want .agents/skills precedence", got)
	}
}

func TestPluginCannotOverrideOrdinaryNamespacedLookalike(t *testing.T) {
	userDir, pluginDir := t.TempDir(), t.TempDir()
	writeSkill(t, userDir, "ordinary", "---\nname: docs:search\ndescription: ordinary\n---\nordinary")
	writeSkill(t, pluginDir, "search", "---\nname: search\ndescription: plugin\n---\nplugin")
	r, err := LoadScoped(LoadOptions{
		UserDir: userDir,
		Plugins: []Root{{Dir: pluginDir, PluginName: "docs"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := r.Get("docs:search")
	if !ok || got.Scope != ScopeUser || got.Body != "ordinary" {
		t.Fatalf("docs:search = %#v, want ordinary user skill", got)
	}
}
