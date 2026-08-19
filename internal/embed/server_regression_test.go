package embed

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"code-agent/internal/settings"
)

// TestEmbeddedDiskProvidersLoadIntoRuntimeModels guards the disk-authoritative
// startup path: a persisted settings.json with a providers block must be
// normalized (providers → flat Models expansion + registry defaults) before the
// runtime is assembled. A regression here means StartServer runs with an empty
// model space even though the disk file declares models.
func TestEmbeddedDiskProvidersLoadIntoRuntimeModels(t *testing.T) {
	const serverAccessToken = "0123456789abcdef0123456789abcdef"
	dataDir := t.TempDir()
	workspace := t.TempDir()

	settingsPath := filepath.Join(dataDir, ".codeagent", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `{
		"default_model": "dashscope/qwen3-coder-plus",
		"credentials": {
			"llm": {"dashscope": {"source": "env", "env": "DASHSCOPE_API_KEY"}}
		},
		"providers": {
			"dashscope": {
				"enabled": true,
				"base_url": "https://dashscope.aliyuncs.com/compatible-mode/v1",
				"api": "openai",
				"credential": {"namespace": "llm", "name": "dashscope"},
				"models": [{"id": "qwen3-coder-plus", "context_window": 128000}]
			}
		},
		"web": {"fetch": {"timeout_seconds": 30}}
	}`
	if err := os.WriteFile(settingsPath, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	h, err := StartServer(context.Background(), Options{
		WorkspaceDir:      workspace,
		DataDir:           dataDir,
		Sandboxed:         true,
		ServerAccessToken: serverAccessToken,
	})
	if err != nil {
		t.Fatalf("StartServer over persisted providers: %v", err)
	}
	defer h.Stop()

	// The disk providers must be expanded into the runtime model space.
	mc, ok := h.cfg.Models["dashscope/qwen3-coder-plus"]
	if !ok {
		keys := make([]string, 0, len(h.cfg.Models))
		for k := range h.cfg.Models {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Fatalf("friendly model key missing; models=%v", keys)
	}
	if mc.BaseURL != "https://dashscope.aliyuncs.com/compatible-mode/v1" {
		t.Errorf("BaseURL = %q, want provider base_url", mc.BaseURL)
	}
	if mc.ContextWindow != 128000 {
		t.Errorf("ContextWindow = %d, want 128000", mc.ContextWindow)
	}
	if mc.Credential != (settings.CredentialRef{Namespace: "llm", Name: "dashscope"}) {
		t.Errorf("Credential = %+v, want llm/dashscope", mc.Credential)
	}

	// The serve builder resolves the declared model for turns.
	resolved, err := h.rt.Builder.ResolveModel("dashscope/qwen3-coder-plus")
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	if resolved == nil || resolved.Model != "qwen3-coder-plus" {
		t.Fatalf("ResolveModel = %+v, want wire model qwen3-coder-plus", resolved)
	}

	// Global skills/prompts dirs derive from the host data dir on the disk path too.
	if h.cfg.GlobalSkillsDir != filepath.Join(dataDir, "skills") {
		t.Errorf("GlobalSkillsDir = %q, want %q", h.cfg.GlobalSkillsDir, filepath.Join(dataDir, "skills"))
	}
}
