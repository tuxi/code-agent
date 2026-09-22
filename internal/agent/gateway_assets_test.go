package agent

import (
	"context"
	"strings"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/tools"
)

func TestRunTurnWithAssetsPreservesUserGatewayAsset(t *testing.T) {
	provider := &scriptedProvider{responses: []model.Response{{Content: "done"}}}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 1, UserAssetsSupported: true, RequestID: "req_7", ReservedTurnID: "turn_7"}
	asset := model.GatewayAssetRef{AssetID: 7, Kind: "image", MIMEType: "image/jpeg", Filename: "error.jpg", SHA256: "sha"}
	if _, err := runner.RunTurnWithAssets(context.Background(), &session.Session{ID: "session", Messages: []model.Message{{Role: model.RoleSystem, Content: "test"}}, Metadata: map[string]any{}}, "analyze", []model.GatewayAssetRef{asset}); err != nil {
		t.Fatal(err)
	}
	if provider.lastRequest.TurnID != "turn_7" || provider.lastRequest.RequestID != "req_7" || provider.lastRequest.ExecutionID == "" {
		t.Fatalf("gateway identities missing: %+v", provider.lastRequest)
	}
	if len(provider.lastMessages) < 2 || len(provider.lastMessages[1].Assets) != 1 || provider.lastMessages[1].Assets[0].AssetID != 7 {
		t.Fatalf("user assets lost: %+v", provider.lastMessages)
	}
}

func TestRunTurnWithLocalAssetsSendsOnlySafeManifestToProvider(t *testing.T) {
	provider := &scriptedProvider{responses: []model.Response{{Content: "done"}}}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 1, RequestID: "req_local", ReservedTurnID: "turn_local"}
	local := model.LocalAssetRef{
		ID: "doc", RelativePath: "user-assets/doc/report.pdf", Filename: "report.pdf",
		MIMEType: "application/pdf", Kind: "pdf", SizeBytes: 42,
		SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TransferPolicy: "local_only",
	}
	sess := &session.Session{ID: "session", Messages: []model.Message{{Role: model.RoleSystem, Content: "test"}}, Metadata: map[string]any{}}
	if _, err := runner.RunTurnWithAllAssets(context.Background(), sess, "", nil, []model.LocalAssetRef{local}); err != nil {
		t.Fatal(err)
	}
	if len(sess.Messages[1].LocalAssets) != 1 || sess.Messages[1].Content != "" {
		t.Fatalf("runtime session did not preserve local attachment: %+v", sess.Messages[1])
	}
	if len(provider.lastMessages) < 2 || len(provider.lastMessages[1].LocalAssets) != 0 {
		t.Fatalf("provider received structured local assets: %+v", provider.lastMessages)
	}
	content := provider.lastMessages[1].Content
	for _, want := range []string{"user-assets/doc/report.pdf", "report.pdf", "application/pdf", "size_bytes=42", "NOT visible", "Do not guess", "read_pdf"} {
		if !strings.Contains(content, want) {
			t.Fatalf("manifest %q does not contain %q", content, want)
		}
	}
	if strings.Contains(content, local.SHA256) {
		t.Fatalf("manifest leaked sha256: %q", content)
	}
}
