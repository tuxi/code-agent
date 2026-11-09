package main

import (
	"bytes"
	"context"
	"os"
	"testing"

	agentruntime "code-agent/internal/runtime"
)

func TestR4ARealBackendTurnProducesAndResolvesPreview(t *testing.T) {
	oldState := agentruntime.StoreBaseDir()
	defer agentruntime.SetStoreBaseDir(oldState)
	var output bytes.Buffer
	summary, err := runAcceptance(context.Background(), AcceptanceOptions{
		Workspace: t.TempDir(), StateDir: t.TempDir(), ViewerDir: t.TempDir(), Open: false, Output: &output,
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.SessionID == "" || summary.TurnID == "" || summary.ArtifactID == "" || summary.Revision != 1 {
		t.Fatalf("summary identity = %+v", summary)
	}
	if summary.Capability != "ui-adaptation" || summary.ProducerID != "ui_preview" {
		t.Fatalf("producer identity = %s/%s", summary.Capability, summary.ProducerID)
	}
	if summary.Status != "ready" || summary.ViewerSHA256 == "" || summary.EventSeq == 0 {
		t.Fatalf("preview summary = %+v", summary)
	}
	info, err := os.Stat(summary.ViewerPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("viewer mode = %o", info.Mode().Perm())
	}
	if !bytes.Contains(output.Bytes(), []byte("[preview] R4a Backend Preview")) {
		t.Fatalf("live CLI output missing preview event:\n%s", output.String())
	}
}
