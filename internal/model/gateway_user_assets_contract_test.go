package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type contractRoundTrip func(*http.Request) (*http.Response, error)

func (f contractRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func contractFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "protocols", "fixtures", "runtime-gateway-user-assets", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestGatewayCapabilityCanonicalFixture(t *testing.T) {
	p := NewOpenAICompatibleProviderWithKey("https://gateway-cap.test/api/v1/agent", "jwt")
	p.HTTPClient = &http.Client{Transport: contractRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.Path != "/api/v1/agent/capabilities" || req.Header.Get("Authorization") != "Bearer jwt" {
			t.Fatalf("request=%s %s auth=%q", req.Method, req.URL.Path, req.Header.Get("Authorization"))
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(contractFixture(t, "capabilities_image_input.json")))), Header: make(http.Header)}, nil
	})}
	enabled, err := p.ImageInputCapability(context.Background())
	if err != nil || !enabled {
		t.Fatalf("enabled=%v err=%v", enabled, err)
	}
}

func TestGatewayCapabilityExpiredCacheFailsClosedAndAuthInvalidates(t *testing.T) {
	p := NewOpenAICompatibleProviderWithKey("https://gateway-expired.test/api/v1/agent", "jwt-expired")
	key := p.BaseURL + "\x00" + p.AssetUploadScope(context.Background())
	gatewayCapabilityCache.Lock()
	gatewayCapabilityCache.entries[key] = capabilityCacheEntry{enabled: true, expires: time.Now().Add(-time.Second)}
	gatewayCapabilityCache.Unlock()
	p.HTTPClient = &http.Client{Transport: contractRoundTrip(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("gateway offline")
	})}
	if enabled, err := p.ImageInputCapability(context.Background()); err == nil || enabled {
		t.Fatalf("expired network result enabled=%v err=%v", enabled, err)
	}
	p.HTTPClient = &http.Client{Transport: contractRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	})}
	if enabled, err := p.ImageInputCapability(context.Background()); err == nil || enabled {
		t.Fatalf("auth result enabled=%v err=%v", enabled, err)
	}
	gatewayCapabilityCache.Lock()
	_, cached := gatewayCapabilityCache.entries[key]
	gatewayCapabilityCache.Unlock()
	if cached {
		t.Fatal("401 must invalidate the credential-scoped capability cache")
	}
}

func TestGatewayChatCanonicalIdentityAndAssets(t *testing.T) {
	asset := GatewayAssetRef{AssetID: 10001, SHA256: strings.Repeat("a", 64), Kind: "image", MIMEType: "image/jpeg", Filename: "build-error.jpg"}
	req := Request{
		SessionID: "sess_01J2Y8", TurnID: "turn_01J2Y9", RequestID: "req_01J2YA",
		ExecutionID: "exec_01J2YB", Model: "deepseek-v4-pro",
		Messages: []Message{{Role: RoleUser, Content: "解释这张截图里的错误", Assets: []GatewayAssetRef{asset}}},
	}
	body := chatCompletionRequest{SessionID: req.SessionID, TurnID: req.TurnID, RequestID: req.RequestID, ExecutionID: req.ExecutionID, Model: req.Model, Messages: req.Messages, Tools: toolsForGatewayRequest(req.Messages, req.Tools), Stream: true}
	actual, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(actual, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(contractFixture(t, "chat_request_with_user_asset.json"), &want); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"session_id", "turn_id", "request_id", "execution_id", "model"} {
		if got[key] != want[key] {
			t.Fatalf("%s=%v want=%v", key, got[key], want[key])
		}
	}
	if _, ok := got["tools"].([]any); !ok {
		t.Fatalf("tools must be an explicit array: %s", actual)
	}
	gotMessages, _ := json.Marshal(got["messages"])
	wantMessages, _ := json.Marshal(want["messages"])
	if string(gotMessages) != string(wantMessages) {
		t.Fatalf("messages=%s want=%s", gotMessages, wantMessages)
	}
}

