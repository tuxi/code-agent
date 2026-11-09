package preview

import "strings"

const (
	CapabilityContract    = "preview_contract_v1"
	CapabilityInteractive = "preview_interactive_html_v1"
	CapabilityStatic      = "preview_static_asset_v1"
	CapabilityReplay      = "preview_replay_v1"
)

// RuntimeFlags is the REST transport form of the runtime preview capability
// snapshot. The JSON field names are the canonical wire capability strings.
type RuntimeFlags struct {
	Contract    bool `json:"preview_contract_v1"`
	Interactive bool `json:"preview_interactive_html_v1"`
	Static      bool `json:"preview_static_asset_v1"`
	Replay      bool `json:"preview_replay_v1"`
}

func FullRuntimeFlags() RuntimeFlags {
	return RuntimeFlags{Contract: true, Interactive: true, Static: true, Replay: true}
}

func (f RuntimeFlags) List() []string {
	out := make([]string, 0, 4)
	if f.Contract {
		out = append(out, CapabilityContract)
	}
	if f.Interactive {
		out = append(out, CapabilityInteractive)
	}
	if f.Static {
		out = append(out, CapabilityStatic)
	}
	if f.Replay {
		out = append(out, CapabilityReplay)
	}
	return out
}

type PresentationMode string

const (
	ModeInteractive                  PresentationMode = "interactive"
	ModeStatic                       PresentationMode = "static"
	ModeUnavailable                  PresentationMode = "unavailable"
	ModeDegradedInvalidRuntime       PresentationMode = "degraded_invalid_runtime_advertisement_static"
	ModeLiveInteractiveHistoryStatic PresentationMode = "live_interactive_history_static"
	ModeInteractiveUnknownIgnored    PresentationMode = "interactive_unknown_ignored"
	ModeUnavailableContractAbsent    PresentationMode = "unavailable_contract_absent"
)

type NegotiatedCapabilities struct {
	Runtime        RuntimeFlags
	Client         RuntimeFlags
	Effective      RuntimeFlags
	LiveMode       PresentationMode
	HistoryMode    PresentationMode
	InvalidRuntime bool
	InvalidClient  bool
}

