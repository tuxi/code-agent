// Package memory provides file-based per-workspace persistent memory.
//
// Memories are markdown files under <workspace>/.codeagent/memory/.
// Each file has optional YAML frontmatter (name, description) and a
// markdown body. Unlike AGENTS.md which is loaded in full every turn,
// memories are recalled on-demand via the recall_memory tool.
//
// Reference: Claude Code's ~/.claude/projects/<project>/memory/ system.
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Memory is a single persisted memory entry.
type Memory struct {
	Name        string // slug, e.g. "go-test-conventions"
	Description string // one-line summary used for relevance matching
	Content     string // markdown body
	FilePath    string // absolute path on disk
}

// Store manages a file-based memory directory.
type Store struct {
	dir string
}

// Open returns a Store rooted at dir directly — the caller decides
// where memories live (e.g. <workspace>/.codeagent/memory/).
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Dir returns the on-disk path for this store.
func (s *Store) Dir() string { return s.dir }

// Create writes a new memory file. Returns an error when a memory with the
// same slug already exists.
func (s *Store) Create(name, description, content string) (*Memory, error) {
	name = slugify(name)
	path := filepath.Join(s.dir, name+".md")
	m := &Memory{Name: name, Description: description, Content: content, FilePath: path}
	// O_EXCL ensures atomic check-and-create, no TOCTOU window.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("memory %q already exists", name)
		}
		return nil, err
	}
	f.Close()
	if err := s.write(m); err != nil {
		return nil, err
	}
	return m, nil
}

// Update writes a memory. If a memory with this name already exists it is
// overwritten; otherwise a new memory is created (upsert semantics).
func (s *Store) Update(name, description, content string) error {
	name = slugify(name)
	path := filepath.Join(s.dir, name+".md")
	m := &Memory{Name: name, Description: description, Content: content, FilePath: path}
	return s.write(m)
}

// Delete removes a memory file.
func (s *Store) Delete(name string) error {
	name = slugify(name)
	path := filepath.Join(s.dir, name+".md")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Get reads a single memory by name.
func (s *Store) Get(name string) (*Memory, error) {
	name = slugify(name)
	path := filepath.Join(s.dir, name+".md")
	return s.read(path)
}

// List returns all memories in the store, sorted by name.
func (s *Store) List() ([]Memory, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var mems []Memory
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || e.Name() == "MEMORY.md" {
			continue
		}
		m, err := s.read(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		mems = append(mems, *m)
	}
	sort.Slice(mems, func(i, j int) bool { return mems[i].Name < mems[j].Name })
	return mems, nil
}

// Recall returns up to limit memories whose description or content contains
// any token from query. A simple scan — enough for a few dozen memories.
func (s *Store) Recall(query string, limit int) ([]Memory, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	if query == "" || limit <= 0 {
		return nil, nil
	}
	tokens := strings.Fields(strings.ToLower(query))
	type entry struct {
		m    Memory
		hits int
	}
	var results []entry
	for _, m := range all {
		text := strings.ToLower(m.Description + " " + m.Content)
		hits := 0
		for _, t := range tokens {
			if len(t) > 2 && strings.Contains(text, t) {
				hits++
			}
		}
		if hits > 0 {
			results = append(results, entry{m, hits})
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].hits > results[j].hits })
	if len(results) > limit {
		results = results[:limit]
	}
	var out []Memory
	for _, s := range results {
		out = append(out, s.m)
	}
	return out, nil
}

// ── internal helpers ────────────────────────────────────────────────

func (s *Store) write(m *Memory) error {
	var b strings.Builder
	if m.Description != "" || m.Name != "" {
		b.WriteString("---\n")
		b.WriteString(fmt.Sprintf("name: %s\n", m.Name))
		b.WriteString(fmt.Sprintf("description: %s\n", m.Description))
		b.WriteString("---\n\n")
	}
	b.WriteString(m.Content)
	if !strings.HasSuffix(b.String(), "\n") {
		b.WriteString("\n")
	}
	return os.WriteFile(m.FilePath, []byte(b.String()), 0o644)
}

func (s *Store) read(path string) (*Memory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	content := string(data)
	name := strings.TrimSuffix(filepath.Base(path), ".md")
	desc := ""
	if strings.HasPrefix(content, "---\n") {
		if idx := strings.Index(content[4:], "\n---\n"); idx >= 0 {
			fm := strings.TrimSpace(content[4 : idx+4])
			body := content[4+idx+5:]
			// Parse simple key: value pairs.
			for _, line := range strings.Split(fm, "\n") {
				parts := strings.SplitN(strings.TrimSpace(line), ":", 2)
				if len(parts) == 2 {
					k, v := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
					switch k {
					case "name":
						name = v
					case "description":
						desc = v
					}
				}
			}
			content = body
		}
	}
	return &Memory{
		Name:        name,
		Description: desc,
		Content:     strings.TrimSpace(content),
		FilePath:    path,
	}, nil
}

func slugify(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		if r == ' ' || r == '_' {
			return '-'
		}
		return -1
	}, strings.ToLower(s))
	if s == "" {
		s = "memory"
	}
	return s
}
