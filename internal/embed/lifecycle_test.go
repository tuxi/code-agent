package embed

import (
	"context"
	"strings"
	"testing"
)

func TestStartServerRejectsSecondEmbeddedRuntime(t *testing.T) {
	dataDir := t.TempDir()
	workspace := t.TempDir()
	options := Options{
		WorkspaceDir:      workspace,
		DataDir:           dataDir,
		SettingsJSON:      `{"default_model":"","providers":{}}`,
		ServerAccessToken: strings.Repeat("a", 32),
		Sandboxed:         true,
	}

	first, err := StartServer(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Stop()

	if _, err := StartServer(context.Background(), options); err == nil {
		t.Fatal("second embedded Runtime start unexpectedly succeeded")
	} else if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second start error = %v, want already-running diagnostic", err)
	}

	if err := first.Stop(); err != nil {
		t.Fatal(err)
	}
	second, err := StartServer(context.Background(), options)
	if err != nil {
		t.Fatalf("start after Stop failed: %v", err)
	}
	defer second.Stop()
}
