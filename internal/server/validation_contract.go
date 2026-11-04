package server

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Contract-bearing envelopes cannot let duplicate routing/identity fields
// silently select a different declaration. Other unknown keys remain compatible.
func unambiguousValidationEnvelope(data []byte) bool {
	d := json.NewDecoder(bytes.NewReader(data))
	if opening, err := d.Token(); err != nil || opening != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return false
		}
		key, ok := token.(string)
		if !ok {
			return false
		}
		if key == "validation_contract" || key == "request_id" || key == "kind" {
			if seen[key] {
				return false
			}
			seen[key] = true
		}
		var raw json.RawMessage
		if d.Decode(&raw) != nil {
			return false
		}
	}
	return true // the outer decoder already checked closing/trailing JSON
}

func validValidationRequestID(id string) bool {
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
