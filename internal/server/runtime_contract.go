package server

import (
	"code-agent/internal/settings"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"code-agent/internal/buildinfo"
	"code-agent/internal/credential"
	"code-agent/internal/model"
	runtimepkg "code-agent/internal/runtime"
)

const (
	RuntimeProfileSandboxed   = "sandboxed"
	RuntimeProfileFullDesktop = "full_desktop"
	RuntimeProfileHeadless    = "headless"
)

type AgentWireProtocolInfo struct {
	Major    int    `json:"major"`
	Revision string `json:"revision"`
}

type RuntimeInfo struct {
	Schema            string                `json:"schema"`
	ServerID          string                `json:"server_id"`
	DisplayName       string                `json:"display_name"`
	Product           string                `json:"product"`
	RuntimeVersion    string                `json:"runtime_version"`
	AgentWireProtocol AgentWireProtocolInfo `json:"agent_wire_protocol"`
	RuntimeProfile    string                `json:"runtime_profile"`
}

type RuntimeModelCatalog struct {
	DefaultModel              string                   `json:"default_model"`
	VerificationScope         string                   `json:"verification_scope"`
	VerificationMaxAgeSeconds int64                    `json:"verification_max_age_seconds"`
	Schema                    string                   `json:"schema"`
	Revision                  int64                    `json:"revision"`
	DefaultRuntimeAlias       string                   `json:"default_runtime_alias"`
	Connections               []RuntimeModelConnection `json:"connections"`
}

type RuntimeModelConnection struct {
	ID            string                   `json:"id"`
	ProviderID    string                   `json:"provider_id"`
	DisplayName   string                   `json:"display_name"`
	BillingSource string                   `json:"billing_source"`
	Models        []RuntimeModelDescriptor `json:"models"`
	// Credential is the per-connection credential status/source (wire v2,
	// design-runtime-models-wire-v2 §4). Optional: old clients ignore it; a nil
	// value means the connection has no configured credential.
	Credential *RuntimeConnectionCredential `json:"credential,omitempty"`
}

// RuntimeConnectionCredential is the non-secret declaration of how a
// connection authenticates, surfaced for host UIs. It never carries the secret
// value itself.
type RuntimeConnectionCredential struct {
	Status string `json:"status"` // "configured" | "missing" | "none"
	Source string `json:"source"` // "env" | "injected" | "keychain" | "none"
}

type RuntimeModelDescriptor struct {
	ID                string     `json:"id"`
	Configured        bool       `json:"configured"`
	Verified          bool       `json:"verified"`
	VerifiedAt        *time.Time `json:"verified_at,omitempty"`
	ActualModel       string     `json:"actual_model,omitempty"`
	ConfigurationID   string     `json:"configuration_id"`
	RuntimeAlias      string     `json:"runtime_alias"`
	WireModelID       string     `json:"wire_model_id"`
	DisplayName       string     `json:"display_name"`
	ContextWindow     int        `json:"context_window,omitempty"`
	SupportsTools     bool       `json:"supports_tools"`
	SupportsReasoning bool       `json:"supports_reasoning"`
	InputModalities   []string   `json:"input_modalities"`
	Available         bool       `json:"available"`
	// UnavailableReason explains why Available is false (wire v2). Omitted when
	// the model is available.
	UnavailableReason string `json:"unavailable_reason,omitempty"`

	// ReasoningEffort is the model's configured default thinking budget
	// ("low"|"medium"|"high"|"x-high"|"max"; omitted = auto/provider default).
	// The host pre-selects it in the effort picker; the same value is forwarded
	// on every model request until overridden.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// SupportedReasoningEfforts lists the effort levels this model accepts
	// (empty = toggle only, no standardized effort control).
	SupportedReasoningEfforts []string `json:"supported_reasoning_efforts,omitempty"`
	// CanDisableReasoning reports whether reasoning may be turned off entirely
	// (false = reasoner-only; nil/true = the off position is allowed).
	CanDisableReasoning *bool `json:"can_disable_reasoning,omitempty"`
}

// BuildRuntimeModelCatalog rebuilds the runtime model catalog with the given
// injected resolver (A2). It is the live-rebuild entry used by codeagentd's
// GET /v1/runtime/models after a POST /v1/secrets — the startup snapshot
// (BuildRuntimeContract) and the live view diverge only in credential
// availability.
func BuildRuntimeModelCatalog(cfg settings.Settings, injected credential.Resolver) RuntimeModelCatalog {
	return buildRuntimeModelCatalog(cfg, injected)
}

