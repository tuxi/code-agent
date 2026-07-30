package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSharedEnrollmentWaitsForHostAcknowledgement(t *testing.T) {
	const bootstrap = "bootstrap-secret-0123456789abcdef0123456789"
	bootstrapHash := sha256.Sum256([]byte(bootstrap))
	authenticator, err := NewSharedDeviceAuthenticator(
		nil,
		hex.EncodeToString(bootstrapHash[:]),
		time.Now().Add(time.Minute),
		time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	core := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := authenticator.Handler(core)

	body, _ := json.Marshal(map[string]string{
		"bootstrap_secret": bootstrap,
		"device_name":      "Test iPhone",
		"platform":         "iOS",
	})
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, sharedPairPath, bytes.NewReader(body))
	request.RemoteAddr = "192.168.1.2:54321"
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(done)
	}()

	var enrollment SharedEnrollment
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		pending := authenticator.PendingEnrollments()
		if len(pending) == 1 {
			enrollment = pending[0]
			break
		}
		time.Sleep(time.Millisecond)
	}
	if enrollment.EnrollmentID == "" {
		t.Fatal("pair request did not create a pending enrollment")
	}
	select {
	case <-done:
		t.Fatal("pair request completed before host acknowledgement")
	default:
	}
	if enrollment.DeviceID == "" || enrollment.CredentialSHA256 == "" {
		t.Fatalf("pending enrollment omitted validation material: %+v", enrollment)
	}
	if err := authenticator.AcknowledgeEnrollment(enrollment.EnrollmentID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pair request did not complete after acknowledgement")
	}
	if response.Code != http.StatusOK {
		t.Fatalf("pair status = %d body=%s", response.Code, response.Body.String())
	}
	var pairEnvelope struct {
		Code int `json:"code"`
		Data struct {
			DeviceID   string `json:"device_id"`
			Credential string `json:"credential"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &pairEnvelope); err != nil {
		t.Fatal(err)
	}
	if pairEnvelope.Code != 0 || pairEnvelope.Data.DeviceID != enrollment.DeviceID {
		t.Fatalf("pair response = %+v", pairEnvelope)
	}
	if !strings.HasPrefix(
		pairEnvelope.Data.Credential,
		deviceCredentialPrefix+"."+enrollment.DeviceID+".",
	) {
		t.Fatal("credential does not carry its code-agent-generated device id")
	}
	digest := sha256.Sum256([]byte(pairEnvelope.Data.Credential))
	if hex.EncodeToString(digest[:]) != enrollment.CredentialSHA256 {
		t.Fatal("returned credential does not match persisted validation hash")
	}

	authorized := httptest.NewRecorder()
	authorizedRequest := httptest.NewRequest(http.MethodGet, "/v1/runtime/info", nil)
	authorizedRequest.Header.Set("Authorization", "Bearer "+pairEnvelope.Data.Credential)
	handler.ServeHTTP(authorized, authorizedRequest)
	if authorized.Code != http.StatusNoContent {
		t.Fatalf("device credential status = %d body=%s", authorized.Code, authorized.Body.String())
	}

	if err := authenticator.UpdateDevices(nil); err != nil {
		t.Fatal(err)
	}
	revoked := httptest.NewRecorder()
	handler.ServeHTTP(revoked, authorizedRequest)
	if revoked.Code != http.StatusUnauthorized {
		t.Fatalf("revoked credential status = %d", revoked.Code)
	}

	secondPair := httptest.NewRecorder()
	handler.ServeHTTP(
		secondPair,
		httptest.NewRequest(http.MethodPost, sharedPairPath, bytes.NewReader(body)),
	)
	if secondPair.Code != http.StatusUnauthorized {
		t.Fatalf("consumed bootstrap status = %d", secondPair.Code)
	}
}

func TestSharedDeviceAuthUsesDeviceIDLookupAndConstantHash(t *testing.T) {
	credential := "ca_device.dev_test-device." +
		base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 32))
	digest := sha256.Sum256([]byte(credential))
	authenticator, err := NewSharedDeviceAuthenticator(
		[]SharedDeviceRecord{{
			DeviceID:         "dev_test-device",
			CredentialSHA256: hex.EncodeToString(digest[:]),
		}},
		"",
		time.Time{},
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	handler := authenticator.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tc := range []struct {
		name       string
		credential string
		wantStatus int
	}{
		{name: "valid", credential: credential, wantStatus: http.StatusNoContent},
		{name: "wrong secret", credential: credential + "x", wantStatus: http.StatusUnauthorized},
		{
			name:       "changed locator",
			credential: strings.Replace(credential, "dev_test-device", "dev_other-device", 1),
			wantStatus: http.StatusUnauthorized,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/v1/runtime/info", nil)
			request.Header.Set("Authorization", "Bearer "+tc.credential)
			handler.ServeHTTP(recorder, request)
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestSharedEnrollmentTimeoutRevokesPendingStateAndKeepsBootstrapRetryable(t *testing.T) {
	const bootstrap = "retryable-bootstrap-secret-0123456789abcdef"
	bootstrapHash := sha256.Sum256([]byte(bootstrap))
	authenticator, err := NewSharedDeviceAuthenticator(
		nil,
		hex.EncodeToString(bootstrapHash[:]),
		time.Now().Add(time.Minute),
		25*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	handler := authenticator.Handler(http.NotFoundHandler())
	body := []byte(`{"bootstrap_secret":"` + bootstrap + `"}`)

	first := httptest.NewRecorder()
	firstRequest := httptest.NewRequest(http.MethodPost, sharedPairPath, bytes.NewReader(body))
	firstRequest.RemoteAddr = "192.168.1.20:1000"
	handler.ServeHTTP(first, firstRequest)
	if first.Code != http.StatusRequestTimeout {
		t.Fatalf("timed-out enrollment status = %d body=%s", first.Code, first.Body.String())
	}
	if pending := authenticator.PendingEnrollments(); len(pending) != 0 {
		t.Fatalf("timed-out enrollment remained pending: %+v", pending)
	}

	second := httptest.NewRecorder()
	secondRequest := httptest.NewRequest(http.MethodPost, sharedPairPath, bytes.NewReader(body))
	secondRequest.RemoteAddr = "192.168.1.20:1001"
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(second, secondRequest)
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	var enrollmentID string
	for time.Now().Before(deadline) {
		if pending := authenticator.PendingEnrollments(); len(pending) == 1 {
			enrollmentID = pending[0].EnrollmentID
			break
		}
		time.Sleep(time.Millisecond)
	}
	if enrollmentID == "" {
		t.Fatal("bootstrap was not reusable after enrollment timeout")
	}
	if err := authenticator.RejectEnrollment(enrollmentID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("rejected retry did not finish")
	}
	if second.Code != http.StatusConflict {
		t.Fatalf("rejected enrollment status = %d body=%s", second.Code, second.Body.String())
	}
}

func TestSharedPairingRateLimit(t *testing.T) {
	authenticator, err := NewSharedDeviceAuthenticator(nil, "", time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	handler := authenticator.Handler(http.NotFoundHandler())
	body := []byte(`{"bootstrap_secret":"wrong-bootstrap-secret-0123456789"}`)
	for attempt := 1; attempt <= pairRateLimitAttempts+1; attempt++ {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, sharedPairPath, bytes.NewReader(body))
		request.RemoteAddr = "192.168.1.9:1234"
		handler.ServeHTTP(recorder, request)
		want := http.StatusUnauthorized
		if attempt > pairRateLimitAttempts {
			want = http.StatusTooManyRequests
		}
		if recorder.Code != want {
			t.Fatalf("attempt %d status = %d, want %d", attempt, recorder.Code, want)
		}
	}
}

func TestSharedRequestBodyLimit(t *testing.T) {
	credential := "ca_device.dev_body-test." +
		base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x4d}, 32))
	digest := sha256.Sum256([]byte(credential))
	authenticator, err := NewSharedDeviceAuthenticator(
		[]SharedDeviceRecord{{
			DeviceID:         "dev_body-test",
			CredentialSHA256: hex.EncodeToString(digest[:]),
		}},
		"",
		time.Time{},
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	coreCalled := false
	handler := authenticator.Handler(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		coreCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/conversations",
		bytes.NewReader(bytes.Repeat([]byte{'x'}, maxSharedRequestBytes+1)),
	)
	request.Header.Set("Authorization", "Bearer "+credential)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized shared body status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if coreCalled {
		t.Fatal("oversized shared body reached the Runtime handler")
	}

	pairing := httptest.NewRecorder()
	handler.ServeHTTP(pairing, httptest.NewRequest(
		http.MethodPost,
		sharedPairPath,
		bytes.NewReader(bytes.Repeat([]byte{'x'}, maxPairRequestBytes+1)),
	))
	if pairing.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized pairing body status = %d body=%s", pairing.Code, pairing.Body.String())
	}
}

func TestSharedDeviceRevocationAllowsFiniteRequestToFinishAndRejectsNext(t *testing.T) {
	credential := "ca_device.dev_inflight." +
		base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x2a}, 32))
	digest := sha256.Sum256([]byte(credential))
	authenticator, err := NewSharedDeviceAuthenticator(
		[]SharedDeviceRecord{{
			DeviceID:         "dev_inflight",
			CredentialSHA256: hex.EncodeToString(digest[:]),
		}},
		"",
		time.Time{},
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	handler := authenticator.Handler(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/v1/runtime/info", nil)
	request.Header.Set("Authorization", "Bearer "+credential)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(recorder, request)
		close(done)
	}()
	<-entered
	if err := authenticator.UpdateDevices(nil); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("in-flight request status = %d", recorder.Code)
	}

	rejected := httptest.NewRecorder()
	handler.ServeHTTP(rejected, request)
	if rejected.Code != http.StatusUnauthorized {
		t.Fatalf("post-revocation request status = %d", rejected.Code)
	}
}
