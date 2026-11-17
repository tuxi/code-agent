package server

import (
	"code-agent/internal/agent"
	"code-agent/internal/session"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposerSearchDoesNotFollowLinksOrScanDependencies(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	for _, path := range []string{"README.md", "docs/usage.md", "node_modules/private.md", ".git/config"} {
		target := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	items, _, err := searchComposerFiles(context.Background(), root, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if strings.Contains(item.RelativePath, "secret") || strings.Contains(item.RelativePath, "node_modules") || strings.Contains(item.RelativePath, ".git") || strings.Contains(item.RelativePath, "linked") {
			t.Fatalf("out-of-scope entry: %+v", item)
		}
	}
	items, _, err = searchComposerFiles(context.Background(), root, "readme")
	if err != nil || len(items) != 1 || items[0].RelativePath != "README.md" {
		t.Fatalf("filtered result: %+v %v", items, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := searchComposerFiles(ctx, root, ""); err == nil {
		t.Fatal("cancelled query succeeded")
	}
}

func TestComposerDraftScopeAndAuthentication(t *testing.T) {
	root := t.TempDir()
	req := composerQuery{WorkspacePath: root, Kind: "reference"}
	if _, err := composerWorkspace(context.Background(), nil, MuxOptions{}, req); err == nil {
		t.Fatal("sandbox accepted arbitrary draft root")
	}
	resolved, err := composerWorkspace(context.Background(), nil, MuxOptions{RuntimeInfo: RuntimeInfo{RuntimeProfile: RuntimeProfileFullDesktop}}, req)
	canonical, _ := filepath.EvalSymlinks(root)
	if err != nil || resolved != canonical {
		t.Fatalf("desktop root: %q %v", resolved, err)
	}
	mux := http.NewServeMux()
	registerComposerRoutes(mux, nil, MuxOptions{})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/v1/composer/query", strings.NewReader(`{"kind":"command"}`)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated query status: %d", w.Code)
	}
}

func TestComposerQueryEchoesRevisionWithoutCreatingSession(t *testing.T) {
	repo := newFakeConversationRepo()
	mux := http.NewServeMux()
	registerComposerRoutes(mux, repo, MuxOptions{ServerAuth: ServerAuth{Enabled: true}})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/v1/composer/query", strings.NewReader(`{"kind":"command","query":"model","revision":7}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var result composerResult
	decodeResponse(t, w.Result(), &result)
	if result.Revision != 7 || len(result.Items) != 1 || result.Items[0].Action != "model" {
		t.Fatalf("result=%+v", result)
	}
	if len(repo.sessions) != 0 {
		t.Fatal("query created a session")
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/v1/composer/query", strings.NewReader(`{"kind":"reference","session_id":"missing","workspace_path":"/tmp"}`)))
	if w.Code != http.StatusForbidden {
		t.Fatalf("forged session scope status=%d", w.Code)
	}
}

type indexedComposerStore struct{ fakeEventStore }

func (s *indexedComposerStore) SessionEventsByKind(ctx context.Context, id string, kinds []string) ([]session.EventRecord, error) {
	all, err := s.Replay(ctx, id)
	if err != nil {
		return nil, err
	}
	out := []session.EventRecord{}
	for _, record := range all {
		for _, kind := range kinds {
			if record.Kind == kind {
				out = append(out, record)
				break
			}
		}
	}
	return out, nil
}

func TestComposerConversationReferencesAreBoundedAndWorkspaceScoped(t *testing.T) {
	root := t.TempDir()
	canonical, _ := filepath.EvalSymlinks(root)
	repo := newFakeConversationRepo()
	source, err := repo.Create(context.Background(), canonical, "")
	if err != nil {
		t.Fatal(err)
	}
	source.Name = "输入框讨论"
	events := &indexedComposerStore{}
	for n := 0; n < 8; n++ {
		for _, kind := range []agent.EventKind{agent.EventTurnStarted, agent.EventTurnFinished} {
			payload, _ := json.Marshal(agent.Event{Kind: kind, TurnID: fmt.Sprintf("turn-%d", n), Text: strings.Repeat("输入框上下文", 700)})
			if _, err := events.Append(context.Background(), session.EventRecord{SessionID: source.ID, Kind: string(kind), Payload: payload}); err != nil {
				t.Fatal(err)
			}
		}
	}
	opts := MuxOptions{ServerAuth: ServerAuth{Enabled: true}, RuntimeInfo: RuntimeInfo{RuntimeProfile: RuntimeProfileFullDesktop}}
	mux := http.NewServeMux()
	registerComposerReferenceRoute(mux, repo, events, opts)
	body, _ := json.Marshal(map[string]any{"workspace_path": canonical, "source_session_id": source.ID})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/v1/composer/reference", strings.NewReader(string(body))))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var snapshot composerReferenceSnapshot
	decodeResponse(t, w.Result(), &snapshot)
	if snapshot.SessionID != source.ID || snapshot.HistoryRevision != "16" || len(snapshot.ContextText) > 16<<10 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	foreign, _ := json.Marshal(map[string]any{"workspace_path": t.TempDir(), "source_session_id": source.ID})
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/v1/composer/reference", strings.NewReader(string(foreign))))
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-workspace reference status=%d", w.Code)
	}
}

func TestComposerPathValidationRejectsMissingTraversalAndEscapingLinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.pdf"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "private"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	opts := MuxOptions{ServerAuth: ServerAuth{Enabled: true}, RuntimeInfo: RuntimeInfo{RuntimeProfile: RuntimeProfileFullDesktop}}
	mux := http.NewServeMux()
	registerComposerPathValidationRoute(mux, nil, opts)
	for _, tc := range []struct {
		path   string
		status int
	}{{"file.pdf", 200}, {"missing", 409}, {"../outside", 403}, {"linked/private", 403}} {
		body, _ := json.Marshal(map[string]any{"workspace_path": root, "paths": []string{tc.path}})
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/v1/composer/paths/validate", strings.NewReader(string(body))))
		if w.Code != tc.status {
			t.Errorf("%q: status=%d want=%d body=%s", tc.path, w.Code, tc.status, w.Body.String())
		}
	}
}
