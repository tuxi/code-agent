package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"code-agent/internal/app"
	"code-agent/internal/buildinfo"
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
		group.connection.Models = append(group.connection.Models, RuntimeModelDescriptor{
			RuntimeAlias: alias, WireModelID: mc.Model, DisplayName: displayName,
			ContextWindow: mc.ContextWindow, SupportsTools: supportsTools,
			SupportsReasoning: mc.Catalog.SupportsReasoning,
			InputModalities:   modalities, Available: true,
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
		Schema: "runtime-model-catalog/v1", DefaultRuntimeAlias: defaultAlias,
		Connections: connections,
	}
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
