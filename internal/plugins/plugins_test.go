package plugins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleMarketplace = `{
  "name": "test-marketplace",
  "owner": { "name": "Test", "email": "test@example.com" },
  "metadata": { "description": "Test marketplace", "version": "1.0.0" },
  "plugins": [
    {
      "name": "test-plugin",
      "description": "A test plugin",
      "source": "./",
      "strict": false,
      "skills": ["./skills/alpha", "./skills/beta"]
    },
    {
      "name": "another-plugin",
      "description": "Another one",
      "source": "./",
      "strict": false,
      "skills": ["./skills/gamma"]
    }
  ]
}`

func TestParseMarketplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "marketplace.json")
	if err := os.WriteFile(path, []byte(sampleMarketplace), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := ParseMarketplace(path)
	if err != nil {
		t.Fatalf("ParseMarketplace: %v", err)
	}
	if m.Name != "test-marketplace" {
		t.Errorf("Name = %q, want test-marketplace", m.Name)
	}
	if len(m.Plugins) != 2 {
		t.Fatalf("Plugins = %d, want 2", len(m.Plugins))
	}
	if m.Plugins[0].Name != "test-plugin" {
		t.Errorf("Plugins[0].Name = %q", m.Plugins[0].Name)
	}
	if len(m.Plugins[0].Skills) != 2 {
		t.Errorf("test-plugin skills = %d, want 2", len(m.Plugins[0].Skills))
	}
}

func TestParseMarketplace_MissingName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "marketplace.json")
	os.WriteFile(path, []byte(`{"plugins":[]}`), 0o644)
	_, err := ParseMarketplace(path)
	if err == nil {
		t.Error("expected error for missing name")
	}
}

func TestRepoName(t *testing.T) {
	tests := []struct{ url, want string }{
		{"https://github.com/anthropics/skills.git", "skills"},
		{"git@github.com:anthropics/skills.git", "skills"},
		{"https://github.com/anthropics/skills", "skills"},
		{"https://example.com/foo/bar/cool-tools", "cool-tools"},
	}
	for _, tc := range tests {
		got := repoName(tc.url)
		if got != tc.want {
			t.Errorf("repoName(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestSkillName(t *testing.T) {
	got := skillName("my-marketplace", "doc-tools", "./skills/xlsx")
	if !strings.Contains(got, "my-marketplace") || !strings.Contains(got, "doc-tools") || !strings.Contains(got, "xlsx") {
		t.Errorf("skillName = %q, want something like my-marketplace/doc-tools/xlsx", got)
	}
}
