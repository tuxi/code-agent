package mcpbridge

import (
	"context"
	"encoding/json"

	"code-agent/internal/session"
)

type ValidationContractRef struct {
	ServerID    string `json:"server_id,omitempty"`
	StoreID     string `json:"store_id,omitempty"`
	SessionID   string `json:"session_id"`
	TurnID      string `json:"turn_id"`
	RequestID   string `json:"request_id"`
	AcceptedSeq int64  `json:"accepted_seq"`
}

type ValidationProvenance struct {
	ValidationSource          string                 `json:"validation_source"`
	ValidationContractVersion int                    `json:"validation_contract_version,omitempty"`
	ValidationContractHash    string                 `json:"validation_contract_hash,omitempty"`
	ValidationContractRef     *ValidationContractRef `json:"validation_contract_ref,omitempty"`
	PendingManualChecks       []string               `json:"pending_manual_checks,omitempty"`
}

func persistedHash(contract *session.ValidationContract) string {
	hash, _ := contract.Hash() // caller has already strictly validated the contract
	return hash
}

func (c *Client) requireValidationCapability(ctx context.Context) error {
	var response struct {
		Capabilities struct {
			Durable bool `json:"durable_validation_contract_v1"`
		} `json:"capabilities"`
	}
	if err := c.request(ctx, "GET", "/v1/runtime/capabilities", nil, &response); err != nil || !response.Capabilities.Durable {
		return bridgeError("validation_contract_unsupported", err)
	}
	return nil
}

// Restore from the exact immutable acceptance, never the most recent message or
// a process-local declaration. Missing legacy metadata remains not_declared.
func (c *Client) validationFromEvents(events []Event, id, turn string) (*session.ValidationContract, ValidationProvenance, error) {
	p := ValidationProvenance{ValidationSource: "not_declared"}
	var accepted *Event
	var previous int64
	hasTerminal := false
	for i := range events {
		e := &events[i]
		if e.SessionID != id || e.Seq <= previous {
			return nil, p, bridgeError("evidence_incomplete", nil)
		}
		previous = e.Seq
		if e.TurnID != turn {
			continue
		}
		if e.Kind == "turn_finished" || e.Kind == "turn_failed" || e.Kind == "turn_cancelled" {
			hasTerminal = true
		}
		if e.Kind != "turn_accepted" {
			if len(e.ValidationContract) > 0 || e.ValidationContractHash != "" {
				return nil, p, bridgeError("evidence_incomplete", nil)
			}
			continue
		}
		if accepted != nil || !validID(e.RequestID) {
			return nil, p, bridgeError("evidence_incomplete", nil)
		}
		accepted = e
	}
	if accepted == nil {
		// Legacy Read can surface nonterminal attention before an acceptance is
		// observed. This proves neither admission nor validation; a terminal
		// handoff still requires its exact acceptance.
		if !hasTerminal {
			return nil, p, nil
		}
		return nil, p, bridgeError("evidence_incomplete", nil)
	}
	e := *accepted
	for _, other := range events {
		if other.Kind == "turn_accepted" && other.RequestID == e.RequestID && other.TurnID != turn {
			return nil, p, bridgeError("evidence_incomplete", nil)
		}
	}
	// Omitted keys must remain omitted: a legacy event is not a null contract.
	payload, err := json.Marshal(e)
	if err != nil {
		return nil, p, bridgeError("evidence_incomplete", err)
	}
	contract, err := session.AcceptedValidation(session.EventRecord{SessionID: id, TurnID: turn, Kind: e.Kind, Payload: payload}, session.TurnInput{SessionID: id, TurnID: turn, RequestID: e.RequestID})
	if err != nil {
		return nil, p, bridgeError("evidence_incomplete", err)
	}
	if contract == nil {
		return nil, p, nil
	}
	ref := &ValidationContractRef{SessionID: id, TurnID: turn, RequestID: e.RequestID, AcceptedSeq: e.Seq}
	if c.binding != nil {
		ref.ServerID, ref.StoreID = c.binding.verifiedServerID, c.binding.verifiedStoreID
	}
	p.ValidationSource, p.ValidationContractVersion, p.ValidationContractHash, p.ValidationContractRef = "persisted_turn_accepted", contract.Version, e.ValidationContractHash, ref
	return contract, p, nil
}
