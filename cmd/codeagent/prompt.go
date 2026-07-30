package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"code-agent/internal/prompt"
)

// runPrompt handles `codeagent prompt <subcommand> [args...]`.
func runPrompt(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: codeagent prompt <init|list> [args...]")
	}
	switch args[0] {
	case "init":
		return promptInit(args[1:])
	case "list":
		return promptList()
	default:
		return fmt.Errorf("unknown prompt subcommand: %s (try init, list)", args[0])
	}
}

func promptInit(args []string) error {
	var global bool
	var desc, hint string
	remaining := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--global", "-global":
			global = true
		case "--description", "-desc":
			if i+1 < len(args) {
				desc = args[i+1]
				i++
			}
		case "--hint", "-hint":
			if i+1 < len(args) {
				hint = args[i+1]
				i++
			}
		default:
			remaining = append(remaining, args[i])
		}
	}
	if len(remaining) < 1 {
		return fmt.Errorf("usage: codeagent prompt init [--description <text>] [--hint <text>] [--global] <name>")
	}
	name := remaining[0]
	if desc == "" {
		desc = "FIXME — describe what this template does"
	}

	root, _ := os.Getwd()
	var dir string
	if global {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("cannot determine home: %w", err)
		}
		dir = filepath.Join(home, ".codeagent", "prompts")
	} else {
		dir = filepath.Join(root, ".codeagent", "prompts")
	}

	if err := prompt.ScaffoldTemplate(dir, name, desc, hint); err != nil {
		return err
	}

	path := filepath.Join(dir, name+".md")
	fmt.Printf("Created %s\n", path)
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  1. Edit the file to define the template body.")
	fmt.Println("  2. Use $1, $2 for positional args, $@ for all args, ${1:-default} for defaults.")
	fmt.Println("  3. Invoke in the REPL or TUI by typing /" + name + " <args>.")
	return nil
}

func promptList() error {
	root, _ := os.Getwd()
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".codeagent", "prompts"))
	}
	dirs = append(dirs, filepath.Join(root, ".codeagent", "prompts"))
	tmpls := prompt.LoadTemplates(dirs...)
	fmt.Print(prompt.FormatTemplateList(tmpls))

	// Show template list in system prompt too, like skills L1 index.
	if len(tmpls) > 0 {
		fmt.Println()
		fmt.Println("These are available as slash commands in the REPL and TUI.")
	}
	return nil
}

// BuildPromptTemplatesIndex returns an L1-style index of available templates,
// suitable for appending to the system prompt so the LLM can suggest them.
func BuildPromptTemplatesIndex(templates []prompt.Template) string {
	if len(templates) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n## Prompt Templates\n\n")
	b.WriteString("The user can invoke these templates by typing /<name>:\n\n")
	for _, t := range templates {
		arg := ""
		if t.ArgHint != "" {
			arg = " " + t.ArgHint
		}
		fmt.Fprintf(&b, "- `/%s%s` — %s\n", t.Name, arg, t.Description)
	}
	return b.String()
}