func TestKnownUserAssetErrorsAreSafeAndNonRetryable(t *testing.T) {
	err := apiErrorFromBody(http.StatusNotFound, contractFixture(t, "chat_error_asset_unavailable.json"))
	code, ok := UserAssetErrorCode(err)
	if !ok || code != "asset_unavailable" {
		t.Fatalf("code=%q ok=%v err=%v", code, ok, err)
	}
	if IsRetryable(err) {
		t.Fatal("known user asset error must not be retried")
	}
	if SafeUserAssetErrorMessage(code) != "One or more image assets are unavailable" {
		t.Fatal("unsafe asset error mapping")
	}
}

func TestAllFrozenUserAssetErrorsAreSafeAndNonRetryable(t *testing.T) {
	for code := range userAssetErrorMessages {
		err := &APIError{StatusCode: http.StatusBadGateway, Type: "user_asset_error", Code: code, Message: "unsafe upstream detail"}
		mapped, ok := UserAssetErrorCode(err)
		if !ok || mapped != code || IsRetryable(err) || SafeUserAssetErrorMessage(mapped) == "unsafe upstream detail" {
			t.Fatalf("code=%q mapped=%q ok=%v retryable=%v", code, mapped, ok, IsRetryable(err))
		}
	}
}

func TestUnknownUserAssetErrorCollapsesToSafeRequestFailed(t *testing.T) {
	err := &APIError{StatusCode: http.StatusBadGateway, Type: "user_asset_error", Code: "future_internal_detail", Message: "secret upstream detail"}
	code, ok := UserAssetErrorCode(err)
	if !ok || code != "request_failed" || SafeUserAssetErrorMessage(code) != "Request failed" {
		t.Fatalf("code=%q ok=%v message=%q", code, ok, SafeUserAssetErrorMessage(code))
	}
	if IsRetryable(err) {
		t.Fatal("unknown user_asset_error must not be retried or fall back")
	}
}

func TestGatewaySSEUserAssetErrorFixtureIsParsedWithoutFallback(t *testing.T) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, contractFixture(t, "chat_sse_error_image_processing_failed.json")); err != nil {
		t.Fatal(err)
	}
	sse := "data: " + compact.String() + "\n\n"
	inner := NewOpenAICompatibleProviderWithKey("https://gateway-stream.test/api/v1/agent", "jwt")
	inner.HTTPClient = &http.Client{Transport: contractRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(sse)), Header: http.Header{"Content-Type": {"text/event-stream"}}}, nil
	})}
	provider := &ResilientProvider{Inner: inner, MaxRetries: 3, LogWriter: io.Discard}
	_, err := provider.CompleteStream(context.Background(), Request{Model: "text-model"}, func(string) {})
	code, ok := UserAssetErrorCode(err)
	if !ok || code != "image_processing_failed" {
		t.Fatalf("code=%q ok=%v err=%v", code, ok, err)
	}
}

func TestGatewayConversationAssetRelease(t *testing.T) {
	p := NewOpenAICompatibleProviderWithKey("https://gateway-release.test/api/v1/agent", "jwt")
	p.HTTPClient = &http.Client{Transport: contractRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodDelete || req.URL.Path != "/api/v1/agent/conversations/sess_01J2Y8/asset-refs" {
			t.Fatalf("request=%s %s", req.Method, req.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(contractFixture(t, "release_asset_refs.json")))), Header: make(http.Header)}, nil
	})}
	if err := p.ReleaseConversationAssetRefs(context.Background(), "sess_01J2Y8"); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayConversationAssetReleasePercentEncodesSessionPathSegment(t *testing.T) {
	p := NewOpenAICompatibleProviderWithKey("https://gateway-release-escape.test/api/v1/agent", "jwt")
	p.HTTPClient = &http.Client{Transport: contractRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.URL.EscapedPath() != "/api/v1/agent/conversations/session%2Fchild/asset-refs" {
			t.Fatalf("escaped path=%q", req.URL.EscapedPath())
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"code":0,"msg":"success"}`)), Header: make(http.Header)}, nil
	})}
	if err := p.ReleaseConversationAssetRefs(context.Background(), "session/child"); err != nil {
		t.Fatal(err)
	}
}
