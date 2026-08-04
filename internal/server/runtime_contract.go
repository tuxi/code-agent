package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"code-agent/internal/app"
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
	Schema              string                   `json:"schema"`
	Revision            int64                    `json:"revision"`
	DefaultRuntimeAlias string                   `json:"default_runtime_alias"`
	Connections         []RuntimeModelConnection `json:"connections"`
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
	RuntimeAlias      string   `json:"runtime_alias"`
	WireModelID       string   `json:"wire_model_id"`
	DisplayName       string   `json:"display_name"`
	ContextWindow     int      `json:"context_window,omitempty"`
	SupportsTools     bool     `json:"supports_tools"`
	SupportsReasoning bool     `json:"supports_reasoning"`
	InputModalities   []string `json:"input_modalities"`
	Available         bool     `json:"available"`
	// UnavailableReason explains why Available is false (wire v2). Omitted when
	// the model is available.
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

// BuildRuntimeContract constructs allowlisted public DTOs and binds them to the
// durable identity of the current session data source.
func BuildRuntimeContract(cfg app.Config, root, displayName, profile string) (RuntimeInfo, RuntimeModelCatalog, error) {
	catalog := buildRuntimeModelCatalog(cfg)
	fingerprint, err := json.Marshal(catalog)
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

func buildRuntimeModelCatalog(cfg app.Config) RuntimeModelCatalog {
	type connectionBuilder struct {
		connection RuntimeModelConnection
	}
	// Wire v2: real availability instead of the hardcoded true. Probe each
	// model's credential through the configured resolver chain (env + the
	// credentials section). The probe is cheap and non-network — it only checks
	// declaration existence / env presence, per design-runtime-models-wire-v2 §5.
	// A source-injected credential (e.g. gateway JWT) is treated as configured:
	// the catalog is a snapshot taken before the host's next Reconfigure, and
	// marking it unavailable would blank the gateway model on first start.
	resolver := cfg.CredentialResolver(nil)
	ctx := context.Background()
	groups := make(map[string]*connectionBuilder)
	included := make(map[string]struct{})
	for _, alias := range cfg.ModelNames() {
		mc := cfg.Models[alias]
		if strings.TrimSpace(mc.Model) == "" {
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
			}}
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
		// First model in the connection determines the connection-level
		// credential status (models in a connection share its credential).
		if group.connection.Credential == nil {
			if credStatus != "" {
				group.connection.Credential = &RuntimeConnectionCredential{Status: credStatus, Source: credSource}
			}
		}
		group.connection.Models = append(group.connection.Models, RuntimeModelDescriptor{
			RuntimeAlias: alias, WireModelID: mc.Model, DisplayName: displayName,
			ContextWindow: mc.ContextWindow, SupportsTools: supportsTools,
			SupportsReasoning: mc.Catalog.SupportsReasoning,
			InputModalities:   modalities, Available: available,
			UnavailableReason: reason,
		})
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
	if _, ok := included[defaultAlias]; !ok {
		defaultAlias = ""
	}
	return RuntimeModelCatalog{
		Schema: "runtime-model-catalog/v2", DefaultRuntimeAlias: defaultAlias,
		Connections: connections,
	}
}

// probeModelAvailability reports whether a model can be called right now and
// the connection-level credential status to surface. A model is available when
// its credential resolves to a non-zero value, its endpoint needs no
// credential (local base URL / source none), or its credential is declared
// as injected (the real secret will be provided by the host at runtime; the
// env-based resolver chain won't see it at catalog-build time).
func probeModelAvailability(cfg app.Config, mc app.ModelConfig, resolver credential.Resolver, ctx context.Context) (available bool, reason, credStatus, credSource string) {
	// Declared as injected → treat as configured (the secret is not in the
	// env chain — it arrives via injectSecrets / Reconfigure at runtime).
	if !mc.Credential.IsZero() {
		ns, name := mc.Credential.Namespace, mc.Credential.Name
		if entries, ok := cfg.Credentials[ns]; ok {
			if cc, ok := entries[name]; ok && (cc.Source == "injected" || cc.Source == "jwt" || cc.Source == "keychain") {
				return true, "", "configured", "injected"
			}
		}
	}
	// No credential ref and a local base URL → no auth needed.
	if mc.Credential.IsZero() {
		if model.IsLocalBaseURL(mc.BaseURL) {
			return true, "", "none", "none"
		}
		// No credential declared at all: treat as unavailable-but-listed so the
		// host sees why the model cannot run.
		return false, "no_auth", "missing", ""
	}
	// Resolve through the chain (env / credentials section / injected).
	resolved, err := resolver.Resolve(ctx, mc.Credential.Target())
	if err != nil {
		return false, "no_auth", "missing", ""
	}
	if resolved.IsZero() {
		return false, "no_auth", "missing", "env"
	}
	return true, "", "configured", credentialSourceFor(mc.Credential)
}

// credentialSourceFor maps a credential ref's namespace to the wire "source"
// label. Injected credentials (gateway) are declared, not probed.
func credentialSourceFor(ref app.CredentialRef) string {
	if ref.Namespace == "gateway" {
		return "injected"
	}
	return "env"
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
