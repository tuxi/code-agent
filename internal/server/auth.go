package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	"code-agent/internal/app"
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

func ValidateServerAccessToken(token string) error {
	if len([]byte(token)) < minimumServerAccessTokenBytes {
		return errors.New("server access token must be at least 32 bytes; generate it from 256 bits of randomness")
	}
	return nil
}

// ResolveExternalServerAuth loads an external Runtime's access token from its
// configured environment variable. Secrets never live in app.Config.
func ResolveExternalServerAuth(cfg app.ServerConfig) (ServerAuth, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Authentication)) {
	case "", "none":
		return ServerAuth{PublicHealthz: cfg.PublicHealthz}, nil
	case "bearer":
		envName := strings.TrimSpace(cfg.AccessTokenEnv)
		if envName == "" {
			envName = "CODEAGENT_SERVER_ACCESS_TOKEN"
		}
		token := os.Getenv(envName)
		if err := ValidateServerAccessToken(token); err != nil {
			return ServerAuth{}, fmt.Errorf("invalid Server Access Token from %s: %w", envName, err)
		}
		return ServerAuth{Enabled: true, Token: token, PublicHealthz: cfg.PublicHealthz}, nil
	default:
		return ServerAuth{}, fmt.Errorf("unsupported server.authentication %q", cfg.Authentication)
	}
}

// ValidateExternalDeployment fails closed for a directly exposed listener. A
// reverse proxy remains supported by binding CodeAgent to loopback and
// terminating TLS at the proxy.
func ValidateExternalDeployment(addr string, cfg app.ServerConfig, auth ServerAuth) error {
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

func validateTLSPair(cfg app.ServerConfig) error {
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

func withServerAuth(next http.Handler, auth ServerAuth) http.Handler {
	if !auth.Enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