// BuildRuntimeContract constructs allowlisted public DTOs and binds them to the
// durable identity of the current session data source.
func BuildRuntimeContract(cfg settings.Settings, root, displayName, profile string, injected credential.Resolver) (RuntimeInfo, RuntimeModelCatalog, error) {
	catalog := buildRuntimeModelCatalog(cfg, injected)
	fingerprint, err := catalogConfigurationFingerprint(catalog)
	if err != nil {
		return RuntimeInfo{}, RuntimeModelCatalog{}, err
	}
	state, err := runtimepkg.LoadOrCreateServerState(root, fingerprint)
	if err != nil {
		return RuntimeInfo{}, RuntimeModelCatalog{}, err
	}
	catalog.Revision = state.CatalogRevision
	if strings.TrimSpace(displayName) == "" {
		displayName, _ = os.Hostname()
	}
	if strings.TrimSpace(displayName) == "" {
		displayName = "CodeAgent Runtime"
	}
	return RuntimeInfo{
		Schema:         "runtime-info/v1",
		ServerID:       state.ServerID,
		DisplayName:    displayName,
		Product:        buildinfo.Product,
		RuntimeVersion: buildinfo.Version,
		AgentWireProtocol: AgentWireProtocolInfo{
			Major: buildinfo.AgentWireMajor, Revision: buildinfo.AgentWireRevision,
		},
		RuntimeProfile: profile,
	}, catalog, nil
}

func buildRuntimeModelCatalog(cfg settings.Settings, injected credential.Resolver) RuntimeModelCatalog {
	type connectionBuilder struct {
		connection    RuntimeModelConnection
		seenModels    map[string]struct{} // wireModelID → seen, for dedup across aliases
		seenCanonical map[string]string   // wireModelID → canonical alias
	}
	// Wire v2: real availability instead of the hardcoded true. Probe each
	// model's credential through the configured resolver chain (env + the
	// credentials section + any injected resolver). The probe is cheap and
	// non-network — it only checks declaration existence / env presence, per
	// design-runtime-models-wire-v2 §5. A source-injected credential (e.g.
	// gateway JWT or a /v1/secrets post) is treated as configured.
	resolver := cfg.CredentialResolver(injected)
	ctx := context.Background()
	groups := make(map[string]*connectionBuilder)
	included := make(map[string]struct{})
	dedupTo := make(map[string]string) // deduped alias → canonical alias in the catalog
	modelNames := cfg.ModelNames()
	for _, alias := range modelNames {
		mc := cfg.Models[alias]
		if mc.Name == "" {
			mc.Name = alias
		}
		if strings.TrimSpace(mc.Model) == "" {
			continue
		}
		// Skip internal alias keys (provider.<b64>.model.<b64>) — only
		// user-facing names ({pid}/{mid}, runtime_alias, provider shortcut)
		// should appear in the runtime models catalog.
		if strings.HasPrefix(alias, "provider.") {
			continue
		}
		connectionID, _ := runtimeAliasComponents(alias)
		if mc.Catalog.ConnectionID != "" {
			connectionID = mc.Catalog.ConnectionID
		}
		if connectionID == "" {
			connectionID = alias
		}
		providerID := mc.Catalog.ProviderID
		if providerID == "" {
			providerID = mc.Provider
		}
		connectionName := mc.Catalog.ConnectionDisplayName
		if connectionName == "" {
			connectionName = connectionID
		}
		group := groups[connectionID]
		if group == nil {
			group = &connectionBuilder{connection: RuntimeModelConnection{
				ID: connectionID, ProviderID: providerID, DisplayName: connectionName,
				BillingSource: "server_managed", Models: []RuntimeModelDescriptor{},
			}, seenModels: make(map[string]struct{}), seenCanonical: make(map[string]string)}
			groups[connectionID] = group
		}
		displayName := mc.Catalog.DisplayName
		if displayName == "" {
			displayName = mc.Model
		}
		supportsTools := true
		if mc.Catalog.SupportsTools != nil {
			supportsTools = *mc.Catalog.SupportsTools
		}
		modalities := normalizedModalities(mc.Catalog.InputModalities)
		available, reason, credStatus, credSource := probeModelAvailability(cfg, mc, resolver, ctx)
		// Deduplicate by wire model within each connection: multiple
		// aliases (provider shortcut, runtime_alias, pid/mid) may point
		// to the same model — only the first (alphabetically) is kept.
		if _, seen := group.seenModels[mc.Model]; seen {
			included[alias] = struct{}{}
			dedupTo[alias] = group.seenCanonical[mc.Model]
			continue
		}
		group.seenModels[mc.Model] = struct{}{}
		group.seenCanonical[mc.Model] = alias // first alias wins as canonical
		// First model in the connection determines the connection-level
		// credential status (models in a connection share its credential).
		if group.connection.Credential == nil {
			if credStatus != "" {
				group.connection.Credential = &RuntimeConnectionCredential{Status: credStatus, Source: credSource}
			}
		}
		descriptor := RuntimeModelDescriptor{
			ID: settings.ModelID(alias, mc), Configured: true, ConfigurationID: settings.ModelConfigurationID(mc),
			RuntimeAlias: alias, WireModelID: mc.Model, DisplayName: displayName,
			ContextWindow: mc.ContextWindow, SupportsTools: supportsTools,
			SupportsReasoning: mc.Catalog.SupportsReasoning != nil && *mc.Catalog.SupportsReasoning,
			InputModalities:   modalities, Available: available,
			UnavailableReason:         reason,
			ReasoningEffort:           mc.ReasoningEffort,
			SupportedReasoningEfforts: mc.Catalog.SupportedReasoningEfforts,
			CanDisableReasoning:       mc.Catalog.CanDisableReasoning,
		}
		if verification, ok := model.RecentVerification(descriptor.ConfigurationID, time.Now()); ok {
			descriptor.Verified = true
			descriptor.VerifiedAt = &verification.At
			descriptor.ActualModel = verification.ActualModel
		}
		group.connection.Models = append(group.connection.Models, descriptor)
		included[alias] = struct{}{}
	}
	connectionIDs := make([]string, 0, len(groups))
	for id := range groups {
		connectionIDs = append(connectionIDs, id)
	}
	sort.Strings(connectionIDs)
	connections := make([]RuntimeModelConnection, 0, len(connectionIDs))
	for _, id := range connectionIDs {
		connection := groups[id].connection
		sort.Slice(connection.Models, func(i, j int) bool {
			return connection.Models[i].RuntimeAlias < connection.Models[j].RuntimeAlias
		})
		connections = append(connections, connection)
	}
	defaultAlias := cfg.DefaultModel
	if _, ok := included[defaultAlias]; ok {
		// Resolve through dedup chain: if the configured default_model is an
		// alias that was deduped away, use the canonical alias that appears
		// in the models list instead.
		if canon, deduped := dedupTo[defaultAlias]; deduped {
			defaultAlias = canon
		}
	} else {
		defaultAlias = ""
	}
	// A configured default may use the opaque AgentKit alias omitted from the
	// public list. Resolve it to the same connection/model configuration.
	if defaultAlias == "" {
		if mc, ok := cfg.Models[cfg.DefaultModel]; ok {
			if mc.Name == "" {
				mc.Name = cfg.DefaultModel
			}
			for _, c := range connections {
				for _, m := range c.Models {
					if m.ConfigurationID == settings.ModelConfigurationID(mc) {
						defaultAlias = m.RuntimeAlias
						break
					}
				}
			}
		}
	}
	defaultID := ""
	for _, connection := range connections {
		for _, m := range connection.Models {
			if m.RuntimeAlias == defaultAlias {
				defaultID = m.ID
			}
		}
	}
	return RuntimeModelCatalog{
		DefaultModel: defaultID, VerificationScope: "process", VerificationMaxAgeSeconds: int64(model.VerificationMaxAge / time.Second),
		Schema: "runtime-model-catalog/v2", DefaultRuntimeAlias: defaultAlias,
		Connections: connections,
	}
}

