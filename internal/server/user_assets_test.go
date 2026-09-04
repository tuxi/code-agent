package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/model"
)

type rejectionRecorder struct{ ch chan AgentInputRejected }

func (r rejectionRecorder) RejectInput(rejected AgentInputRejected) { r.ch <- rejected }

type conflictCommands struct{ *fakeCommands }

func (c conflictCommands) SendMessageWithRequestIDAndAssets(context.Context, string, string, string, string, []model.GatewayAssetRef) (agent.TurnResult, error) {
	return agent.TurnResult{}, testInputError{}
}

type failingRequestCommands struct {
	*fakeCommands
	result agent.TurnResult
}

func (c failingRequestCommands) SendMessageWithRequestID(context.Context, string, string, string) (agent.TurnResult, error) {
	return c.result, errors.New("internal provider configuration error")
}

type localAssetCommands struct {
	*fakeCommands
	received chan []model.LocalAssetRef
}

func (c localAssetCommands) SendMessageWithRequestIDAndAllAssets(_ context.Context, _ string, _ string, _ string, _ string, _ []model.GatewayAssetRef, localAssets []model.LocalAssetRef) (agent.TurnResult, error) {
	c.received <- localAssets
	return agent.TurnResult{}, nil
}

type testInputError struct{}

func (testInputError) Error() string               { return "conflict" }
func (testInputError) AgentInputErrorCode() string { return "request_conflict" }
func (testInputError) SafeMessage() string {
	return "request_id was already used with a different payload"
}

func TestCanonicalUserAssetFixturesValidate(t *testing.T) {
	root := filepath.Join("..", "..", "docs", "protocols", "fixtures", "user-assets")
	for _, name := range []string{"agent_input_text_with_image.json", "agent_input_image_only.json", "agent_input_two_images.json"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		input, rejected := decodeAndValidateAgentInput(data, true)
		if rejected != nil {
			t.Fatalf("%s rejected: %+v", name, rejected)
		}
		if len(input.Assets) == 0 || input.RequestID == "" {
			t.Fatalf("%s decoded incompletely: %+v", name, input)
		}
	}
}

func TestUserAssetValidationRejectsFrozenInvalidShapes(t *testing.T) {
	tests := []struct{ name, payload, code string }{
		{"missing request", `{"type":"agent_input","kind":"text","assets":[{"asset_id":1,"kind":"image","mime_type":"image/png","filename":"a.png"}]}`, "invalid_input"},
		{"unsupported capability", `{"type":"agent_input","kind":"text","request_id":"r","assets":[{"asset_id":1,"kind":"image","mime_type":"image/png","filename":"a.png"}]}`, "image_input_unsupported"},
		{"forbidden url", `{"type":"agent_input","kind":"text","request_id":"r","assets":[{"asset_id":1,"kind":"image","mime_type":"image/png","filename":"a.png","url":"https://secret"}]}`, "invalid_assets"},
		{"forbidden bytes", `{"type":"agent_input","kind":"text","request_id":"r-bytes","assets":[{"asset_id":1,"kind":"image","mime_type":"image/png","filename":"a.png","bytes":"iVBORw0KGgo="}]}`, "invalid_assets"},
		{"duplicate", `{"type":"agent_input","kind":"text","request_id":"r","assets":[{"asset_id":1,"kind":"image","mime_type":"image/png","filename":"a.png"},{"asset_id":1,"kind":"image","mime_type":"image/png","filename":"b.png"}]}`, "invalid_assets"},
		{"bad sha", `{"type":"agent_input","kind":"text","request_id":"r","assets":[{"asset_id":1,"sha256":"ABC","kind":"image","mime_type":"image/png","filename":"a.png"}]}`, "invalid_assets"},
		{"bad filename", `{"type":"agent_input","kind":"text","request_id":"r","assets":[{"asset_id":1,"kind":"image","mime_type":"image/png","filename":"../a.png"}]}`, "invalid_assets"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			imageInput := tc.name != "unsupported capability"
			_, rejected := decodeAndValidateAgentInput([]byte(tc.payload), imageInput)
			if rejected == nil || rejected.Error.Code != tc.code {
				t.Fatalf("rejected=%+v want %s", rejected, tc.code)
			}
		})
	}
}

func TestToWireKeepsUserAssetsSeparateFromToolAssets(t *testing.T) {
	w := toWire(agent.Event{Kind: agent.EventTurnStarted, UserAssets: []model.GatewayAssetRef{{AssetID: 7, Kind: "image", MIMEType: "image/png", Filename: "a.png"}}})
	if len(w.UserAssets) != 1 || w.UserAssets[0].AssetID != 7 || len(w.Assets) != 0 {
		t.Fatalf("wire assets mixed: %+v", w)
	}
}

