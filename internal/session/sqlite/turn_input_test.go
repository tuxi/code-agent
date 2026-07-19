package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"code-agent/internal/model"
	"code-agent/internal/session"
)

func TestTurnInputReserveStartAndReload(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "turn-input.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	sess := &session.Session{ID: "s", Messages: []model.Message{{Role: model.RoleSystem, Content: "sys"}}, Metadata: map[string]any{}}
	if err := store.Save(ctx, sess); err != nil {
		t.Fatal(err)
	}
	input := session.TurnInput{SessionID: "s", RequestID: "r", TurnID: "t", PayloadHash: "hash", Text: "look", WireModel: "", ResolvedModel: "gateway-model", Assets: []model.GatewayAssetRef{{AssetID: 1, Kind: "image", MIMEType: "image/png", Filename: "a.png"}}}
	event := session.EventRecord{SessionID: "s", TurnID: "t", Kind: "turn_accepted", At: time.Now().UTC(), Payload: json.RawMessage(`{"kind":"turn_accepted"}`)}
	stored, created, seq, err := store.ReserveTurnInput(ctx, input, event)
	if err != nil || !created || seq == 0 || stored.ResolvedModel != "gateway-model" {
		t.Fatalf("reserve=%+v created=%v seq=%d err=%v", stored, created, seq, err)
	}
	duplicate, created, _, err := store.ReserveTurnInput(ctx, input, event)
	if err != nil || created || duplicate.TurnID != "t" {
		t.Fatalf("duplicate=%+v created=%v err=%v", duplicate, created, err)
	}

	sess.Messages = append(sess.Messages, model.Message{Role: model.RoleUser, Content: input.Text, Assets: input.Assets, OriginTurnID: input.TurnID})
	if err := store.StartTurnInput(ctx, input, sess); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Messages[len(loaded.Messages)-1]; got.OriginTurnID != "t" || len(got.Assets) != 1 {
		t.Fatalf("message=%+v", got)
	}
	gotInput, err := store.TurnInput(ctx, "s", "r")
	if err != nil || gotInput.State != session.TurnInputRunning {
		t.Fatalf("input=%+v err=%v", gotInput, err)
	}
}

func TestAssetRefReleaseOutboxSurvivesConversationDelete(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "release.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Save(ctx, &session.Session{ID: "s", Metadata: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteWithAssetRefRelease(ctx, "s", session.AssetRefRelease{SessionID: "s", CredentialScope: "scope"}); err != nil {
		t.Fatal(err)
	}
	pending, err := store.PendingAssetRefReleases(ctx, "scope", time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].SessionID != "s" {
		t.Fatalf("pending=%+v", pending)
	}
	if err := store.CompleteAssetRefRelease(ctx, "s"); err != nil {
		t.Fatal(err)
	}
}
