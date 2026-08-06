package server

import (
	"encoding/json"
	"net/http"

	"code-agent/internal/credential"
)

// registerSecretsRoutes wires POST /v1/secrets (A2): the body is a
// {target: secret} map updated into the runtime's mutable injected resolver, so
// a host can push provider keys to a running daemon without restarting. After a
// successful POST, the next GET /v1/runtime/models rebuilds the catalog with
// the injected credentials and models become available. Bearer-authed by the
// global withServerAuth wrapper. A nil InjectSecrets disables the endpoint.
func registerSecretsRoutes(mux *http.ServeMux, opts MuxOptions) {
	if opts.InjectSecrets == nil {
		return
	}
	mux.HandleFunc("POST /v1/secrets", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, r, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
			return
		}
		targets := make(map[credential.Target]credential.Credential, len(body))
		for key, raw := range body {
			target, err := parseSecretsTarget(key)
			if err != nil {
				writeJSON(w, r, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			c, err := parseSecretValue(raw)
			if err != nil {
				writeJSON(w, r, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			targets[target] = c
		}
		if err := opts.InjectSecrets(targets); err != nil {
			writeJSON(w, r, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"injected": len(targets)})
	})
}

// parseSecretsTarget parses a key of the form "{namespace}/{name}" (e.g.
// "llm/qwen", "gateway/default") into a credential.Target.
func parseSecretsTarget(key string) (credential.Target, error) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return credential.Target{Namespace: key[:i], Name: key[i+1:]}, nil
		}
	}
	return credential.Target{}, &secretsKeyError{key: key}
}

type secretsKeyError struct{ key string }

func (e *secretsKeyError) Error() string {
	return "invalid secrets key " + e.key + ": expected {namespace}/{name}"
}

// parseSecretValue parses the secret value: a plain string (bearer secret) or a
// {"type":"bearer","secret":"..."} envelope (secretsJSON shape).
func parseSecretValue(raw json.RawMessage) (credential.Credential, error) {
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return credential.Credential{Type: credential.Bearer, Secret: str}, nil
	}
	var env struct {
		Type    string `json:"type"`
		Secret  string `json:"secret"`
		Expires int64  `json:"expires_at,omitempty"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return credential.Credential{}, err
	}
	return credential.Credential{Type: credential.Bearer, Secret: env.Secret}, nil
}
