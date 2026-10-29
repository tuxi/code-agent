package model

import (
	"sync"
	"time"
)

// Selection is audit data, never provider request content. ActualModel is only
// populated from an upstream response, never inferred from the request.
type Selection struct {
	RequestedModel  *string `json:"requested_model"`
	ResolvedModel   string  `json:"resolved_model"`
	Provider        string  `json:"provider"`
	ActualModel     string  `json:"actual_model,omitempty"`
	ConfigurationID string  `json:"configuration_id"`
}

type Verification struct {
	At          time.Time `json:"at"`
	ActualModel string    `json:"actual_model,omitempty"`
}

// The cache is process-scoped; durable history remains in model_finished events.
const VerificationMaxAge = 24 * time.Hour

var verifications = struct {
	sync.Mutex
	entries map[string]Verification
}{entries: make(map[string]Verification)}

func RecordVerification(selection Selection, at time.Time) {
	if selection.ConfigurationID == "" {
		return
	}
	verifications.Lock()
	defer verifications.Unlock()
	if len(verifications.entries) >= 1024 {
		var oldest string
		var oldestAt time.Time
		for id, v := range verifications.entries {
			if oldest == "" || v.At.Before(oldestAt) {
				oldest, oldestAt = id, v.At
			}
		}
		delete(verifications.entries, oldest)
	}
	verifications.entries[selection.ConfigurationID] = Verification{At: at.UTC(), ActualModel: selection.ActualModel}
}

func RecentVerification(configurationID string, now time.Time) (Verification, bool) {
	verifications.Lock()
	defer verifications.Unlock()
	v, ok := verifications.entries[configurationID]
	return v, ok && !v.At.After(now) && now.Sub(v.At) <= VerificationMaxAge
}
