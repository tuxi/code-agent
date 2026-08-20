package server

import (
	"code-agent/internal/settings"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
)

const minimumServerAccessTokenBytes = 32

// ServerAuth configures Runtime access authentication. Token is held only in
// memory and must never be logged or exposed through Runtime metadata.
type ServerAuth struct {
	Enabled       bool
	Token         string
	PublicHealthz bool
}

type runtimeAuthError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type sharedDeviceAuthContextKey struct{}

func ValidateServerAccessToken(token string) error {
	if len([]byte(token)) < minimumServerAccessTokenBytes {
		return errors.New("server access token must be at least 32 bytes; generate it from 256 bits of randomness")
	}
	return nil
}

// ResolveExternalServerAuth loads an external Runtime's access token. A
// configured environment variable takes precedence over the local YAML value,
// allowing production deployments to override a developer's config.yaml.
// Embedded Runtime never calls this path; its host injects a temporary token.
func ResolveExternalServerAuth(cfg settings.ServerConfig) (ServerAuth, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Authentication)) {
	case "", "none":
		return ServerAuth{PublicHealthz: cfg.PublicHealthz}, nil
	case "bearer":
		envName := strings.TrimSpace(cfg.AccessTokenEnv)
		if envName == "" {
			envName = "CODEAGENT_SERVER_ACCESS_TOKEN"
		}
		token := strings.TrimSpace(os.Getenv(envName))
		source := envName
		if token == "" {
			token = strings.TrimSpace(cfg.AccessToken)
			source = "server.access_token"
		}
		if token == "" {
			return ServerAuth{}, fmt.Errorf(
				"missing Server Access Token: set %s or server.access_token", envName,
			)
		}
		if err := ValidateServerAccessToken(token); err != nil {
			return ServerAuth{}, fmt.Errorf("invalid Server Access Token from %s: %w", source, err)
		}
		return ServerAuth{Enabled: true, Token: token, PublicHealthz: cfg.PublicHealthz}, nil
	default:
		return ServerAuth{}, fmt.Errorf("unsupported server.authentication %q", cfg.Authentication)
	}
}

// ValidateExternalDeployment fails closed for a directly exposed listener. A
// reverse proxy remains supported by binding CodeAgent to loopback and
// terminating TLS at the proxy.
func ValidateExternalDeployment(addr string, cfg settings.ServerConfig, auth ServerAuth) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid server listen address %q: %w", addr, err)
	}
	if isLoopbackListenHost(host) {
		return validateTLSPair(cfg)
	}
	if !auth.Enabled {
		return errors.New("non-loopback Runtime listeners require Server Access Token authentication")
	}
	if cfg.TLSCertificate == "" || cfg.TLSPrivateKey == "" {
		return errors.New("non-loopback Runtime listeners require TLS certificate and private key")
	}
	return validateTLSPair(cfg)
}

func validateTLSPair(cfg settings.ServerConfig) error {
	if (cfg.TLSCertificate == "") != (cfg.TLSPrivateKey == "") {
		return errors.New("server TLS certificate and private key must be configured together")
	}
	return nil
}

func isLoopbackListenHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// WithServerAuth applies the Runtime's single-token authentication policy to a
// handler. Embedded hosts use it to put a private loopback authentication layer
// around the same core handler that a shared listener exposes with device auth.
func WithServerAuth(next http.Handler, auth ServerAuth) http.Handler {
	if !auth.Enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, _ := r.Context().Value(sharedDeviceAuthContextKey{}).(bool); ok {
			next.ServeHTTP(w, r)
			return
		}
		if auth.PublicHealthz && r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		raw := r.Header.Get("Authorization")
		if raw == "" {
			writeRuntimeAuthError(w, r, "runtime_auth_required", "Server access token is required")
			return
		}
		token, ok := parseServerBearer(raw)
		if !ok || !constantTimeTokenEqual(token, auth.Token) {
			writeRuntimeAuthError(w, r, "runtime_auth_invalid", "Server access token is invalid")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func withSharedDeviceAuthContext(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), sharedDeviceAuthContextKey{}, true))
}

func withServerAuth(next http.Handler, auth ServerAuth) http.Handler {
	return WithServerAuth(next, auth)
}

func parseServerBearer(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	return token, token != ""
}

func constantTimeTokenEqual(got, want string) bool {
	gotHash := sha256.Sum256([]byte(got))
	wantHash := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(gotHash[:], wantHash[:]) == 1
}

func writeRuntimeAuthError(w http.ResponseWriter, r *http.Request, code, message string) {
	Result(w, r, http.StatusUnauthorized, CodeUnauthorized, "unauthorized", runtimeAuthError{
		Code: code, Message: message,
	})
}
