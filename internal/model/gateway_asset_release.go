package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type ConversationAssetRefReleaser interface {
	ReleaseConversationAssetRefs(ctx context.Context, sessionID string) error
}

func (p *OpenAICompatibleProvider) ReleaseConversationAssetRefs(ctx context.Context, sessionID string) error {
	base, err := gatewayURL(p.BaseURL, "agent/conversations")
	if err != nil {
		return err
	}
	u, err := url.Parse(base)
	if err != nil {
		return err
	}
	pathPrefix := strings.TrimSuffix(u.Path, "/")
	u.Path = pathPrefix + "/" + sessionID + "/asset-refs"
	u.RawPath = pathPrefix + "/" + url.PathEscape(sessionID) + "/asset-refs"
	endpoint := u.String()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if err := p.applyAuth(ctx, req); err != nil {
		return err
	}
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &APIError{StatusCode: resp.StatusCode, Message: "gateway asset-ref release failed"}
	}
	var envelope struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("decode gateway asset-ref release: %w", err)
	}
	if envelope.Code != 0 {
		return &APIError{StatusCode: resp.StatusCode, Message: "gateway asset-ref release failed"}
	}
	return nil
}
