package server

// DaemonRuntimeSharing owns the daemon-side shared HTTPS listener and its
// durable identity/device registry. It is deliberately independent of the
// embedded runtime API; the daemon is the sole owner of these secrets.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	sharingStateFile = "runtime-sharing.json"
	sharingMinTTL    = 30 * time.Second
	sharingMaxTTL    = 300 * time.Second
)

type RuntimeSharedListenerStatus struct {
	State            string `json:"state"`
	ListenAddress    string `json:"listen_address,omitempty"`
	ListenOrigin     string `json:"listen_origin,omitempty"`
	Port             int    `json:"port"`
	StartedAt        string `json:"started_at,omitempty"`
	StoppedAt        string `json:"stopped_at,omitempty"`
	LastTransitionAt string `json:"last_transition_at,omitempty"`
	LastError        string `json:"last_error,omitempty"`
}

type RuntimeSharedDevice struct {
	DeviceID         string  `json:"device_id"`
	CredentialSHA256 string  `json:"credential_sha256"`
	DisplayName      string  `json:"display_name,omitempty"`
	Platform         string  `json:"platform,omitempty"`
	ClientDeviceID   string  `json:"client_device_id,omitempty"`
	OSVersion        string  `json:"os_version,omitempty"`
	AppVersion       string  `json:"app_version,omitempty"`
	PairedAt         string  `json:"paired_at"`
	RevokedAt        *string `json:"revoked_at"`
}

type RuntimePairingInvitation struct {
	Version            int    `json:"version"`
	ServerID           string `json:"server_id"`
	ServerDisplayName  string `json:"server_display_name"`
	ServiceType        string `json:"service_type"`
	ServiceName        string `json:"service_name"`
	FallbackHost       string `json:"fallback_host"`
	Port               int    `json:"port"`
	BootstrapSecret    string `json:"bootstrap_secret"`
	BootstrapExpiresAt string `json:"bootstrap_expires_at"`
	SPKISHA256         string `json:"spki_sha256"`
}

type sharingDisk struct {
	ServerID      string                `json:"server_id"`
	DisplayName   string                `json:"display_name"`
	Enabled       bool                  `json:"enabled"`
	ListenAddress string                `json:"listen_address"`
	Identity      identityDisk          `json:"identity"`
	Devices       []RuntimeSharedDevice `json:"devices"`
}
type identityDisk struct{ PrivateKeyPEM, CertificatePEM, SPKISHA256, CreatedAt string }

type DaemonRuntimeSharing struct {
	mu                          sync.Mutex
	path, serverID, displayName string
	disk                        sharingDisk
	identity                    tlsIdentity
	server                      *http.Server
	listener                    net.Listener
	auth                        *SharedDeviceAuthenticator
	status                      RuntimeSharedListenerStatus
	core                        http.Handler
	bonjour                     *mdnsAdvertiser
}
type tlsIdentity struct {
	privatePEM, certPEM, spki string
	created                   string
}

