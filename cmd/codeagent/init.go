package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// runInit handles `codeagent init [flags]`.
func runInit(args []string) error {
	var global bool
	var dryRun bool
	var force bool
	var lang string

	// Parse flags from positional args.
	remaining := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--global", "-global":
			global = true
		case "--dry-run", "-dry-run":
			dryRun = true
		case "--force", "-force":
			force = true
		case "--lang", "-lang":
			if i+1 < len(args) {
				lang = args[i+1]
				i++
			}
		default:
			remaining = append(remaining, args[i])
		}
	}
	if len(remaining) > 0 {
		return fmt.Errorf("unexpected argument: %s", remaining[0])
	}

	root, _ := os.Getwd()
	var targetPath string
	if global {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("cannot determine home directory: %w", err)
		}
		codeagentDir := filepath.Join(home, ".codeagent")
		if err := os.MkdirAll(codeagentDir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", codeagentDir, err)
		}
		targetPath = filepath.Join(codeagentDir, "AGENTS.md")
	} else {
		targetPath = filepath.Join(root, "AGENTS.md")
	}

	// Check if file already exists.
	if _, err := os.Stat(targetPath); err == nil && !force {
		return fmt.Errorf("%s already exists. Use --force to overwrite, or --dry-run to preview.", targetPath)
	}

	// Detect language if not explicitly provided.
	if lang == "" && !global {
		lang = detectLanguage(root)
	}

	content := agentsTemplate(lang, global)

	if dryRun {
		fmt.Printf("Would write to: %s\n", targetPath)
		fmt.Println("---")
		fmt.Print(content)
		return nil
	}

	if err := os.WriteFile(targetPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", targetPath, err)
	}

	if global {
		fmt.Printf("Created global AGENTS.md at %s\n", targetPath)
		fmt.Println("This file applies to ALL projects on this machine.")
	} else {
		fmt.Printf("Created %s\n", targetPath)
	}
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  1. Review and customize the rules — they are starting defaults, not a final answer.")
	fmt.Println("  2. Keep it concise: high-frequency rules stay; low-frequency workflows go into skills/.")
	fmt.Println("  3. As you discover new patterns, add them. As rules become obvious, remove them.")
	fmt.Println("  4. Run with --no-context-files (-nc) to temporarily disable context file loading.")

	return nil
}

// detectLanguage tries to identify the project's primary language from common
// marker files. Returns "go" if go.mod exists, "node" for package.json, etc.
// Returns empty string when nothing is recognized.
func detectLanguage(root string) string {
	markers := []struct {
		file string
		lang string
	}{
		{"go.mod", "go"},
		{"package.json", "node"},
		{"Cargo.toml", "rust"},
		{"pyproject.toml", "python"},
		{"setup.py", "python"},
		{"requirements.txt", "python"},
		{"Gemfile", "ruby"},
		{"Package.swift", "swift"},
		{"CMakeLists.txt", "cpp"},
		{"Makefile", "c"},
		{"pom.xml", "java"},
		{"build.gradle", "java"},
		{"build.gradle.kts", "java"},
	}
	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(root, m.file)); err == nil {
			return m.lang
		}
	}
	return ""
}

// agentsTemplate returns the AGENTS.md template for a given language.
// When global is true, the project conventions section is omitted.
func agentsTemplate(lang string, global bool) string {
	var conventions string
	switch lang {
	case "go":
		conventions = goConventions
	case "node":
		conventions = nodeConventions
	case "rust":
		conventions = rustConventions
	case "python":
		conventions = pythonConventions
	default:
		conventions = genericConventions
	}

	var b strings.Builder
	b.WriteString(`# Project Rules

## Core Principles

This project uses an AI-native coding agent. The LLM decides what to do next;
the runtime controls what can actually be executed. Tools are explicit, typed,
observable capabilities.

- Inspect before editing. Read files in full before making changes.
- Plan before complex changes. Ask when requirements are ambiguous.
- Every step must be traceable. No hidden automation.
- Never apply patches silently. Show git diff after changes.
- Validate with tests when command execution is available.
- Keep answers short and concise. Technical prose only.
`)

	b.WriteString(`
## Safety Hardlines

These rules are non-negotiable. Violating them can destroy work.

- NEVER: ` + "`git reset --hard`" + `, ` + "`git checkout .`" + `, ` + "`git clean -fd`" + `
- NEVER: ` + "`git stash`" + `, ` + "`git add -A`" + `, ` + "`git add .`" + `
- NEVER: ` + "`git push --force`" + ` or ` + "`git push --force-with-lease`" + `
- NEVER: silently delete or overwrite user files without confirmation
- NEVER: run unapproved dependency changes without user consent
- When in doubt, ask before acting.
`)

	if !global {
		b.WriteString(conventions)
	}

	b.WriteString(`
## Skills

This project uses a progressive-disclosure skill system (` + "`skills/`" + ` directory).
Skills are task-specific playbooks loaded on demand. When you repeatedly give
similar instructions, consider moving them into a skill via ` + "`codeagent skill init <name>`" + `.

<!-- This file was generated by ` + "`codeagent init`" + `. Customize it as you learn what
your agent needs. Rules should be high-frequency and non-obvious — low-frequency
workflows belong in skills. -->
`)

	return b.String()
}

const goConventions = `
## Project Conventions

- Language: Go
- Build: ` + "`go build ./...`" + `
- Test: ` + "`go test ./...`" + `
- Lint: ` + "`go vet ./...`" + `
- Format: ` + "`gofmt -w .`" + ` (or ` + "`goimports`" + `)
- Commit message format: concise, technical, no emojis
- Do not commit unless the user explicitly asks
- Run specific package tests with ` + "`go test ./internal/<pkg>/...`" + `; avoid full test suite unless asked
`

const nodeConventions = `
## Project Conventions

- Language: TypeScript / JavaScript
- Package manager: npm
- Install: ` + "`npm install --ignore-scripts`" + `
- Build: ` + "`npm run build`" + `
- Test: ` + "`npm test`" + ` or ` + "`npx vitest --run`" + `
- Lint: ` + "`npm run check`" + ` or ` + "`npx biome check .`" + `
- Commit message format: {feat,fix,docs}: <description>
- Do not commit unless the user explicitly asks
- Use top-level imports only; no dynamic ` + "`await import()`" + `
`

const rustConventions = `
## Project Conventions

- Language: Rust
- Build: ` + "`cargo build`" + `
- Test: ` + "`cargo test`" + `
- Lint: ` + "`cargo clippy`" + `
- Format: ` + "`cargo fmt`" + `
- Commit message format: concise, technical, no emojis
- Do not commit unless the user explicitly asks
`

const pythonConventions = `
## Project Conventions

- Language: Python
- Virtual env: ` + "`source .venv/bin/activate`" + ` (or ` + "`poetry shell`" + `)
- Install: ` + "`pip install -e .`" + ` or ` + "`poetry install`" + `
- Test: ` + "`pytest`" + `
- Lint: ` + "`ruff check .`" + `
- Format: ` + "`ruff format .`" + `
- Commit message format: concise, technical, no emojis
- Do not commit unless the user explicitly asks
`

const genericConventions = `
## Project Conventions

<!-- Auto-detection did not find a recognized project file (go.mod, package.json,
Cargo.toml, etc.). Fill in the conventions for your project below. -->

- Language: FIXME
- Build: FIXME
- Test: FIXME
- Lint: FIXME
- Commit message format: concise, technical, no emojis
- Do not commit unless the user explicitly asks
`
