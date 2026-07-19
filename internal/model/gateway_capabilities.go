package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type ImageInputCapabilityProber interface {
	ImageInputCapability(ctx context.Context) (bool, error)
}

type gatewayCapabilityEnvelope struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Contract        string   `json:"contract"`
		ContractVersion int      `json:"contract_version"`
		Capabilities    []string `json:"capabilities"`
	} `json:"data"`
}

type capabilityCacheEntry struct {
	enabled bool
	expires time.Time
}

var gatewayCapabilityCache = struct {
	sync.Mutex
	entries map[string]capabilityCacheEntry
}{entries: make(map[string]capabilityCacheEntry)}

func (p *OpenAICompatibleProvider) ImageInputCapability(ctx context.Context) (bool, error) {
	key := p.BaseURL + "\x00" + p.AssetUploadScope(ctx)
	now := time.Now()
	gatewayCapabilityCache.Lock()
	entry, cached := gatewayCapabilityCache.entries[key]
	gatewayCapabilityCache.Unlock()
	if cached && now.Before(entry.expires) {
		return entry.enabled, nil
	}

	endpoint, err := gatewayURL(p.BaseURL, "agent/capabilities")
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/json")
	if err := p.applyAuth(ctx, req); err != nil {
		return false, err
	}
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		// An unexpired success would have returned above. Expired entries fail
		// closed exactly as the frozen contract requires.
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		gatewayCapabilityCache.Lock()
		delete(gatewayCapabilityCache.entries, key)
		gatewayCapabilityCache.Unlock()
		return false, &APIError{StatusCode: resp.StatusCode, Code: "auth_expired", Message: "gateway capability authentication failed"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, &APIError{StatusCode: resp.StatusCode, Message: "gateway capability request failed"}
	}
	var envelope gatewayCapabilityEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return false, fmt.Errorf("decode gateway capabilities: %w", err)
	}
	enabled := envelope.Code == 0 && envelope.Data.Contract == "runtime-gateway-user-assets" && envelope.Data.ContractVersion == 1
	if enabled {
		enabled = false
		for _, capability := range envelope.Data.Capabilities {
			if capability == "image_input" {
				enabled = true
				break
			}
		}
	}
	gatewayCapabilityCache.Lock()
	gatewayCapabilityCache.entries[key] = capabilityCacheEntry{enabled: enabled, expires: now.Add(60 * time.Second)}
	gatewayCapabilityCache.Unlock()
	return enabled, nil
}
