package skills

import (
	"os"
	"path/filepath"
	"testing"
)

// The user's real news-pulse.md file was copied from ai-berkshire/skills.
// It's a flat .md file in the skills directory (not a subdirectory).
func TestParse_NewsPulse(t *testing.T) {
	content := `---
name: news-pulse
description: 公司新闻脉搏：股价异动时快速归因。用 4 个并行 Agent 侦察公司事件/监管政策/行业对手/市场情绪，产出"事件时间线 + 异动主因判断 + 是否触发论文重审"。
---

# 公司新闻脉搏`
	skill, err := parseSkill(content)
	if err != nil {
		t.Fatalf("parseSkill failed: %v", err)
	}
	if skill.Name != "news-pulse" {
		t.Errorf("Name = %q, want news-pulse", skill.Name)
	}
}

func TestLoad_NewsPulseAsFlatFile(t *testing.T) {
	dir := t.TempDir()
	content := `---
name: news-pulse
description: 公司新闻脉搏：股价异动时快速归因。用 4 个并行 Agent 侦察公司事件/监管政策/行业对手/市场情绪，产出"事件时间线 + 异动主因判断 + 是否触发论文重审"。
---

# 公司新闻脉搏`
	if err := os.WriteFile(filepath.Join(dir, "news-pulse.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := Load("", dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1", r.Len())
	}
	s, ok := r.Get("news-pulse")
	if !ok {
		t.Fatal("news-pulse not found in registry")
	}
	t.Logf("Name=%q Desc=%q", s.Name, s.Description)
}

func TestLoad_TwoChineseFlatFiles(t *testing.T) {
	dir := t.TempDir()

	files := map[string]string{
		"news-pulse.md": `---
name: news-pulse
description: 公司新闻脉搏：股价异动时快速归因
---
body`,
		"earnings.md": `---
name: earnings-deep
description: 财报深度研读
---
body`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	r, err := Load("", dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Len() != 2 {
		t.Fatalf("Len = %d, want 2", r.Len())
	}
}
