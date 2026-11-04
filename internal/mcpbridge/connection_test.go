//go:build unix

package mcpbridge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHTTPSAndWSSPreserveCertificateVerification(t *testing.T) {
	root := testRepo(t)
	f := &fakeRuntime{root: root, events: []Event{}}
	srv := httptest.NewTLSServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	c, err := New(context.Background(), srv.URL, "test-token", root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Create(context.Background(), "untrusted"); err == nil {
		t.Fatal("untrusted TLS certificate accepted")
	}
	// Trust only this test CA; never turn off chain or hostname verification.
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	c.http.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	ref, err := c.Create(context.Background(), "trusted")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := c.Send(context.Background(), ref.ID, "wss-turn", "local fixture", "")
	if err != nil || !receipt.Accepted {
		t.Fatalf("WSS admission failed: %v", err)
	}
	if _, err = c.Read(context.Background(), ref.ID, receipt.TurnID, 0); err != nil {
		t.Fatal(err)
	}
}

func writeGrant(t *testing.T, endpoint string) (string, connectionManifest) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	m := connectionManifest{SchemaVersion: 1, Endpoint: endpoint, ServerID: "server-1", StoreID: "store-1", InstanceID: "instance-1", DaemonPID: os.Getpid(), ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), AccessToken: strings.Repeat("fixture-token", 4)}
	path := filepath.Join(dir, "connection.json")
	saveGrant(t, path, m)
	return path, m
}

func saveGrant(t *testing.T, path string, m connectionManifest) {
	t.Helper()
	b, err := json.Marshal(m)
	if err == nil {
		err = os.WriteFile(path, b, 0600)
	}
	if err != nil {
		t.Fatal("cannot write fixture grant")
	}
}

func TestManifestIdentityAndRevocation(t *testing.T) {
	root := testRepo(t)
	token := strings.Repeat("fixture-token", 4)
	serverID, storeID := "server-1", "store-1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path != "/v1/runtime/info" && r.URL.Path != "/v1/tasks/capabilities" {
			t.Error("startup attempted execution or model discovery")
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]string{"server_id": serverID, "store_id": storeID}})
	}))
	defer srv.Close()
	path, m := writeGrant(t, srv.URL)
	c, info, err := NewFromManifest(context.Background(), path, root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	encoded, _ := json.Marshal(info)
	if !info.IdentityVerified || info.StoreID != m.StoreID || strings.Contains(string(encoded), token) {
		t.Fatal("unsafe or unverified public identity")
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if c.checkBinding() == nil {
		t.Fatal("revoked grant accepted")
	}
	select {
	case <-c.ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("revocation did not close bridge")
	}
	saveGrant(t, path, m)
	if c.checkBinding() == nil {
		t.Fatal("closed bridge resumed after grant restoration")
	}
	serverID = "different"
	if c2, _, err := NewFromManifest(context.Background(), path, root); err == nil {
		c2.Close()
		t.Fatal("wrong runtime accepted")
	}
	serverID, storeID = m.ServerID, "different"
	if c2, _, err := NewFromManifest(context.Background(), path, root); err == nil {
		c2.Close()
		t.Fatal("wrong store accepted")
	}
}

func TestManifestRejectsUnsafeFilesAndExpiredOrReplacedGrants(t *testing.T) {
	path, m := writeGrant(t, "https://127.0.0.1:9876")
	if _, _, err := loadConnection(path); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0644, 0400, 0660} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadConnection(path); err == nil {
			t.Fatal("unsafe file mode accepted")
		}
	}
	_ = os.Chmod(path, 0600)
	_ = os.Chmod(filepath.Dir(path), 0755)
	if _, _, err := loadConnection(path); err == nil {
		t.Fatal("public directory accepted")
	}
	_ = os.Chmod(filepath.Dir(path), 0700)
	link := filepath.Join(filepath.Dir(path), "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadConnection(link); err == nil {
		t.Fatal("symlink accepted")
	}
	for _, change := range []func(*connectionManifest){
		func(v *connectionManifest) { v.ExpiresAt = time.Now().Add(-time.Second).UTC().Format(time.RFC3339) },
		func(v *connectionManifest) { v.ExpiresAt = time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339) },
		func(v *connectionManifest) { v.SchemaVersion = 2 },
		func(v *connectionManifest) { v.Endpoint = "https://example.com:443" },
		func(v *connectionManifest) { v.AccessToken = "missing" },
		func(v *connectionManifest) { v.DaemonPID = 0 },
	} {
		bad := m
		change(&bad)
		saveGrant(t, path, bad)
		if _, _, err := loadConnection(path); err == nil {
			t.Fatal("invalid manifest accepted")
		}
	}
	saveGrant(t, path, m)
	_, digest, _ := loadConnection(path)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Client{ctx: ctx, binding: &connectionBinding{path: path, digest: digest}}
	m.InstanceID = "replacement"
	saveGrant(t, path, m)
	if c.checkBinding() == nil {
		t.Fatal("replacement grant followed silently")
	}
	if err := os.WriteFile(path, []byte(`{"access_token":"SECRET-MUST-NOT-LEAK",broken`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadConnection(path); err == nil || strings.Contains(err.Error(), "SECRET-MUST-NOT-LEAK") {
		t.Fatal("manifest decoding leaked input")
	}
}

func TestLocalMCPConnectionSharedFixture(t *testing.T) {
	b, err := os.ReadFile("../../docs/contracts/local-mcp-connection-example.json")
	if err != nil {
		t.Fatal(err)
	}
	var m connectionManifest
	if err = json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.SchemaVersion != 1 || m.ServerID != "example-server" || m.StoreID != "example-store" || m.DaemonPID != 4242 || m.InstanceID != "example-instance" {
		t.Fatal("Go/Swift contract mismatch")
	}
	if _, err = time.Parse(time.RFC3339, m.ExpiresAt); err != nil {
		t.Fatal(err)
	}
}

func TestGrantDiagnosticsDistinguishExpiryStopAndReplacement(t *testing.T) {
	path, original := writeGrant(t, "http://127.0.0.1:9876")
	for _, tc := range []struct {
		code string
		edit func(*connectionManifest)
	}{
		{"grant_expired", func(m *connectionManifest) { m.ExpiresAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339) }},
		{"daemon_stopped", func(m *connectionManifest) { m.DaemonPID = 1 << 30 }},
		{"grant_invalid", func(m *connectionManifest) { m.AccessToken = "PRIVATE" }},
	} {
		t.Run(tc.code, func(t *testing.T) {
			m := original
			tc.edit(&m)
			saveGrant(t, path, m)
			_, _, err := loadConnection(path)
			requireDiagnostic(t, err, tc.code)
			if strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), original.AccessToken) {
				t.Fatal("credential reflected", err)
			}
		})
	}
	saveGrant(t, path, original)
	_, digest, err := loadConnection(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Client{ctx: ctx, binding: &connectionBinding{path: path, digest: digest}}
	replaced := original
	replaced.InstanceID = "replacement"
	saveGrant(t, path, replaced)
	requireDiagnostic(t, c.checkBinding(), "grant_replaced")
	saveGrant(t, path, original)
	cancel()
	requireDiagnostic(t, c.checkBinding(), "connection_closed")
	// Startup wrappers retain typed diagnostics for CLI/log consumers.
	expired := original
	expired.ExpiresAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	saveGrant(t, path, expired)
	_, _, err = NewFromManifest(context.Background(), path, "/not-inspected")
	requireDiagnostic(t, err, "grant_expired")
}
