package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// runSkill handles `codeagent skill init <name>`.
func runSkill(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: codeagent skill init <name>")
	}
	switch args[0] {
	case "init":
		if len(args) < 2 {
			return fmt.Errorf("usage: codeagent skill init <name>")
		}
		return skillInit(args[1])
	default:
		return fmt.Errorf("unknown skill subcommand: %s (try init)", args[0])
	}
}

// skillInit scaffolds a new skill directory under ./skills/<name>/.
func skillInit(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("skill name must not be empty")
	}
	root := filepath.Join("skills", name)

	// Create directory layout.
	dirs := []string{
		root,
		filepath.Join(root, "references"),
		filepath.Join(root, "scripts"),
		filepath.Join(root, "assets"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}

	// Write .gitkeep files so empty dirs are tracked.
	for _, d := range dirs[1:] {
		path := filepath.Join(d, ".gitkeep")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}

	// Write SKILL.md with the template.
	mdPath := filepath.Join(root, "SKILL.md")
	if _, err := os.Stat(mdPath); err == nil {
		return fmt.Errorf("skill %q already exists at %s", name, mdPath)
	}
	tmpl := skillTemplate(name)
	if err := os.WriteFile(mdPath, []byte(tmpl), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", mdPath, err)
	}

	fmt.Printf("Created skill %q:\n", name)
	fmt.Printf("  %s\n", mdPath)
	fmt.Printf("  %s/  (add reference docs here)\n", filepath.Join(root, "references"))
	fmt.Printf("  %s/  (add executable scripts here)\n", filepath.Join(root, "scripts"))
	fmt.Printf("  %s/  (add templates, fonts, icons here)\n", filepath.Join(root, "assets"))
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  1. Edit the description in SKILL.md — it is the ONLY thing the model uses to decide when to load this skill.")
	fmt.Println("  2. Keep SKILL.md under 500 lines; use references/ for deeper docs.")
	fmt.Println("  3. Add a ## Gotchas section with real pitfalls as you hit them.")
	fmt.Println("  4. Use 'codeagent plugin install' to share it, or commit skills/ to the project repo.")
	return nil
}

// skillTemplate returns a SKILL.md template pre-filled with authoring guidance.
func skillTemplate(name string) string {
	return fmt.Sprintf(`---
name: %s
version: "1"
description: FIXME — write a clear, specific description of what this skill does and when to use it. This is the ONLY trigger the model sees. Be specific enough to not fire on unrelated tasks, broad enough to fire on real ones. One or two sentences.
---

# %s

FIXME — write the instructions. Follow these principles from the skill authoring guide:

- Give information, not a script. Skills are reused across many situations — over-specific step-by-step instructions backfire. Give the model what it needs to know and leave the judgment to it.
- Don't encode common sense. Spend words on what pushes the model OUT of its defaults: non-obvious rules, real pitfalls, project-specific conventions.
- Use imperative form and concrete examples.

## Quick Start

FIXME — the most common workflow in 3-5 steps.

## References

- references/  — add deeper docs here; reference them from this section so the model knows when to read them.
- scripts/    — add executable utilities here.
- assets/     — add templates, fonts, icons here.

## Gotchas

<!-- Append real pitfalls as the model trips on them. A mature skill's value ≈ its gotchas' value, not its prose. -->
`, name, name)
}