// probeModelAvailability is a non-networking local readiness check; injected
// credential declarations alone do not establish availability.
func probeModelAvailability(cfg settings.Settings, mc settings.ModelConfig, resolver credential.Resolver, ctx context.Context) (available bool, reason, credStatus, credSource string) {
	return settings.ProbeModelAvailability(ctx, cfg, mc, resolver)
}

func normalizedModalities(values []string) []string {
	if len(values) == 0 {
		return []string{"text"}
	}
	set := make(map[string]struct{})
	for _, value := range values {
		switch value = strings.ToLower(strings.TrimSpace(value)); value {
		case "text", "image", "audio":
			set[value] = struct{}{}
		}
	}
	if len(set) == 0 {
		return []string{"text"}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func runtimeAliasComponents(alias string) (connectionID, wireModelID string) {
	parts := strings.Split(alias, ".")
	if len(parts) != 4 || parts[0] != "provider" || parts[2] != "model" {
		return "", ""
	}
	connectionID, err := decodeAliasComponent(parts[1])
	if err != nil {
		return "", ""
	}
	wireModelID, err = decodeAliasComponent(parts[3])
	if err != nil {
		return "", ""
	}
	return connectionID, wireModelID
}

func decodeAliasComponent(value string) (string, error) {
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	decoded := string(data)
	if decoded == "" || base64.RawURLEncoding.EncodeToString(data) != value {
		return "", fmt.Errorf("non-canonical runtime alias component")
	}
	return decoded, nil
}

// Verification timestamps must not churn the durable configuration revision.
func catalogConfigurationFingerprint(catalog RuntimeModelCatalog) ([]byte, error) {
	catalog.Connections = append([]RuntimeModelConnection(nil), catalog.Connections...)
	for i := range catalog.Connections {
		c := &catalog.Connections[i]
		c.Models = append([]RuntimeModelDescriptor(nil), c.Models...)
		for j := range c.Models {
			c.Models[j].Verified = false
			c.Models[j].VerifiedAt = nil
			c.Models[j].ActualModel = ""
		}
	}
	return json.Marshal(catalog)
}
