package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContextCatalogEmptyRoundTripAndBound(t *testing.T) {
	sess := &Session{}
	source := RegisterContextSource(sess, "system_prompt", "base", "", "original")
	b, err := json.Marshal(sess.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err = json.Unmarshal(b, &metadata); err != nil {
		t.Fatal(err)
	}
	loaded := &Session{Metadata: metadata}
	if got := ContextCatalog(loaded)[source.ID]; got.Content != "original" || ContentHash(got.Content) != got.SHA256 {
		t.Fatal("version lost in checkpoint")
	}
	RegisterContextSource(loaded, "system_prompt", "oversized", "", strings.Repeat("a", 4*1024*1024+1))
	if len(ContextCatalog(loaded)) != 0 || loaded.Metadata["context_source_catalog_evicted_v1"] != true {
		t.Fatal("catalog was not bounded")
	}
	if len(SkillContextCalls(&Session{})) != 0 {
		t.Fatal("empty skill ledger")
	}
}
func TestContextMemoryMatchesActualInjectionAndDistinctIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".codeagent"), 0700); err != nil {
		t.Fatal(err)
	}
	text := "same instructions"
	for _, path := range []string{filepath.Join(root, "AGENTS.md"), filepath.Join(os.Getenv("HOME"), ".codeagent", "AGENTS.md")} {
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sess, err := NewBuilder(root).WithSystemPrompt("base also says same instructions").Build()
	if err != nil {
		t.Fatal(err)
	}
	catalog := ContextCatalog(sess)
	matched, remaining := MatchContextSources(sess.Messages[0].Content, catalog, map[string]bool{"memory_files": true})
	if len(matched) != 2 || matched[0].ID == matched[1].ID || !strings.Contains(remaining, "base also says same instructions") {
		t.Fatalf("identity or match boundary lost: %+v %s", matched, remaining)
	}
	for _, source := range matched {
		if !strings.Contains(source.Prefix, "project_instructions") {
			t.Fatal("missing injection anchor")
		}
	}
	matched, _ = MatchContextSources("base also says same instructions", catalog, map[string]bool{"memory_files": true})
	if len(matched) != 0 {
		t.Fatal("removed instructions still attributed through unrelated prose")
	}
}
func TestContextMatchingDoesNotCreateNewSubstringAcrossRemovedSpan(t *testing.T) {
	sess := &Session{}
	a := RegisterContextSource(sess, "memory_files", "a", "", "X")
	b := RegisterContextSource(sess, "memory_files", "b", "", "ab")
	matched, _ := MatchContextSources("aXb", map[string]ContextSource{a.ID: a, b.ID: b}, map[string]bool{"memory_files": true})
	if len(matched) != 1 || matched[0].ID != a.ID {
		t.Fatal(fmt.Sprint(matched))
	}
}