func OpenDaemonRuntimeSharing(dir, serverID, displayName string) (*DaemonRuntimeSharing, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &DaemonRuntimeSharing{path: filepath.Join(dir, sharingStateFile), serverID: serverID, displayName: displayName}
	data, err := os.ReadFile(s.path)
	if err == nil {
		if err := json.Unmarshal(data, &s.disk); err != nil {
			return nil, fmt.Errorf("decode sharing state: %w", err)
		}
		if s.disk.ServerID != "" && s.disk.ServerID != serverID {
			return nil, errors.New("sharing state belongs to another server")
		}
		s.displayName = s.disk.DisplayName
		if s.disk.Identity.PrivateKeyPEM == "" || s.disk.Identity.CertificatePEM == "" {
			return nil, errors.New("sharing identity is incomplete")
		}
		s.identity = tlsIdentity{s.disk.Identity.PrivateKeyPEM, s.disk.Identity.CertificatePEM, s.disk.Identity.SPKISHA256, s.disk.Identity.CreatedAt}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else {
		identity, err := newTLSIdentity()
		if err != nil {
			return nil, err
		}
		s.identity = identity
		s.disk = sharingDisk{ServerID: serverID, DisplayName: displayName, ListenAddress: "0.0.0.0:0", Identity: identityDisk{identity.privatePEM, identity.certPEM, identity.spki, identity.created}}
		if err := s.persistLocked(); err != nil {
			return nil, err
		}
	}
	if s.displayName == "" {
		s.displayName = "CodeAgent Runtime"
	}
	s.disk.ServerID, s.disk.DisplayName = serverID, s.displayName
	return s, nil
}

func (s *DaemonRuntimeSharing) SetCoreHandler(h http.Handler) {
	s.mu.Lock()
	s.core = h
	restore := s.disk.Enabled
	addr, displayName := s.disk.ListenAddress, s.disk.DisplayName
	s.mu.Unlock()
	if restore {
		if err := s.Start(addr, displayName); err != nil {
			s.mu.Lock()
			now := time.Now().UTC().Format(time.RFC3339Nano)
			s.status = RuntimeSharedListenerStatus{State: "failed", LastTransitionAt: now, LastError: safeSharingError(err)}
			_ = s.persistLocked()
			s.mu.Unlock()
		}
	}
}

func (s *DaemonRuntimeSharing) Status() RuntimeSharedListenerStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *DaemonRuntimeSharing) Start(addr, displayName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.server != nil {
		return nil
	}
	if s.core == nil {
		return errors.New("sharing core handler is not configured")
	}
	if strings.TrimSpace(displayName) != "" {
		s.displayName = strings.TrimSpace(displayName)
	}
	if addr == "" {
		addr = s.disk.ListenAddress
	}
	if addr == "" {
		addr = "0.0.0.0:0"
	}
	cert, err := tls.X509KeyPair([]byte(s.identity.certPEM), []byte(s.identity.privatePEM))
	if err != nil {
		return err
	}
	raw, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	auth, err := NewSharedDeviceAuthenticator(s.activeRecordsLocked(), "", time.Time{}, 0)
	if err != nil {
		_ = raw.Close()
		return err
	}
	auth.SetEnrollmentCommitter(func(enrollment SharedEnrollment) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		// Upsert: if a device with this auth-table device_id already exists
		// (same physical client re-paired), refresh its credential and metadata
		// in place instead of rejecting it or accumulating a duplicate row.
		for i := range s.disk.Devices {
			if s.disk.Devices[i].DeviceID == enrollment.DeviceID {
				s.disk.Devices[i].CredentialSHA256 = enrollment.CredentialSHA256
				s.disk.Devices[i].DisplayName = enrollment.DeviceName
				s.disk.Devices[i].Platform = enrollment.Platform
				s.disk.Devices[i].ClientDeviceID = enrollment.ClientDeviceID
				s.disk.Devices[i].OSVersion = enrollment.OSVersion
				s.disk.Devices[i].AppVersion = enrollment.AppVersion
				s.disk.Devices[i].PairedAt = enrollment.CreatedAt.Format(time.RFC3339Nano)
				s.disk.Devices[i].RevokedAt = nil
				return s.persistLocked()
			}
		}
		s.disk.Devices = append(s.disk.Devices, RuntimeSharedDevice{DeviceID: enrollment.DeviceID, CredentialSHA256: enrollment.CredentialSHA256, DisplayName: enrollment.DeviceName, Platform: enrollment.Platform, ClientDeviceID: enrollment.ClientDeviceID, OSVersion: enrollment.OSVersion, AppVersion: enrollment.AppVersion, PairedAt: enrollment.CreatedAt.Format(time.RFC3339Nano)})
		return s.persistLocked()
	})
	sharedCore := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/runtime/sharing/") {
			ErrorSimple(w, r, http.StatusNotFound, "not found")
			return
		}
		s.core.ServeHTTP(w, r)
	})
	now := time.Now().UTC().Format(time.RFC3339Nano)
	s.listener, s.auth, s.server = raw, auth, &http.Server{Handler: auth.Handler(sharedCore), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
	port := raw.Addr().(*net.TCPAddr).Port
	s.disk.Enabled, s.disk.ListenAddress, s.disk.DisplayName = true, addr, s.displayName
	s.status = RuntimeSharedListenerStatus{State: "running", ListenAddress: raw.Addr().String(), ListenOrigin: "https://" + raw.Addr().String(), Port: port, StartedAt: now, LastTransitionAt: now}
	if err := s.persistLocked(); err != nil {
		_ = raw.Close()
		s.server, s.listener, s.auth = nil, nil, nil
		return err
	}
	advertiser, err := newMDNSAdvertiser(s.serverID, s.displayName, port)
	if err != nil {
		_ = raw.Close()
		s.server, s.listener, s.auth = nil, nil, nil
		s.disk.Enabled = false
		s.status = RuntimeSharedListenerStatus{State: "failed", LastTransitionAt: now, LastError: safeSharingError(err)}
		_ = s.persistLocked()
		return err
	}
	s.bonjour = advertiser
	go func(srv *http.Server, l net.Listener) {
		if err := srv.Serve(tls.NewListener(l, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.mu.Lock()
			s.status.State = "failed"
			s.status.LastError = err.Error()
			s.status.LastTransitionAt = time.Now().UTC().Format(time.RFC3339Nano)
			s.mu.Unlock()
		}
	}(s.server, raw)
	return nil
}

func (s *DaemonRuntimeSharing) Stop() error {
	s.mu.Lock()
	if s.server == nil {
		s.mu.Unlock()
		return nil
	}
	srv := s.server
	bonjour := s.bonjour
	auth := s.auth
	s.server, s.listener, s.auth, s.bonjour = nil, nil, nil, nil
	now := time.Now().UTC().Format(time.RFC3339Nano)
	s.disk.Enabled = false
	s.status = RuntimeSharedListenerStatus{State: "stopped", StoppedAt: now, LastTransitionAt: now}
	err := s.persistLocked()
	s.mu.Unlock()
	if bonjour != nil {
		bonjour.Close()
	}
	if auth != nil {
		auth.CloseConnections()
	}
	return errors.Join(srv.Close(), err)
}

func (s *DaemonRuntimeSharing) CreateInvitation(ttl int) (RuntimePairingInvitation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.State != "running" {
		return RuntimePairingInvitation{}, errors.New("sharing listener is not running")
	}
	if ttl < int(sharingMinTTL/time.Second) {
		ttl = int(sharingMinTTL / time.Second)
	}
	if ttl > int(sharingMaxTTL/time.Second) {
		ttl = int(sharingMaxTTL / time.Second)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return RuntimePairingInvitation{}, err
	}
	sum := sha256.Sum256(secret)
	if s.auth == nil {
		return RuntimePairingInvitation{}, errors.New("sharing authenticator is not running")
	}
	expiresAt := time.Now().Add(time.Duration(ttl) * time.Second)
	if err := s.auth.RotateBootstrap(hex.EncodeToString(sum[:]), expiresAt); err != nil {
		return RuntimePairingInvitation{}, err
	}
	serverSuffix := s.serverID
	if len(serverSuffix) > 6 {
		serverSuffix = serverSuffix[len(serverSuffix)-6:]
	}
	host, _ := os.Hostname()
	if host == "" || net.ParseIP(host) != nil {
		host = "localhost"
	}
	return RuntimePairingInvitation{Version: 1, ServerID: s.serverID, ServerDisplayName: s.displayName, ServiceType: "_talkify-agent._tcp.", ServiceName: s.displayName + " " + serverSuffix, FallbackHost: host, Port: s.status.Port, BootstrapSecret: base64.RawURLEncoding.EncodeToString(secret), BootstrapExpiresAt: expiresAt.UTC().Format(time.RFC3339Nano), SPKISHA256: s.identity.spki}, nil
}

func (s *DaemonRuntimeSharing) Devices() []RuntimeSharedDevice {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RuntimeSharedDevice(nil), s.disk.Devices...)
}
func (s *DaemonRuntimeSharing) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.disk.Devices {
		if s.disk.Devices[i].DeviceID == id && s.disk.Devices[i].RevokedAt == nil {
			now := time.Now().UTC().Format(time.RFC3339Nano)
			s.disk.Devices[i].RevokedAt = &now
			if err := s.persistLocked(); err != nil {
				return err
			}
			if s.auth != nil {
				if err := s.auth.UpdateDevices(s.activeRecordsLocked()); err != nil {
					return err
				}
			}
			return nil
		}
	}
	return errors.New("shared device not found")
}

