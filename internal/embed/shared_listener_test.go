package embed

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"code-agent/internal/server"
)

func TestEmbeddedSharedListenerReusesCoreRuntimeWithSeparateAuth(t *testing.T) {
	const (
		loopbackToken = "loopback-token-0123456789abcdef0123456789"
		deviceID      = "dev_test-device"
	)
	deviceCredential := "ca_device." + deviceID + "." +
		base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x3c}, 32))
	credentialHash := sha256.Sum256([]byte(deviceCredential))
	certificatePEM, privateKeyPEM := testTLSIdentity(t)

	h, err := StartServer(context.Background(), Options{
		WorkspaceDir:      t.TempDir(),
		DataDir:           t.TempDir(),
		SettingsJSON:      `{"default_model":"","providers":{},"credentials":{}}`,
		Sandboxed:         true,
		ServerAccessToken: loopbackToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop()
	if status := h.SharedListenerStatus(); status.State != SharedListenerStopped {
		t.Fatalf("initial shared listener state = %+v", status)
	}

	if err := h.StartSharedListener(SharedListenerOptions{
		CertificatePEM: certificatePEM,
		PrivateKeyPEM:  privateKeyPEM,
		Devices: []server.SharedDeviceRecord{{
			DeviceID:         deviceID,
			CredentialSHA256: hex.EncodeToString(credentialHash[:]),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	status := h.SharedListenerStatus()
	if status.State != SharedListenerRunning || status.Port == 0 ||
		(!strings.Contains(status.ListenOrigin, "0.0.0.0") &&
			!strings.Contains(status.ListenOrigin, "[::]")) {
		t.Fatalf("running shared listener status = %+v", status)
	}
	if status.ListenOrigin != h.SharedEndpoint() {
		t.Fatalf("listen diagnostics disagree: status=%q method=%q",
			status.ListenOrigin, h.SharedEndpoint())
	}
	if h.sharedSrv.ReadHeaderTimeout != sharedReadHeaderTimeout ||
		h.sharedSrv.IdleTimeout != sharedIdleTimeout ||
		h.sharedSrv.MaxHeaderBytes != sharedMaxHeaderBytes {
		t.Fatalf("shared HTTP hardening = header:%s idle:%s max:%d",
			h.sharedSrv.ReadHeaderTimeout, h.sharedSrv.IdleTimeout, h.sharedSrv.MaxHeaderBytes)
	}
	connectBaseURL := "https://127.0.0.1:" + fmt.Sprint(h.SharedPort())

	loopbackURL := strings.Replace(h.LoopbackURL(), "ws://", "http://", 1)
	loopbackInfo := runtimeInfoForTest(t, http.DefaultClient, loopbackURL, loopbackToken)
	sharedClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, // Test-only certificate; production uses AgentKit SPKI pinning.
	}}}
	sharedInfo := runtimeInfoForTest(t, sharedClient, connectBaseURL, deviceCredential)
	if loopbackInfo.ServerID == "" || sharedInfo.ServerID != loopbackInfo.ServerID {
		t.Fatalf("listeners returned different server ids: loopback=%q shared=%q",
			loopbackInfo.ServerID, sharedInfo.ServerID)
	}

	assertRuntimeAuthRejected(t, sharedClient, connectBaseURL, loopbackToken)
	assertRuntimeAuthRejected(t, http.DefaultClient, loopbackURL, deviceCredential)

	session, err := h.rt.Repo.Create(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	websocketURL := "wss://127.0.0.1:" + fmt.Sprint(h.SharedPort()) +
		"/v1/conversations/" + session.ID + "/stream"
	header := http.Header{}
	header.Set("Authorization", "Bearer "+deviceCredential)
	websocketConnection, _, err := websocket.Dial(context.Background(), websocketURL, &websocket.DialOptions{
		HTTPClient: sharedClient,
		HTTPHeader: header,
	})
	if err != nil {
		t.Fatalf("shared Agent Wire connect: %v", err)
	}
	defer websocketConnection.CloseNow()

	if err := h.UpdateSharedDevices(nil); err != nil {
		t.Fatal(err)
	}
	assertRuntimeAuthRejected(t, sharedClient, connectBaseURL, deviceCredential)
	readContext, cancelRead := context.WithTimeout(context.Background(), time.Second)
	defer cancelRead()
	var revokedReadErr error
	for revokedReadErr == nil {
		_, _, revokedReadErr = websocketConnection.Read(readContext)
	}
	if errors.Is(revokedReadErr, context.DeadlineExceeded) {
		t.Fatal("revoked device's existing Agent Wire connection remained open")
	}

	if err := h.sharedLis.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) &&
		h.SharedListenerStatus().State != SharedListenerFailed {
		time.Sleep(time.Millisecond)
	}
	failedStatus := h.SharedListenerStatus()
	if failedStatus.State != SharedListenerFailed || failedStatus.LastError == "" ||
		failedStatus.Port != 0 {
		t.Fatalf("unexpected listener stop status = %+v", failedStatus)
	}
	if err := h.StopSharedListener(); err != nil {
		t.Fatal(err)
	}
	if stoppedStatus := h.SharedListenerStatus(); stoppedStatus.State != SharedListenerStopped ||
		stoppedStatus.LastError != "" {
		t.Fatalf("intentional stop status = %+v", stoppedStatus)
	}
	if err := h.StartSharedListener(SharedListenerOptions{
		CertificatePEM: "invalid",
		PrivateKeyPEM:  "invalid",
	}); err == nil {
		t.Fatal("invalid TLS identity unexpectedly started")
	}
	if invalidStatus := h.SharedListenerStatus(); invalidStatus.State != SharedListenerFailed ||
		invalidStatus.LastError == "" {
		t.Fatalf("startup failure status = %+v", invalidStatus)
	}
}

func runtimeInfoForTest(
	t *testing.T,
	client *http.Client,
	baseURL, token string,
) server.RuntimeInfo {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, baseURL+"/v1/runtime/info", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Runtime info status = %d", response.StatusCode)
	}
	var envelope struct {
		Data server.RuntimeInfo `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data
}

func assertRuntimeAuthRejected(
	t *testing.T,
	client *http.Client,
	baseURL, token string,
) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, baseURL+"/v1/runtime/info", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cross-surface credential status = %d", response.StatusCode)
	}
}

func testTLSIdentity(t *testing.T) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Embedded Runtime Test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	privateKey := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return string(certificate), string(privateKey)
}
