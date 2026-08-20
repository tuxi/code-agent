package server

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	sharedPairPath         = "/v1/runtime/pair"
	maxPairRequestBytes    = 16 << 10
	maxSharedRequestBytes  = 1 << 20
	maxDeviceMetadataBytes = 128
	pairBodyReadTimeout    = 10 * time.Second
	sharedBodyReadTimeout  = 30 * time.Second
	defaultEnrollmentTTL   = 60 * time.Second
	pairRateLimitWindow    = time.Minute
	pairRateLimitAttempts  = 5
	deviceCredentialPrefix = "ca_device"
)

// SharedDeviceRecord is the non-secret validation material AgentKit persists
// after an enrollment. CredentialSHA256 is the lowercase hex SHA-256 digest of
// the complete device credential.
type SharedDeviceRecord struct {
	DeviceID         string `json:"device_id"`
	CredentialSHA256 string `json:"credential_sha256"`
}

// SharedEnrollment is the host-visible, non-secret half of an enrollment. The
// plaintext credential remains inside code-agent until the host acknowledges
// that this validation material has been durably stored.
type SharedEnrollment struct {
	EnrollmentID     string    `json:"enrollment_id"`
	DeviceID         string    `json:"device_id"`
	CredentialSHA256 string    `json:"credential_sha256"`
	DeviceName       string    `json:"device_name,omitempty"`
	Platform         string    `json:"platform,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	ExpiresAt        time.Time `json:"expires_at"`
}

type pendingSharedEnrollment struct {
	public     SharedEnrollment
	credential string
	result     chan bool
	resolved   bool
}

type pairRateState struct {
	windowStart time.Time
	attempts    int
}

// SharedDeviceAuthenticator owns only in-memory pairing state and credential
// validation hashes. It never persists or logs bootstrap/device credentials.
type SharedDeviceAuthenticator struct {
	mu sync.Mutex

	devices map[string][sha256.Size]byte
	pending map[string]*pendingSharedEnrollment

	bootstrapHash    [sha256.Size]byte
	hasBootstrap     bool
	bootstrapExpiry  time.Time
	bootstrapPending bool
	enrollmentTTL    time.Duration
	rateByAddress    map[string]pairRateState
	connections      map[string]map[*trackedDeviceConn]struct{}
	// enrollmentCommitter is used by daemon mode to durably commit a device
	// before the pairing response releases the plaintext credential.
	enrollmentCommitter func(SharedEnrollment) error
}

func (a *SharedDeviceAuthenticator) SetEnrollmentCommitter(f func(SharedEnrollment) error) {
	a.mu.Lock()
	a.enrollmentCommitter = f
	a.mu.Unlock()
}

type trackedDeviceConn struct {
	net.Conn
	owner          *SharedDeviceAuthenticator
	deviceID       string
	credentialHash [sha256.Size]byte
	once           sync.Once
}

func (c *trackedDeviceConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.owner.removeConnection(c) })
	return err
}

type sharedDeviceResponseWriter struct {
	http.ResponseWriter
	authenticator  *SharedDeviceAuthenticator
	deviceID       string
	credentialHash [sha256.Size]byte
}

func (w *sharedDeviceResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *sharedDeviceResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *sharedDeviceResponseWriter) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return http.ErrNotSupported
}

func (w *sharedDeviceResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer does not support hijacking")
	}
	conn, readWriter, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}
	tracked := &trackedDeviceConn{
		Conn:           conn,
		owner:          w.authenticator,
		deviceID:       w.deviceID,
		credentialHash: w.credentialHash,
	}
	if !w.authenticator.addConnection(tracked) {
		_ = conn.Close()
		return nil, nil, errors.New("device credential was revoked during upgrade")
	}
	return tracked, readWriter, nil
}

func NewSharedDeviceAuthenticator(
	devices []SharedDeviceRecord,
	bootstrapSHA256 string,
	bootstrapExpiresAt time.Time,
	enrollmentTTL time.Duration,
) (*SharedDeviceAuthenticator, error) {
	if enrollmentTTL <= 0 {
		enrollmentTTL = defaultEnrollmentTTL
	}
	a := &SharedDeviceAuthenticator{
		devices:       make(map[string][sha256.Size]byte),
		pending:       make(map[string]*pendingSharedEnrollment),
		enrollmentTTL: enrollmentTTL,
		rateByAddress: make(map[string]pairRateState),
		connections:   make(map[string]map[*trackedDeviceConn]struct{}),
	}
	if err := a.UpdateDevices(devices); err != nil {
		return nil, err
	}
	if bootstrapSHA256 != "" {
		if err := a.RotateBootstrap(bootstrapSHA256, bootstrapExpiresAt); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// Handler protects the shared Runtime surface. Health is intentionally public;
// pairing has its own one-time bootstrap authentication; all remaining HTTP and
// WebSocket upgrades require a per-device Bearer credential.
func (a *SharedDeviceAuthenticator) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == sharedPairPath {
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", http.MethodPost)
				ErrorSimple(w, r, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			a.handlePair(w, r)
			return
		}
		deviceID, credentialHash, ok := a.authenticate(r.Header.Get("Authorization"))
		if !ok {
			code := "runtime_auth_invalid"
			message := "Device credential is invalid"
			if r.Header.Get("Authorization") == "" {
				code = "runtime_auth_required"
				message = "Device credential is required"
			}
			a.audit(r, "authentication_failed", deviceID)
			writeRuntimeAuthError(w, r, code, message)
			return
		}
		if !limitSharedRequestBody(w, r) {
			return
		}
		next.ServeHTTP(&sharedDeviceResponseWriter{
			ResponseWriter: w,
			authenticator:  a,
			deviceID:       deviceID,
			credentialHash: credentialHash,
		}, withSharedDeviceAuthContext(r))
	})
}

func (a *SharedDeviceAuthenticator) UpdateDevices(records []SharedDeviceRecord) error {
	next := make(map[string][sha256.Size]byte, len(records))
	for _, record := range records {
		if !validDeviceID(record.DeviceID) {
			return fmt.Errorf("invalid shared device id")
		}
		digest, err := decodeSHA256(record.CredentialSHA256)
		if err != nil {
			return fmt.Errorf("invalid credential hash for device %s: %w", record.DeviceID, err)
		}
		if _, exists := next[record.DeviceID]; exists {
			return fmt.Errorf("duplicate shared device id %s", record.DeviceID)
		}
		next[record.DeviceID] = digest
	}
	a.mu.Lock()
	var closeConnections []*trackedDeviceConn
	for deviceID, active := range a.connections {
		oldDigest, existed := a.devices[deviceID]
		nextDigest, retained := next[deviceID]
		if retained && existed && oldDigest == nextDigest {
			continue
		}
		for connection := range active {
			closeConnections = append(closeConnections, connection)
		}
		delete(a.connections, deviceID)
	}
	a.devices = next
	a.mu.Unlock()
	for _, connection := range closeConnections {
		_ = connection.Close()
	}
	return nil
}

func (a *SharedDeviceAuthenticator) RotateBootstrap(hash string, expiresAt time.Time) error {
	digest, err := decodeSHA256(hash)
	if err != nil {
		return fmt.Errorf("invalid bootstrap hash: %w", err)
	}
	if expiresAt.IsZero() || !expiresAt.After(time.Now()) {
		return errors.New("bootstrap expiry must be in the future")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.bootstrapPending {
		return errors.New("cannot rotate bootstrap while an enrollment is pending")
	}
	a.bootstrapHash = digest
	a.hasBootstrap = true
	a.bootstrapExpiry = expiresAt
	return nil
}

func (a *SharedDeviceAuthenticator) PendingEnrollments() []SharedEnrollment {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make([]SharedEnrollment, 0, len(a.pending))
	for _, enrollment := range a.pending {
		if !enrollment.resolved {
			result = append(result, enrollment.public)
		}
	}
	return result
}

func (a *SharedDeviceAuthenticator) AcknowledgeEnrollment(enrollmentID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	enrollment, ok := a.pending[enrollmentID]
	if !ok || enrollment.resolved {
		return errors.New("shared enrollment not found")
	}
	digest, err := decodeSHA256(enrollment.public.CredentialSHA256)
	if err != nil {
		return errors.New("shared enrollment validation material is invalid")
	}
	a.devices[enrollment.public.DeviceID] = digest
	enrollment.resolved = true
	a.hasBootstrap = false
	a.bootstrapHash = [sha256.Size]byte{}
	a.bootstrapExpiry = time.Time{}
	a.bootstrapPending = false
	enrollment.result <- true
	a.auditLocked("enrollment_acknowledged", enrollment.public.DeviceID)
	return nil
}

func (a *SharedDeviceAuthenticator) RejectEnrollment(enrollmentID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	enrollment, ok := a.pending[enrollmentID]
	if !ok || enrollment.resolved {
		return errors.New("shared enrollment not found")
	}
	enrollment.resolved = true
	a.bootstrapPending = false
	enrollment.result <- false
	a.auditLocked("enrollment_rejected", enrollment.public.DeviceID)
	return nil
}

func (a *SharedDeviceAuthenticator) authenticate(
	header string,
) (string, [sha256.Size]byte, bool) {
	credential, ok := parseServerBearer(header)
	if !ok {
		return "", [sha256.Size]byte{}, false
	}
	deviceID, ok := deviceIDFromCredential(credential)
	if !ok {
		return "", [sha256.Size]byte{}, false
	}
	got := sha256.Sum256([]byte(credential))
	a.mu.Lock()
	want, exists := a.devices[deviceID]
	a.mu.Unlock()
	if !exists {
		return deviceID, got, false
	}
	return deviceID, got, subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

func (a *SharedDeviceAuthenticator) handlePair(w http.ResponseWriter, r *http.Request) {
	remote := remoteAddress(r.RemoteAddr)
	if !a.allowPairAttempt(remote, time.Now()) {
		a.audit(r, "pairing_rate_limited", "")
		Result(w, r, http.StatusTooManyRequests, 42900, "too many pairing attempts", map[string]string{
			"code": "pairing_rate_limited",
		})
		return
	}
	var request struct {
		BootstrapSecret string `json:"bootstrap_secret"`
		DeviceName      string `json:"device_name"`
		Platform        string `json:"platform"`
	}
	body, bodyErr := readSharedRequestBody(w, r, maxPairRequestBytes, pairBodyReadTimeout)
	if bodyErr != nil {
		status := http.StatusBadRequest
		code := CodeBadRequest
		dataCode := "pairing_request_invalid"
		if errors.Is(bodyErr, errSharedRequestTooLarge) {
			status = http.StatusRequestEntityTooLarge
			code = 41300
			dataCode = "pairing_request_too_large"
		}
		a.audit(r, "pairing_request_invalid", "")
		Result(w, r, status, code, "invalid pairing request", map[string]string{
			"code": dataCode,
		})
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil ||
		len([]byte(request.BootstrapSecret)) < minimumServerAccessTokenBytes ||
		decoder.Decode(&struct{}{}) != io.EOF {
		a.audit(r, "pairing_request_invalid", "")
		Result(w, r, http.StatusBadRequest, CodeBadRequest, "invalid pairing request", map[string]string{
			"code": "pairing_request_invalid",
		})
		return
	}
	if len([]byte(request.DeviceName)) > maxDeviceMetadataBytes ||
		len([]byte(request.Platform)) > maxDeviceMetadataBytes {
		Result(w, r, http.StatusBadRequest, CodeBadRequest, "pairing metadata is too long", map[string]string{
			"code": "pairing_metadata_invalid",
		})
		return
	}
	enrollment, err := a.beginEnrollment(request.BootstrapSecret, request.DeviceName, request.Platform)
	if err != nil {
		a.audit(r, "pairing_bootstrap_rejected", "")
		Result(w, r, http.StatusUnauthorized, CodeUnauthorized, "pairing authorization failed", map[string]string{
			"code": "pairing_bootstrap_invalid",
			"err":  err.Error(),
		})
		return
	}
	defer a.finishEnrollment(enrollment.public.EnrollmentID)
	a.audit(r, "enrollment_pending", enrollment.public.DeviceID)
	a.mu.Lock()
	committer := a.enrollmentCommitter
	a.mu.Unlock()
	if committer != nil {
		if err := committer(enrollment.public); err != nil {
			_ = a.RejectEnrollment(enrollment.public.EnrollmentID)
			Result(w, r, http.StatusInternalServerError, CodeInternal, "pairing could not be persisted", map[string]string{
				"code": "pairing_persistence_failed",
				"err":  err.Error(),
			})
			return
		}
		if err := a.AcknowledgeEnrollment(enrollment.public.EnrollmentID); err != nil {
			Result(w, r, http.StatusInternalServerError, CodeInternal, "pairing could not be activated", map[string]string{
				"code": "pairing_activation_failed",
				"err":  err.Error(),
			})
			return
		}
		writeSharedEnrollmentResult(w, r, enrollment, true)
		return
	}

	timer := time.NewTimer(time.Until(enrollment.public.ExpiresAt))
	defer timer.Stop()
	select {
	case accepted := <-enrollment.result:
		writeSharedEnrollmentResult(w, r, enrollment, accepted)
	case <-timer.C:
		if !a.cancelEnrollment(enrollment.public.EnrollmentID) {
			writeSharedEnrollmentResult(w, r, enrollment, <-enrollment.result)
			return
		}
		a.audit(r, "enrollment_timed_out", enrollment.public.DeviceID)
		Result(w, r, http.StatusRequestTimeout, 40800, "pairing confirmation timed out", map[string]string{
			"code": "pairing_confirmation_timeout",
		})
	case <-r.Context().Done():
		a.cancelEnrollment(enrollment.public.EnrollmentID)
		a.audit(r, "enrollment_client_disconnected", enrollment.public.DeviceID)
	}
}

func writeSharedEnrollmentResult(
	w http.ResponseWriter,
	r *http.Request,
	enrollment *pendingSharedEnrollment,
	accepted bool,
) {
	if !accepted {
		Result(w, r, http.StatusConflict, 40900, "pairing rejected", map[string]string{
			"code": "pairing_rejected",
		})
		return
	}
	Success(w, r, map[string]string{
		"enrollment_id": enrollment.public.EnrollmentID,
		"device_id":     enrollment.public.DeviceID,
		"credential":    enrollment.credential,
	})
}

func (a *SharedDeviceAuthenticator) beginEnrollment(
	bootstrapSecret, deviceName, platform string,
) (*pendingSharedEnrollment, error) {
	got := sha256.Sum256([]byte(bootstrapSecret))
	now := time.Now()

	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.hasBootstrap || a.bootstrapPending || !now.Before(a.bootstrapExpiry) ||
		subtle.ConstantTimeCompare(got[:], a.bootstrapHash[:]) != 1 {
		return nil, errors.New("pairing bootstrap unavailable")
	}
	deviceID, err := randomIdentifier("dev", 16)
	if err != nil {
		return nil, err
	}
	enrollmentID, err := randomIdentifier("enr", 16)
	if err != nil {
		return nil, err
	}
	secret, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	credential := deviceCredentialPrefix + "." + deviceID + "." + secret
	digest := sha256.Sum256([]byte(credential))
	expiresAt := now.Add(a.enrollmentTTL)
	if expiresAt.After(a.bootstrapExpiry) {
		expiresAt = a.bootstrapExpiry
	}
	enrollment := &pendingSharedEnrollment{
		public: SharedEnrollment{
			EnrollmentID:     enrollmentID,
			DeviceID:         deviceID,
			CredentialSHA256: hex.EncodeToString(digest[:]),
			DeviceName:       strings.TrimSpace(deviceName),
			Platform:         strings.TrimSpace(platform),
			CreatedAt:        now.UTC(),
			ExpiresAt:        expiresAt.UTC(),
		},
		credential: credential,
		result:     make(chan bool, 1),
	}
	a.pending[enrollmentID] = enrollment
	a.bootstrapPending = true
	return enrollment, nil
}

func (a *SharedDeviceAuthenticator) finishEnrollment(enrollmentID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	enrollment, ok := a.pending[enrollmentID]
	if !ok {
		return
	}
	if !enrollment.resolved {
		a.bootstrapPending = false
	}
	enrollment.credential = ""
	delete(a.pending, enrollmentID)
}

// cancelEnrollment wins only while the enrollment is still pending. If an
// acknowledge/reject already won the race, the caller must consume the queued
// result instead of reporting a timeout.
func (a *SharedDeviceAuthenticator) cancelEnrollment(enrollmentID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	enrollment, ok := a.pending[enrollmentID]
	if !ok || enrollment.resolved {
		return false
	}
	enrollment.resolved = true
	a.bootstrapPending = false
	return true
}

func (a *SharedDeviceAuthenticator) allowPairAttempt(address string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.rateByAddress[address]
	if state.windowStart.IsZero() || now.Sub(state.windowStart) >= pairRateLimitWindow {
		state = pairRateState{windowStart: now}
	}
	state.attempts++
	a.rateByAddress[address] = state
	return state.attempts <= pairRateLimitAttempts
}

var errSharedRequestTooLarge = errors.New("shared Runtime request body is too large")

func limitSharedRequestBody(w http.ResponseWriter, r *http.Request) bool {
	if r.Body == nil || r.Body == http.NoBody ||
		r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	body, err := readSharedRequestBody(w, r, maxSharedRequestBytes, sharedBodyReadTimeout)
	if err != nil {
		status := http.StatusBadRequest
		code := CodeBadRequest
		dataCode := "runtime_request_invalid"
		if errors.Is(err, errSharedRequestTooLarge) {
			status = http.StatusRequestEntityTooLarge
			code = 41300
			dataCode = "runtime_request_too_large"
		}
		Result(w, r, status, code, "invalid request body", map[string]string{
			"code": dataCode,
		})
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return true
}

func readSharedRequestBody(
	w http.ResponseWriter,
	r *http.Request,
	limit int64,
	timeout time.Duration,
) ([]byte, error) {
	if r.ContentLength > limit {
		return nil, errSharedRequestTooLarge
	}
	controller := http.NewResponseController(w)
	if timeout > 0 {
		_ = controller.SetReadDeadline(time.Now().Add(timeout))
		defer controller.SetReadDeadline(time.Time{})
	}
	reader := http.MaxBytesReader(w, r.Body, limit)
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, errSharedRequestTooLarge
		}
		return nil, err
	}
	return body, nil
}

func (a *SharedDeviceAuthenticator) addConnection(connection *trackedDeviceConn) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	current, ok := a.devices[connection.deviceID]
	if !ok || current != connection.credentialHash {
		return false
	}
	active := a.connections[connection.deviceID]
	if active == nil {
		active = make(map[*trackedDeviceConn]struct{})
		a.connections[connection.deviceID] = active
	}
	active[connection] = struct{}{}
	return true
}

func (a *SharedDeviceAuthenticator) removeConnection(connection *trackedDeviceConn) {
	a.mu.Lock()
	defer a.mu.Unlock()
	active := a.connections[connection.deviceID]
	delete(active, connection)
	if len(active) == 0 {
		delete(a.connections, connection.deviceID)
	}
}

func (a *SharedDeviceAuthenticator) CloseConnections() {
	a.mu.Lock()
	var active []*trackedDeviceConn
	for _, connections := range a.connections {
		for connection := range connections {
			active = append(active, connection)
		}
	}
	a.connections = make(map[string]map[*trackedDeviceConn]struct{})
	a.mu.Unlock()
	for _, connection := range active {
		_ = connection.Close()
	}
}

func (a *SharedDeviceAuthenticator) audit(r *http.Request, action, deviceID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.auditLocked(action, deviceID+" remote="+remoteAddress(r.RemoteAddr))
}

func (a *SharedDeviceAuthenticator) auditLocked(action, detail string) {
	if detail != "" {
		fmt.Fprintf(os.Stderr, "[shared-runtime] %s %s\n", action, detail)
		return
	}
	fmt.Fprintf(os.Stderr, "[shared-runtime] %s\n", action)
}

func decodeSHA256(value string) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	decoded, err := hex.DecodeString(strings.TrimSpace(value))
	if err != nil || len(decoded) != sha256.Size {
		return result, errors.New("expected a 64-character SHA-256 hex digest")
	}
	copy(result[:], decoded)
	return result, nil
}

func deviceIDFromCredential(credential string) (string, bool) {
	parts := strings.Split(credential, ".")
	if len(parts) != 3 || parts[0] != deviceCredentialPrefix ||
		!validDeviceID(parts[1]) {
		return "", false
	}
	secret, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(secret) != 32 ||
		base64.RawURLEncoding.EncodeToString(secret) != parts[2] {
		return "", false
	}
	return parts[1], true
}

func validDeviceID(deviceID string) bool {
	if !strings.HasPrefix(deviceID, "dev_") ||
		len(deviceID) <= len("dev_") ||
		len(deviceID) > 64 {
		return false
	}
	for _, r := range strings.TrimPrefix(deviceID, "dev_") {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') &&
			!(r >= '0' && r <= '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func randomIdentifier(prefix string, size int) (string, error) {
	value, err := randomToken(size)
	if err != nil {
		return "", err
	}
	return prefix + "_" + value, nil
}

func randomToken(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func remoteAddress(value string) string {
	host, _, err := net.SplitHostPort(value)
	if err == nil {
		return host
	}
	return value
}
