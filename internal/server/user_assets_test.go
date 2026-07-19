package server

import (
	"context"
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

func (c conflictCommands) SendMessageWithRequestIDAndAssets(context.Context, string, string, string, []model.GatewayAssetRef) (agent.TurnResult, error) {
	return agent.TurnResult{}, testInputError{}
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
