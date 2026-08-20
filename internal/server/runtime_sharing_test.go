package server

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDaemonRuntimeSharingIdentityAndDurablePairing(t *testing.T) {
	dir := t.TempDir()
	sharing, err := OpenDaemonRuntimeSharing(dir, "srv_test", "Test Mac")
	if err != nil {
		t.Fatal(err)
	}
	sharing.SetCoreHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { Success(w, r, map[string]string{"ok": "true"}) }))
	if err := sharing.Start("127.0.0.1:0", "Test Mac"); err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("sandbox does not permit local sockets: %v", err)
		}
		t.Fatal(err)
	}
	inv, err := sharing.CreateInvitation(120)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base64.RawURLEncoding.DecodeString(inv.BootstrapSecret); err != nil {
		t.Fatal(err)
	}
	persisted, err := os.ReadFile(filepath.Join(dir, sharingStateFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), inv.BootstrapSecret) {
		t.Fatal("bootstrap secret was persisted")
	}

	cert, err := tls.X509KeyPair([]byte(sharing.identity.certPEM), []byte(sharing.identity.privatePEM))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	_ = parsed
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}} // pinning is the AgentKit client's responsibility.
	pairBody := `{"bootstrap_secret":"` + inv.BootstrapSecret + `","device_name":"iPhone","platform":"ios"}`
	request, _ := http.NewRequest(http.MethodPost, "https://"+sharing.Status().ListenAddress+"/v1/runtime/pair", strings.NewReader(pairBody))
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			Credential string `json:"credential"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != 0 || envelope.Data.Credential == "" {
		t.Fatalf("pair response = %+v", envelope)
	}
	persisted, err = os.ReadFile(filepath.Join(dir, sharingStateFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), envelope.Data.Credential) {
		t.Fatal("device credential was persisted")
	}
	if len(sharing.Devices()) != 1 {
		t.Fatalf("devices = %+v", sharing.Devices())
	}
	deviceID := sharing.Devices()[0].DeviceID
	if err := sharing.Revoke(deviceID); err != nil {
		t.Fatal(err)
	}
	authorized, _ := http.NewRequest(http.MethodGet, "https://"+sharing.Status().ListenAddress+"/v1/runtime/info", nil)
	authorized.Header.Set("Authorization", "Bearer "+envelope.Data.Credential)
	response, err = client.Do(authorized)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked status = %d", response.StatusCode)
	}
	if err := sharing.Stop(); err != nil {
		t.Fatal(err)
	}

	restarted, err := OpenDaemonRuntimeSharing(dir, "srv_test", "Test Mac")
	if err != nil {
		t.Fatal(err)
	}
	if restarted.identity.spki != sharing.identity.spki {
		t.Fatalf("SPKI changed after restart")
	}
}