func TestRouterReturnsNonPersistentInputRejections(t *testing.T) {
	rejections := rejectionRecorder{ch: make(chan AgentInputRejected, 2)}
	router := Router{Commands: conflictCommands{newFakeCommands()}, Rejections: rejections, ImageInput: true}
	router.Route(context.Background(), []byte(`{"type":"agent_input","kind":"text","request_id":"r","assets":[{"asset_id":1,"kind":"image","mime_type":"image/gif","filename":"a.gif"}]}`))
	first := <-rejections.ch
	if first.Error.Code != "invalid_assets" || first.RequestID != "r" {
		t.Fatalf("first=%+v", first)
	}
	router.Route(context.Background(), []byte(`{"type":"agent_input","kind":"text","request_id":"r","assets":[{"asset_id":1,"kind":"image","mime_type":"image/png","filename":"a.png"}]}`))
	select {
	case second := <-rejections.ch:
		if second.Error.Code != "request_conflict" {
			t.Fatalf("second=%+v", second)
		}
	case <-time.After(time.Second):
		t.Fatal("request conflict was not returned")
	}
}

func TestRouterTerminatesEveryPreAcceptFailureWithoutRejectingAcceptedTurns(t *testing.T) {
	rejections := rejectionRecorder{ch: make(chan AgentInputRejected, 2)}
	payload := []byte(`{"type":"agent_input","kind":"text","request_id":"pre-accept","text":"hello"}`)

	router := Router{
		Commands:   failingRequestCommands{fakeCommands: newFakeCommands()},
		Rejections: rejections,
	}
	router.Route(context.Background(), payload)
	select {
	case rejected := <-rejections.ch:
		if rejected.RequestID != "pre-accept" || rejected.Error.Code != "request_failed" {
			t.Fatalf("pre-accept rejection = %+v", rejected)
		}
	case <-time.After(time.Second):
		t.Fatal("ordinary pre-accept failure was silently dropped")
	}

	router.Commands = failingRequestCommands{
		fakeCommands: newFakeCommands(),
		result:       agent.TurnResult{TurnID: "accepted-turn"},
	}
	router.Route(context.Background(), payload)
	select {
	case rejected := <-rejections.ch:
		t.Fatalf("accepted turn must use lifecycle failure, got rejection %+v", rejected)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestLocalAssetsValidateWithoutImageCapabilityAndAllowEmptyText(t *testing.T) {
	payload := `{"type":"agent_input","kind":"text","request_id":"local-1","local_assets":[{"id":"a-1","relative_path":"user-assets/a-1/report.pdf","filename":"report.pdf","mime_type":"application/pdf","kind":"pdf","size_bytes":42,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","transfer_policy":"local_only"}]}`
	input, rejected := decodeAndValidateAgentInput([]byte(payload), false)
	if rejected != nil {
		t.Fatalf("local-only input rejected without image_input: %+v", rejected)
	}
	if len(input.LocalAssets) != 1 || input.LocalAssets[0].RelativePath != "user-assets/a-1/report.pdf" {
		t.Fatalf("decoded local assets = %+v", input.LocalAssets)
	}
}

func TestRouterDispatchesLocalAssetsWithoutImageCapability(t *testing.T) {
	commands := localAssetCommands{fakeCommands: newFakeCommands(), received: make(chan []model.LocalAssetRef, 1)}
	router := Router{Commands: commands, ImageInput: false}
	router.Route(context.Background(), []byte(`{"type":"agent_input","kind":"text","request_id":"local-1","local_assets":[{"id":"a-1","relative_path":"user-assets/a-1/report.pdf","filename":"report.pdf","mime_type":"application/pdf","kind":"pdf","size_bytes":42,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","transfer_policy":"local_only"}]}`))
	select {
	case localAssets := <-commands.received:
		if len(localAssets) != 1 || localAssets[0].ID != "a-1" {
			t.Fatalf("routed local assets = %+v", localAssets)
		}
	case <-time.After(time.Second):
		t.Fatal("local asset input was not dispatched")
	}
}

func TestRouterRejectsLocalAssetsWhenTargetDoesNotSupportThem(t *testing.T) {
	commands := newFakeCommands()
	rejections := rejectionRecorder{ch: make(chan AgentInputRejected, 1)}
	router := Router{Commands: commands, Rejections: rejections, ImageInput: false}
	router.Route(context.Background(), []byte(`{"type":"agent_input","kind":"text","request_id":"local-unsupported","text":"inspect","local_assets":[{"id":"a-1","relative_path":"user-assets/a-1/report.pdf","filename":"report.pdf","mime_type":"application/pdf","kind":"pdf","size_bytes":42,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","transfer_policy":"local_only"}]}`))
	select {
	case rejected := <-rejections.ch:
		if rejected.RequestID != "local-unsupported" || rejected.Error.Code != "local_assets_unsupported" {
			t.Fatalf("rejected = %+v", rejected)
		}
	case text := <-commands.text:
		t.Fatalf("local assets were silently downgraded to text %q", text)
	case <-time.After(time.Second):
		t.Fatal("unsupported local assets produced no rejection")
	}
}

func TestLocalAssetValidationRejectsUnsafeShapes(t *testing.T) {
	valid := `"id":"a","relative_path":"user-assets/a/file.pdf","filename":"file.pdf","mime_type":"application/pdf","kind":"pdf","size_bytes":1,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","transfer_policy":"local_only"`
	tests := []string{
		`"id":"a","relative_path":"/tmp/file.pdf","filename":"file.pdf","mime_type":"application/pdf","kind":"pdf","size_bytes":1,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","transfer_policy":"local_only"`,
		`"id":"a","relative_path":"user-assets/../file.pdf","filename":"file.pdf","mime_type":"application/pdf","kind":"pdf","size_bytes":1,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","transfer_policy":"local_only"`,
		`"id":"a","relative_path":"user-assets\\file.pdf","filename":"file.pdf","mime_type":"application/pdf","kind":"pdf","size_bytes":1,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","transfer_policy":"local_only"`,
		`"id":"a","relative_path":"https://example.test/file.pdf","filename":"file.pdf","mime_type":"application/pdf","kind":"pdf","size_bytes":1,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","transfer_policy":"local_only"`,
		`"id":"a","relative_path":"C:/Users/file.pdf","filename":"file.pdf","mime_type":"application/pdf","kind":"pdf","size_bytes":1,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","transfer_policy":"local_only"`,
		`"id":"a","relative_path":"user-assets/a/file.pdf","filename":"file.pdf","mime_type":"application/pdf","kind":"pdf","size_bytes":1,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","transfer_policy":"upload"`,
		valid + `,"bytes":"secret"`,
		valid + `,"future_bytes":"secret"`,
	}
	for i, object := range tests {
		payload := `{"type":"agent_input","kind":"text","request_id":"r","local_assets":[{` + object + `}]}`
		_, rejected := decodeAndValidateAgentInput([]byte(payload), false)
		if rejected == nil || rejected.Error.Code != "invalid_local_assets" {
			t.Fatalf("case %d rejected=%+v", i, rejected)
		}
	}
}

func TestUploadedAndLocalCopyCannotBeSentTogether(t *testing.T) {
	payload := `{"type":"agent_input","kind":"text","request_id":"r","assets":[{"asset_id":1,"kind":"image","mime_type":"image/png","filename":"a.png","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],"local_assets":[{"id":"a","relative_path":"user-assets/a/a.png","filename":"a.png","mime_type":"image/png","kind":"image","size_bytes":1,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","transfer_policy":"local_only"}]}`
	_, rejected := decodeAndValidateAgentInput([]byte(payload), true)
	if rejected == nil || rejected.Error.Code != "invalid_assets" {
		t.Fatalf("rejected=%+v", rejected)
	}
}

func TestMixedAssetsRequireGatewaySHA256(t *testing.T) {
	payload := `{"type":"agent_input","kind":"text","request_id":"r","assets":[{"asset_id":1,"kind":"image","mime_type":"image/png","filename":"cloud.png"}],"local_assets":[{"id":"a","relative_path":"user-assets/a/local.pdf","filename":"local.pdf","mime_type":"application/pdf","kind":"pdf","size_bytes":1,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","transfer_policy":"local_only"}]}`
	_, rejected := decodeAndValidateAgentInput([]byte(payload), true)
	if rejected == nil || rejected.Error.Code != "invalid_assets" {
		t.Fatalf("rejected=%+v", rejected)
	}
}

func TestMixedAssetsWithDistinctSHA256Validate(t *testing.T) {
	payload := `{"type":"agent_input","kind":"text","request_id":"r","assets":[{"asset_id":1,"kind":"image","mime_type":"image/png","filename":"cloud.png","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}],"local_assets":[{"id":"a","relative_path":"user-assets/a/local.pdf","filename":"local.pdf","mime_type":"application/pdf","kind":"pdf","size_bytes":1,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","transfer_policy":"local_only"}]}`
	input, rejected := decodeAndValidateAgentInput([]byte(payload), true)
	if rejected != nil || len(input.Assets) != 1 || len(input.LocalAssets) != 1 {
		t.Fatalf("input=%+v rejected=%+v", input, rejected)
	}
}

func TestToWireIncludesLocalAssetsOnTurnStarted(t *testing.T) {
	local := model.LocalAssetRef{ID: "a", RelativePath: "user-assets/a/file.txt", Filename: "file.txt", MIMEType: "text/plain", Kind: "document", SizeBytes: 3, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TransferPolicy: "local_only"}
	w := toWire(agent.Event{Kind: agent.EventTurnStarted, LocalAssets: []model.LocalAssetRef{local}})
	if len(w.LocalAssets) != 1 || w.LocalAssets[0].ID != "a" {
		t.Fatalf("wire local assets = %+v", w.LocalAssets)
	}
}
