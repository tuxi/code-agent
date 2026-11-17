package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposerPathsDeploymentAndScope(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	outside := t.TempDir()
	outside, _ = filepath.EvalSymlinks(outside)
	inside := filepath.Join(root, "inside.json")
	external := filepath.Join(outside, "external.dmg")
	for _, path := range []string{inside, external} {
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	repo := newFakeConversationRepo()
	sess, err := repo.Create(context.Background(), root, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, profile, path string
		status              int
	}{
		{"daemon external file", RuntimeProfileHeadless, external, 200},
		{"daemon external directory", RuntimeProfileHeadless, outside, 200},
		{"desktop external file", RuntimeProfileFullDesktop, external, 200},
		{"sandbox external file", RuntimeProfileSandboxed, external, 403},
		{"sandbox missing external file", RuntimeProfileSandboxed, filepath.Join(outside, "missing"), 403},
		{"unknown external file", "", external, 403},
		{"sandbox absolute inside", RuntimeProfileSandboxed, inside, 200},
		{"daemon relative escape", RuntimeProfileHeadless, "../outside", 403},
		{"daemon symlink escape", RuntimeProfileHeadless, "linked/external.dmg", 403},
		{"daemon missing", RuntimeProfileHeadless, filepath.Join(outside, "missing"), 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			registerComposerPathValidationRoute(mux, repo, MuxOptions{ServerAuth: ServerAuth{Enabled: true}, RuntimeInfo: RuntimeInfo{RuntimeProfile: tc.profile}})
			body, _ := json.Marshal(map[string]any{"session_id": sess.ID, "paths": []string{tc.path}})
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("POST", "/v1/composer/paths/validate", strings.NewReader(string(body))))
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.status, w.Body.String())
			}
		})
	}
}
