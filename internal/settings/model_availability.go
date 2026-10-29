package settings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"

	"code-agent/internal/credential"
	"code-agent/internal/model"
)

// ModelID retains the connection namespace even when its wire ID contains '/'.
func ModelID(alias string, mc ModelConfig) string {
	connection := mc.Catalog.ConnectionID
	if connection == "" {
		connection, _, _ = strings.Cut(alias, "/")
	}
	return connection + "/" + mc.Model
}

// ModelConfigurationID binds observations to routing/reasoning configuration,
// without including credentials or exporting private endpoint configuration.
func ModelConfigurationID(mc ModelConfig) string {
	b, _ := json.Marshal(struct{ Provider, Endpoint, Model, Connection, Credential, Effort string }{
		mc.Provider, mc.BaseURL, mc.Model, ModelID(mc.Name, mc), mc.Credential.Target().String(), mc.ReasoningEffort,
	})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ProbeModelAvailability is non-networking. Injected/keychain declarations are
// insufficient: the effective resolver must supply credentials now. Error text
// and credential values are never returned.
func ProbeModelAvailability(ctx context.Context, cfg Settings, mc ModelConfig, resolver credential.Resolver) (available bool, reason, status, source string) {
	switch mc.Provider {
	case "", "openai", "responses", "ollama":
	default:
		return false, "unsupported_provider", "missing", ""
	}
	u, err := url.Parse(mc.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false, "invalid_endpoint", "missing", ""
	}
	if mc.Credential.IsZero() {
		if model.IsLocalBaseURL(mc.BaseURL) {
			return true, "", "none", "none"
		}
		return false, "no_auth", "missing", ""
	}
	if entries := cfg.Credentials[mc.Credential.Namespace]; entries != nil && entries[mc.Credential.Name].Source == "none" {
		return true, "", "none", "none"
	}
	if resolver == nil {
		return false, "no_auth", "missing", "env"
	}
	c, err := resolver.Resolve(ctx, mc.Credential.Target())
	if err != nil || c.IsZero() || c.IsExpired() || (c.Type != credential.None && c.Secret == "") {
		return false, "no_auth", "missing", "env"
	}
	if c.Type != credential.None && c.Type != credential.Bearer && c.Type != credential.Secret {
		return false, "invalid_auth", "missing", ""
	}
	if c.Type == credential.None {
		return true, "", "none", "none"
	}
	source = "env"
	if entries := cfg.Credentials[mc.Credential.Namespace]; entries != nil {
		switch entries[mc.Credential.Name].Source {
		case "injected", "jwt":
			source = "injected"
		case "keychain":
			source = "keychain"
		}
	}
	if mc.Credential.Namespace == "gateway" || strings.Contains(c.Source, "inject") || strings.Contains(c.Source, "keychain") {
		source = "injected"
	}
	return true, "", "configured", source
}
