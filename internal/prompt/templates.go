// Package prompt - Prompt template loading and expansion.
//
// Prompt templates are lightweight, file-based slash commands. Each .md file in
// .codeagent/prompts/ or ~/.codeagent/prompts/ becomes a template the user can
// invoke by typing /<name> [args...]. The template body is expanded with the
// provided arguments and sent to the LLM in place of the raw input.
//
// Templates are simpler than skills: no tool registration, no progressive
// disclosure, no L1/L2/L3 layers — just named markdown files with optional
// YAML frontmatter and bash-style argument substitution ($1, $@, ${N:-default}).
//
// Reference: Pi's packages/coding-agent/src/core/prompt-templates.ts.

package prompt

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Template is a loaded prompt template.
type Template struct {
	Name        string // derived from filename (minus .md)
	Description string // from frontmatter "description" or first body line
	ArgHint     string // from frontmatter "argument-hint", shown in help
	Content     string // raw markdown body (with $1, $@ placeholders)
	FilePath    string // absolute path to the source file
}

// ── Loading ──────────────────────────────────────────────────────────

// LoadTemplates scans dirs (in order) for .md files and returns all loaded
// templates. Project dirs take precedence over global dirs; within a dir,
// files are loaded in lexical order. Missing directories are silent.
func LoadTemplates(dirs ...string) []Template {
	var out []Template
	seen := map[string]bool{}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		tmpls, err := loadTemplatesFromDir(d)
		if err != nil {
			continue
		}
		for _, t := range tmpls {
			if seen[t.Name] {
				continue // project overrides global
			}
			seen[t.Name] = true
			out = append(out, t)
		}
	}
	return out
}

func loadTemplatesFromDir(dir string) ([]Template, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Template
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		t, err := loadTemplateFromFile(path)
		if err != nil {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

func loadTemplateFromFile(path string) (Template, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Template{}, err
	}
	content := string(data)

	name := strings.TrimSuffix(filepath.Base(path), ".md")
	fm, body := parseTemplateFrontmatter(content)

	t := Template{
		Name:     name,
		Content:  strings.TrimSpace(body),
		FilePath: path,
	}
	if d, ok := fm["description"]; ok {
		t.Description = d
	} else {
		// Derive from first non-empty body line, truncated.
		for _, line := range strings.Split(body, "\n") {
			if s := strings.TrimSpace(line); s != "" && !strings.HasPrefix(s, "#") {
				if len(s) > 80 {
					s = s[:80] + "..."
				}
				t.Description = s
				break
			}
		}
	}
	if h, ok := fm["argument-hint"]; ok {
		t.ArgHint = h
	}
	return t, nil
}

// parseTemplateFrontmatter extracts a YAML-like frontmatter block delimited by
// "---" lines. Returns the parsed key-value pairs and the body (everything after
// the closing "---"). When no frontmatter is present, returns nil + full content.
func parseTemplateFrontmatter(content string) (map[string]string, string) {
	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "---\r\n") {
		return nil, content
	}
	// Find closing "---".
	rest := content[3:] // skip opening ---
	if idx := strings.Index(rest, "\n---"); idx >= 0 {
		fm := parseSimpleYAML(rest[:idx])
		body := rest[idx+4:] // skip \n---
		return fm, body
	}
	// No closing delimiter — treat whole thing as body.
	return nil, content
}

// parseSimpleYAML reads key: value lines. Bare minimum; not a full YAML parser.
func parseSimpleYAML(s string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		// skip empty and comment lines
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// ── Expansion ────────────────────────────────────────────────────────

// ExpandTemplate matches text against known templates. If text starts with
// "/<name>" matching a loaded template, it expands the template body with
// the provided arguments. Otherwise it returns the original text unchanged.
func ExpandTemplate(text string, templates []Template) string {
	if !strings.HasPrefix(text, "/") {
		return text
	}
	// Extract /name and trailing args.
	parts := strings.SplitN(text, " ", 2)
	name := strings.TrimPrefix(parts[0], "/")
	var args string
	if len(parts) > 1 {
		args = parts[1]
	}

	for _, t := range templates {
		if t.Name == name {
			return substituteArgs(t.Content, parseCommandArgs(args))
		}
	}
	return text
}

// ── Argument handling ────────────────────────────────────────────────

// parseCommandArgs splits a string into arguments, respecting single and
// double quotes (bash-style).
func parseCommandArgs(s string) []string {
	var args []string
	var current strings.Builder
	inQuote := byte(0)

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inQuote != 0:
			if c == inQuote {
				inQuote = 0
			} else {
				current.WriteByte(c)
			}
		case c == '"' || c == '\'':
			inQuote = c
		case c == ' ' || c == '\t':
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
		default:
			current.WriteByte(c)
		}
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args
}

