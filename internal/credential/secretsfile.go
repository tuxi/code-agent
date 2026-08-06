package credential

import (
	"encoding/json"
	"errors"
	"os"
)

// SecretsFile is the on-disk credential share between host apps and CLI tools
// (design: shared ~/.codeagent/secrets.json). The host app writes Keychain
// credentials here (0600); the CLI reads it so TUI sessions can reuse the
// provider keys the app manages, without sharing a runtime process. Shape
// mirrors the secretsJSON injection envelope:
//
//	{"llm/qwen": {"type":"bearer","secret":"..."}}
//
// Only llm namespaced entries are meaningful for provider calls; other
// namespaces (gateway etc.) are accepted but unused by the CLI resolver.
type SecretsFile struct {
	// Path is the secrets file location (default ~/.codeagent/secrets.json).
	Path string
}

// DefaultSecretsPath returns ~/.codeagent/secrets.json (empty on home error).
func DefaultSecretsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home + "/.codeagent/secrets.json"
}

// Load reads the secrets file into a StaticResolver. A missing file yields an
// empty (nil) resolver — the CLI simply falls back to env-only resolution. A
// malformed file is treated as absent (best-effort, never bricks startup).
func (s SecretsFile) Load() (Resolver, error) {
	path := s.Path
	if path == "" {
		path = DefaultSecretsPath()
	}
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var doc map[string]secretsEntry
	if err := json.Unmarshal(data, &doc); err != nil {
		// Malformed file: treat as absent so a bad write never bricks the CLI.
		return nil, nil
	}
	resolver := make(StaticResolver)
	for key, entry := range doc {
		target, ok := parseSecretTarget(key)
		if !ok || target.Namespace != "llm" {
			continue // only llm provider keys are consumed by the CLI resolver
		}
		if entry.Secret == "" {
			continue
		}
		resolver[target] = Credential{Type: Bearer, Secret: entry.Secret}
	}
	if len(resolver) == 0 {
		return nil, nil
	}
	return resolver, nil
}

// secretsEntry is one value in the secrets file (secretsJSON envelope shape).
type secretsEntry struct {
	Type   string `json:"type"`
	Secret string `json:"secret"`
}

// parseSecretTarget parses "{namespace}/{name}" into a Target.
func parseSecretTarget(key string) (Target, bool) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return Target{Namespace: key[:i], Name: key[i+1:]}, true
		}
	}
	return Target{}, false
}