func (s *DaemonRuntimeSharing) activeRecordsLocked() []SharedDeviceRecord {
	out := make([]SharedDeviceRecord, 0)
	for _, d := range s.disk.Devices {
		if d.RevokedAt == nil {
			out = append(out, SharedDeviceRecord{DeviceID: d.DeviceID, CredentialSHA256: d.CredentialSHA256, ClientDeviceID: d.ClientDeviceID})
		}
	}
	return out
}
func (s *DaemonRuntimeSharing) persistLocked() error {
	b, err := json.MarshalIndent(s.disk, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), "sharing-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(b)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, s.path)
}

func safeSharingError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.NewReplacer("\r", " ", "\n", " ").Replace(err.Error())
	if len(message) > 512 {
		message = message[:512]
	}
	return message
}

func newTLSIdentity() (tlsIdentity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tlsIdentity{}, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tlsIdentity{}, err
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return tlsIdentity{}, err
	}
	tmpl := x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "CodeAgent Runtime Sharing"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"localhost"}}
	certDER, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tlsIdentity{}, err
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return tlsIdentity{}, err
	}
	spki := sha256.Sum256(pub)
	return tlsIdentity{string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})), base64.RawStdEncoding.EncodeToString(spki[:]), time.Now().UTC().Format(time.RFC3339Nano)}, nil
}