// substituteArgs replaces bash-style placeholders in the template body:
//
//	$1, $2        → positional args (1-indexed)
//	$@, $ARGUMENTS → all args joined with spaces
//	${N:-default}  → arg N, or "default" when missing/empty
//	${@:-default}  → all args, or "default" when empty
//	${@:N}         → args from Nth onward
//	${@:N:L}       → L args starting from Nth
func substituteArgs(content string, args []string) string {
	allArgs := strings.Join(args, " ")

	re := regexp.MustCompile(
		`\$\{(\d+|ARGUMENTS|@):-([^}]*)\}|\$\{@:(\d+)(?::(\d+))?\}|\$(ARGUMENTS|@|\d+)`,
	)
	return re.ReplaceAllStringFunc(content, func(match string) string {
		// ${N:-default} or ${@:-default} or ${ARGUMENTS:-default}
		if strings.HasPrefix(match, "${") && strings.Contains(match, ":-") {
			// strip ${ and }
			inner := match[2 : len(match)-1]
			parts := strings.SplitN(inner, ":-", 2)
			target, def := parts[0], parts[1]
			switch target {
			case "@", "ARGUMENTS":
				if allArgs == "" {
					return def
				}
				return allArgs
			default:
				n := parseArgIndex(target)
				if n < 0 || n >= len(args) || args[n] == "" {
					return def
				}
				return args[n]
			}
		}

		// ${@:N} or ${@:N:L}
		if strings.HasPrefix(match, "${@:") {
			inner := match[4 : len(match)-1] // strip ${@: and }
			parts := strings.Split(inner, ":")
			start := parseArgIndex(parts[0]) // 1-indexed
			if start < 0 {
				start = 0
			}
			if len(parts) > 1 {
				length := parseInt(parts[1])
				end := start + length
				if end > len(args) {
					end = len(args)
				}
				if start >= len(args) {
					return ""
				}
				return strings.Join(args[start:end], " ")
			}
			if start >= len(args) {
				return ""
			}
			return strings.Join(args[start:], " ")
		}

		// $ARGUMENTS or $@
		if match == "$ARGUMENTS" || match == "$@" {
			return allArgs
		}

		// $N
		n := parseArgIndex(match[1:]) // strip $
		if n < 0 || n >= len(args) {
			return ""
		}
		return args[n]
	})
}

func parseArgIndex(s string) int {
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n - 1 // 1-indexed → 0-indexed
}

func parseInt(s string) int {
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// ── CLI template listing ─────────────────────────────────────────────

// FormatTemplateList returns a human-readable listing of loaded templates.
func FormatTemplateList(templates []Template) string {
	if len(templates) == 0 {
		return "(no prompt templates loaded)"
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%d prompt template(s):\n", len(templates)))
	for _, t := range templates {
		arg := ""
		if t.ArgHint != "" {
			arg = " " + t.ArgHint
		}
		fmt.Fprintf(&b, "  /%s%s\n    %s\n", t.Name, arg, t.Description)
	}
	return b.String()
}

// ScaffoldTemplate creates a new template file at dir/<name>.md with a
// starter frontmatter and body. Returns an error when the file already exists.
func ScaffoldTemplate(dir, name, description, argHint string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, name+".md")
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("template %q already exists at %s", name, path)
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString(fmt.Sprintf("description: %s\n", description))
	if argHint != "" {
		b.WriteString(fmt.Sprintf("argument-hint: %q\n", argHint))
	}
	b.WriteString("---\n\n")
	b.WriteString("# " + name + "\n\n")
	b.WriteString("FIXME — describe what this template does and how to use it.\n")
	b.WriteString("Use $1, $2 for positional arguments, $@ or $ARGUMENTS for all arguments.\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
