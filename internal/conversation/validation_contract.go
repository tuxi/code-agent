package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"code-agent/internal/agent"
	"code-agent/internal/model"
	"code-agent/internal/session"
)

type ValidationContractError struct{ Unsupported bool }

func (e ValidationContractError) Error() string { return e.SafeMessage() }
func (e ValidationContractError) AgentInputErrorCode() string {
	if e.Unsupported {
		return "validation_contract_unsupported"
	}
	return "invalid_validation_contract"
}
func (e ValidationContractError) SafeMessage() string {
	if e.Unsupported {
		return "Durable validation contracts are unavailable; the input was not accepted."
	}
	return "A valid versioned validation contract and stable request_id are required."
}

func (r *sqliteRepository) SupportsValidationContracts() bool {
	capability, ok := r.store.(session.ValidationContractPersistence)
	return ok && capability.SupportsValidationContracts()
}

func (e *TurnExecutor) SupportsValidationContracts() bool {
	_, inbox := e.repo.(TurnInputRepository)
	capability, ok := e.repo.(session.ValidationContractPersistence)
	return inbox && ok && capability.SupportsValidationContracts()
}

// ExecuteWithValidationContract adds typed metadata to the existing admission
// path. It is passed explicitly, so nested runs cannot inherit a parent's
// contract through a shared context.
func (e *TurnExecutor) ExecuteWithValidationContract(ctx context.Context, sessionID, requestID, text, modelName, effort string, assets []model.GatewayAssetRef, localAssets []model.LocalAssetRef, contract *session.ValidationContract) (agent.TurnResult, error) {
	if contract == nil {
		return agent.TurnResult{}, ValidationContractError{}
	}
	copy, err := contract.CloneValidated()
	if err != nil || !validationRequestID(requestID) {
		return agent.TurnResult{}, ValidationContractError{}
	}
	if !e.SupportsValidationContracts() {
		return agent.TurnResult{}, ValidationContractError{Unsupported: true}
	}
	return e.executeWithRequestIDAndAllAssetsValidation(ctx, sessionID, requestID, text, modelName, effort, assets, localAssets, copy)
}

func validationRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.:", r)) {
			return false
		}
	}
	return true
}

func (s *TransportSession) SendMessageWithValidationContract(ctx context.Context, requestID, text, modelName, effort string, assets []model.GatewayAssetRef, localAssets []model.LocalAssetRef, contract *session.ValidationContract) (agent.TurnResult, error) {
	return s.ex.ExecuteWithValidationContract(ctx, s.id, requestID, text, modelName, effort, assets, localAssets, contract)
}

func validationPayloadHash(base string, contract *session.ValidationContract) (string, error) {
	if contract == nil {
		return base, nil
	}
	hash, err := contract.Hash()
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(struct {
		PayloadHash  string `json:"payload_hash"`
		ContractHash string `json:"validation_contract_hash"`
	}{base, hash})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func (s *TransportSession) SupportsValidationContracts() bool {
	return s.ex != nil && s.ex.SupportsValidationContracts()
}
