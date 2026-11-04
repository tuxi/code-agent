package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// ValidationContract is submission metadata, never model configuration or an
// instruction to execute checks. It is immutable once its Turn is accepted.
type ValidationContract struct {
	Version            int      `json:"version"`
	ValidationCommands []string `json:"validation_commands"`
	ManualChecks       []string `json:"manual_checks,omitempty"`
}

const ValidationContractVersion = 1

var ErrInvalidValidationContract = errors.New("invalid or unsupported validation contract")

func (c ValidationContract) CloneValidated() (*ValidationContract, error) {
	if c.Version != ValidationContractVersion || len(c.ValidationCommands) > 32 || len(c.ManualChecks) > 32 {
		return nil, ErrInvalidValidationContract
	}
	for _, list := range []struct {
		values []string
		limit  int
	}{{c.ValidationCommands, 4096}, {c.ManualChecks, 1024}} {
		for _, value := range list.values {
			if !utf8.ValidString(value) || strings.TrimSpace(value) == "" || len(value) > list.limit {
				return nil, ErrInvalidValidationContract
			}
		}
	}
	copy := c
	copy.ValidationCommands = append([]string{}, c.ValidationCommands...)
	copy.ManualChecks = append([]string(nil), c.ManualChecks...)
	return &copy, nil
}

// Strict decoding prevents future/unknown policy fields from being silently
// ignored and then persisted as a different contract.
func (c *ValidationContract) UnmarshalJSON(data []byte) error {
	if !utf8.Valid(data) {
		return ErrInvalidValidationContract
	}
	d := json.NewDecoder(bytes.NewReader(data))
	opening, err := d.Token()
	if err != nil || opening != json.Delim('{') {
		return ErrInvalidValidationContract
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return ErrInvalidValidationContract
		}
		key, ok := token.(string)
		if !ok || (key != "version" && key != "validation_commands" && key != "manual_checks") {
			return ErrInvalidValidationContract
		}
		if _, duplicate := fields[key]; duplicate {
			return ErrInvalidValidationContract
		}
		var raw json.RawMessage
		if d.Decode(&raw) != nil {
			return ErrInvalidValidationContract
		}
		fields[key] = raw
	}
	closing, err := d.Token()
	if err != nil || closing != json.Delim('}') || d.Decode(new(any)) != io.EOF {
		return ErrInvalidValidationContract
	}
	var decoded ValidationContract
	if json.Unmarshal(fields["version"], &decoded.Version) != nil {
		return ErrInvalidValidationContract
	}
	if raw, ok := fields["validation_commands"]; ok && json.Unmarshal(raw, &decoded.ValidationCommands) != nil {
		return ErrInvalidValidationContract
	}
	if raw, ok := fields["manual_checks"]; ok && json.Unmarshal(raw, &decoded.ManualChecks) != nil {
		return ErrInvalidValidationContract
	}
	copy, err := decoded.CloneValidated()
	if err != nil {
		return ErrInvalidValidationContract
	}
	*c = *copy
	return nil
}

func (c ValidationContract) Hash() (string, error) {
	copy, err := c.CloneValidated()
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ValidationContractPersistence is optional: an arbitrary TurnInputStore must
// not be treated as supporting this immutable event/inbox agreement.
type ValidationContractPersistence interface{ SupportsValidationContracts() bool }

// AcceptedValidation reads only the immutable acceptance metadata. Both core
// Event JSON (SessionID) and Agent Wire keys are recognized, without importing
// the agent or server packages into the storage layer.
func AcceptedValidation(record EventRecord, input TurnInput) (*ValidationContract, error) {
	if len(bytes.TrimSpace(record.Payload)) == 0 {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(record.Payload, &fields) != nil {
		return nil, ErrInvalidValidationContract
	}
	raw, hasContract := fields["validation_contract"]
	hashRaw, hasHash := fields["validation_contract_hash"]
	if !hasContract && !hasHash {
		return nil, nil
	}
	if !hasContract || !hasHash || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, ErrInvalidValidationContract
	}
	if input.SessionID == "" || input.TurnID == "" || strings.TrimSpace(input.RequestID) == "" {
		return nil, ErrInvalidValidationContract
	}
	for _, key := range []string{"Kind", "kind"} {
		if value, ok := fields[key]; ok {
			var kind string
			if json.Unmarshal(value, &kind) != nil || kind != "turn_accepted" {
				return nil, ErrInvalidValidationContract
			}
		}
	}
	if record.SessionID != input.SessionID || record.TurnID != input.TurnID || record.Kind != "turn_accepted" {
		return nil, ErrInvalidValidationContract
	}
	for _, identity := range []struct{ core, wire, want string }{{"SessionID", "session_id", input.SessionID}, {"TurnID", "turn_id", input.TurnID}, {"RequestID", "request_id", input.RequestID}} {
		found := false
		for _, key := range []string{identity.core, identity.wire} {
			if value, ok := fields[key]; ok {
				var actual string
				if json.Unmarshal(value, &actual) != nil || actual != identity.want {
					return nil, ErrInvalidValidationContract
				}
				found = true
			}
		}
		if !found {
			return nil, ErrInvalidValidationContract
		}
	}
	var contract ValidationContract
	var storedHash string
	if json.Unmarshal(raw, &contract) != nil || json.Unmarshal(hashRaw, &storedHash) != nil {
		return nil, ErrInvalidValidationContract
	}
	hash, err := contract.Hash()
	if err != nil || storedHash != hash {
		return nil, ErrInvalidValidationContract
	}
	return &contract, nil
}