// Negotiate computes the canonical intersection. Interactive implies static: an
// advertisement with interactive but no static is invalid and drops interactive
// while preserving a valid static surface.
func Negotiate(runtimeCaps, clientCaps []string, _ bool) NegotiatedCapabilities {
	runtimeSet := normalizeCapabilitySet(runtimeCaps)
	clientSet := normalizeCapabilitySet(clientCaps)
	invalidRuntime := runtimeSet[CapabilityInteractive] && !runtimeSet[CapabilityStatic]
	invalidClient := clientSet[CapabilityInteractive] && !clientSet[CapabilityStatic]
	if invalidRuntime || invalidClient {
		delete(runtimeSet, CapabilityInteractive)
		delete(clientSet, CapabilityInteractive)
	}

	effectiveSet := make(map[string]bool)
	for _, cap := range []string{CapabilityContract, CapabilityInteractive, CapabilityStatic, CapabilityReplay} {
		effectiveSet[cap] = runtimeSet[cap] && clientSet[cap]
	}
	if effectiveSet[CapabilityInteractive] {
		effectiveSet[CapabilityStatic] = true
	}
	if !effectiveSet[CapabilityContract] {
		effectiveSet[CapabilityStatic] = false
		effectiveSet[CapabilityInteractive] = false
		effectiveSet[CapabilityReplay] = false
	}
	// A runtime advertisement of interactive without static is invalid, but the
	// client's validated static surface still governs the degraded fallback.
	// This keeps negotiation and presentation on the same interactive=>static
	// implication used by AgentKit.
	if invalidRuntime &&
		runtimeSet[CapabilityContract] &&
		clientSet[CapabilityContract] &&
		clientSet[CapabilityStatic] {
		effectiveSet[CapabilityStatic] = true
	}
	if !effectiveSet[CapabilityStatic] {
		effectiveSet[CapabilityInteractive] = false
		effectiveSet[CapabilityReplay] = false
	}

	result := NegotiatedCapabilities{
		Runtime:        flagsFromSet(runtimeSet),
		Client:         flagsFromSet(clientSet),
		Effective:      flagsFromSet(effectiveSet),
		InvalidRuntime: invalidRuntime,
		InvalidClient:  invalidClient,
	}
	switch {
	case !result.Effective.Contract:
		if len(runtimeSet) == 0 && len(clientSet) == 0 {
			result.LiveMode = ModeUnavailableContractAbsent
			result.HistoryMode = ModeUnavailableContractAbsent
		} else {
			result.LiveMode = ModeUnavailable
			result.HistoryMode = ModeUnavailable
		}
	case result.Effective.Interactive:
		result.LiveMode = ModeInteractive
		if result.Effective.Replay {
			result.HistoryMode = ModeInteractive
		} else {
			result.HistoryMode = ModeStatic
		}
	case result.Effective.Static:
		result.LiveMode = ModeStatic
		result.HistoryMode = ModeStatic
		if invalidRuntime {
			result.LiveMode = ModeDegradedInvalidRuntime
			result.HistoryMode = ModeDegradedInvalidRuntime
		}
	default:
		result.LiveMode = ModeUnavailable
		result.HistoryMode = ModeUnavailable
	}
	if invalidRuntime &&
		runtimeSet[CapabilityContract] &&
		clientSet[CapabilityContract] &&
		clientSet[CapabilityStatic] {
		result.LiveMode = ModeDegradedInvalidRuntime
		result.HistoryMode = ModeDegradedInvalidRuntime
	}
	if result.Effective.Interactive && runtimeSet[CapabilityReplay] && !clientSet[CapabilityReplay] {
		result.LiveMode = ModeLiveInteractiveHistoryStatic
		result.HistoryMode = ModeStatic
	}
	if result.Effective.Interactive && (hasUnknownCapability(runtimeCaps) || hasUnknownCapability(clientCaps)) {
		result.LiveMode = ModeInteractiveUnknownIgnored
	}
	return result
}

func normalizeCapabilitySet(values []string) map[string]bool {
	out := make(map[string]bool)
	for _, value := range values {
		switch strings.TrimSpace(value) {
		case CapabilityContract:
			out[CapabilityContract] = true
		case CapabilityInteractive:
			out[CapabilityInteractive] = true
		case CapabilityStatic:
			out[CapabilityStatic] = true
		case CapabilityReplay:
			out[CapabilityReplay] = true
		}
	}
	return out
}

func flagsFromSet(set map[string]bool) RuntimeFlags {
	return RuntimeFlags{
		Contract:    set[CapabilityContract],
		Interactive: set[CapabilityInteractive],
		Static:      set[CapabilityStatic],
		Replay:      set[CapabilityReplay],
	}
}

func hasUnknownCapability(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) != "" && !IsCapability(value) {
			return true
		}
	}
	return false
}

func IsCapability(value string) bool {
	switch strings.TrimSpace(value) {
	case CapabilityContract, CapabilityInteractive, CapabilityStatic, CapabilityReplay:
		return true
	default:
		return false
	}
}

func ReconcileAdvertised(capabilities []string, flags RuntimeFlags) ([]string, bool) {
	if flags.Interactive && !flags.Static {
		flags.Interactive = false
	}
	if !flags.Contract {
		flags.Static = false
		flags.Interactive = false
		flags.Replay = false
	}
	want := make(map[string]bool)
	for _, capability := range flags.List() {
		want[capability] = true
	}
	out := make([]string, 0, len(capabilities)+len(want))
	seen := make(map[string]bool)
	mismatch := false
	for _, capability := range capabilities {
		if !IsCapability(capability) {
			if !seen[capability] {
				out = append(out, capability)
				seen[capability] = true
			}
			continue
		}
		if want[capability] {
			if !seen[capability] {
				out = append(out, capability)
				seen[capability] = true
			}
		} else {
			mismatch = true
		}
	}
	for _, capability := range flags.List() {
		if !seen[capability] {
			out = append(out, capability)
			seen[capability] = true
		}
	}
	return out, mismatch
}
