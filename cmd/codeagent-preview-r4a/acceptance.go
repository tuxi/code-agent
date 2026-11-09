package main

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/model"
	"code-agent/internal/preview"
	agentruntime "code-agent/internal/runtime"
	"code-agent/internal/settings"
	"code-agent/internal/tools"
	previewtool "code-agent/internal/tools/preview"
)

type AcceptanceOptions struct {
	Workspace string
	StateDir  string
	ViewerDir string
	Open      bool
	Output    io.Writer
}

type AcceptanceSummary struct {
	Workspace       string `json:"workspace"`
	StateDir        string `json:"state_dir"`
	SessionID       string `json:"session_id"`
	TurnID          string `json:"turn_id"`
	Capability      string `json:"capability"`
	ProducerID      string `json:"producer_id"`
	ArtifactID      string `json:"artifact_id"`
	Revision        int64  `json:"revision"`
	RevisionSHA256  string `json:"revision_sha256"`
	EventSeq        int64  `json:"event_seq"`
	Status          string `json:"status"`
	ViewerURL       string `json:"viewer_url"`
	AssetURL        string `json:"asset_url"`
	ViewerPath      string `json:"viewer_path"`
	ViewerSHA256    string `json:"viewer_sha256"`
	FallbackAssetID string `json:"fallback_asset_id"`
	FinalAnswer     string `json:"final_answer"`
}

type alwaysAllowApprover struct{}

func (alwaysAllowApprover) Approve(string, json.RawMessage) agent.Verdict { return agent.VerdictAllow }

type acceptanceBuilder struct {
	workspace string
	cfg       settings.Settings
	mc        settings.ModelConfig
	provider  model.Provider
	service   *preview.Service
}

func (b *acceptanceBuilder) Build(rc conversation.RuntimeContext) conversation.TurnRunner {
	reg := tools.NewRegistry()
	_ = reg.Register(previewtool.NewTool())
	runner := agentruntime.BuildRunner(b.cfg, b.mc, b.provider, reg, nil, alwaysAllowApprover{}, rc.Publisher, nil, b.workspace)
	runner.PreviewProducer = b.service
	runner.ReservedTurnID = rc.TurnID
	runner.RequestID = rc.RequestID
	runner.WorkspaceRoot = b.workspace
	return runner
}

func runAcceptance(ctx context.Context, opts AcceptanceOptions) (AcceptanceSummary, error) {
	if opts.Output == nil {
		opts.Output = io.Discard
	}
	workspace, cleanupWorkspace, err := tempDirIfEmpty(opts.Workspace, "codeagent-r4a-workspace-")
	if err != nil {
		return AcceptanceSummary{}, err
	}
	defer cleanupWorkspace()
	stateDir, cleanupState, err := tempDirIfEmpty(opts.StateDir, "codeagent-r4a-state-")
	if err != nil {
		return AcceptanceSummary{}, err
	}
	defer cleanupState()
	viewerDir, cleanupViewer, err := tempDirIfEmpty(opts.ViewerDir, "codeagent-r4a-viewer-")
	if err != nil {
		return AcceptanceSummary{}, err
	}
	defer cleanupViewer()
	agentruntime.SetStoreBaseDir(stateDir)

	if err := writeFallbackPNG(filepath.Join(workspace, "assets", "fallback.png")); err != nil {
		return AcceptanceSummary{}, fmt.Errorf("write fallback: %w", err)
	}
	store, err := agentruntime.OpenStore(workspace)
	if err != nil {
		return AcceptanceSummary{}, err
	}
	defer store.Close()
	service, err := agentruntime.OpenPreviewService(workspace)
	if err != nil {
		return AcceptanceSummary{}, err
	}

	mc := settings.ModelConfig{Name: "r4a-scripted", Provider: "responses", Model: "r4a-scripted", ContextWindow: 128000}
	cfg := settings.Settings{Agent: settings.AgentConfig{MaxSteps: 4, MaxParallelTools: 1}}
	provider := &agent.FauxProvider{Steps: []agent.FauxStep{
		agent.FauxTool("ui_preview", map[string]any{
			"artifact_key":           "ui-adaptation:r4a-backend-turn",
			"title":                  "R4a Backend Preview",
			"summary":                "A real preview artifact produced by ui_preview inside a durable agent turn.",
			"viewer_html":            r4aViewerHTML,
			"fallback_path":          "assets/fallback.png",
			"fallback_text":          "Interactive preview unavailable. Showing generated static evidence.",
			"alt":                    "R4a generated fallback",
			"preferred_aspect_ratio": 1.5,
			"min_height":             360,
		}),
		agent.FauxText("R4a backend turn completed. The preview artifact was persisted and verified through the in-process viewer resolver."),
	}}
	repo := conversation.NewSQLiteRepository(store, mc.ContextWindow, cfg.CompactThreshold(mc), mc.Model, "", func(string) string { return "" })
	eventStore := &conversation.StoreEventAdapter{Store: store}
	active := conversation.NewActiveTurnRegistry()
	subs := conversation.NewSubscriptionManager()
	builder := &acceptanceBuilder{workspace: workspace, cfg: cfg, mc: mc, provider: provider, service: service}
	executor := conversation.NewTurnExecutor(repo, eventStore, active, subs, builder)
	executor.SetTurnScheduler(conversation.NewTurnScheduler(1))
	defer executor.Shutdown()
	sess, err := repo.Create(ctx, workspace, "")
	if err != nil {
		return AcceptanceSummary{}, err
	}
	sess.Model = mc.Model
	sess.ContextWindow = mc.ContextWindow
	sess.CompactThreshold = cfg.CompactThreshold(mc)

	events, unsubscribe := subs.Subscribe(sess.ID)
	defer unsubscribe()
	fmt.Fprintf(opts.Output, "workspace=%s\nstate_dir=%s\nsession_id=%s\n", workspace, stateDir, sess.ID)
	res, err := executor.ExecuteWithSession(ctx, sess, "Generate and persist the R4a interactive preview through ui_preview.", "")
	if err != nil {
		return AcceptanceSummary{}, err
	}
	if res.TurnID == "" {
		return AcceptanceSummary{}, fmt.Errorf("backend turn returned no turn_id")
	}

	var liveEvent agent.Event
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for liveEvent.Artifact == nil {
		select {
		case event, ok := <-events:
			if !ok {
				return AcceptanceSummary{}, fmt.Errorf("live event subscription closed before preview_artifact")
			}
			if event.Kind == agent.EventPreviewArtifact {
				liveEvent = event
				fmt.Fprintf(opts.Output, "[preview] %s revision=%d seq=%d\n", event.Artifact.Title, event.Artifact.Revision, event.Seq)
			}
		case <-deadline.C:
			return AcceptanceSummary{}, fmt.Errorf("timed out waiting for live preview_artifact event")
		}
	}
	if err := preview.ValidateEventCorrelation(preview.Event{
		Kind: "preview_artifact", EventID: "live", Seq: liveEvent.Seq,
		SessionID: liveEvent.SessionID, TurnID: liveEvent.TurnID, CallID: liveEvent.CallID, Artifact: liveEvent.Artifact,
	}); err != nil {
		return AcceptanceSummary{}, err
	}

	replayed, replayedSeq, err := replayPreviewEvent(ctx, eventStore, sess.ID, liveEvent.Artifact.ArtifactID)
	if err != nil {
		return AcceptanceSummary{}, err
	}
	if replayed.RevisionSHA256 != liveEvent.Artifact.RevisionSHA256 || replayed.Revision != liveEvent.Artifact.Revision {
		return AcceptanceSummary{}, fmt.Errorf("live/replayed preview mismatch")
	}
	artifact := replayed
	viewer, err := service.ResolveViewer(ctx, sess.ID, artifact.ArtifactID, artifact.Revision)
	if err != nil {
		return AcceptanceSummary{}, err
	}
	if artifact.Fallback == nil {
		return AcceptanceSummary{}, fmt.Errorf("preview artifact has no fallback")
	}
	asset, err := service.ResolveAsset(ctx, sess.ID, artifact.ArtifactID, artifact.Revision, artifact.Fallback.AssetID)
	if err != nil {
		return AcceptanceSummary{}, err
	}
	viewerPath, err := service.MaterializePrivateViewer(ctx, viewerDir, sess.ID, artifact.ArtifactID, artifact.Revision)
	if err != nil {
		return AcceptanceSummary{}, err
	}
	if opts.Open {
		if err := openTrustedViewer(viewerPath); err != nil {
			return AcceptanceSummary{}, err
		}
		fmt.Fprintf(opts.Output, "opened=%s\n", viewerPath)
	}
	assetURL := preview.AssetURL(sess.ID, artifact.ArtifactID, artifact.Revision, artifact.Fallback.AssetID)
	fmt.Fprintf(opts.Output, "viewer_verified bytes=%d sha256=%s\nasset_verified bytes=%d sha256=%s\n", viewer.SizeBytes, viewer.SHA256, asset.SizeBytes, asset.SHA256)
	return AcceptanceSummary{
		Workspace: workspace, StateDir: stateDir, SessionID: sess.ID, TurnID: res.TurnID,
		Capability: artifact.Capability, ProducerID: artifact.ProducerID,
		ArtifactID: artifact.ArtifactID, Revision: artifact.Revision, RevisionSHA256: artifact.RevisionSHA256,
		EventSeq: replayedSeq, Status: string(artifact.Status), ViewerURL: preview.ViewerURL(sess.ID, artifact.ArtifactID, artifact.Revision),
		AssetURL: assetURL, ViewerPath: viewerPath, ViewerSHA256: viewer.SHA256,
		FallbackAssetID: artifact.Fallback.AssetID, FinalAnswer: res.Final,
	}, nil
}

func replayPreviewEvent(ctx context.Context, store conversation.ConversationEventStore, sessionID, artifactID string) (preview.Artifact, int64, error) {
	records, err := store.Replay(ctx, sessionID)
	if err != nil {
		return preview.Artifact{}, 0, err
	}
	for _, record := range records {
		var event agent.Event
		if err := json.Unmarshal(record.Payload, &event); err != nil {
			continue
		}
		if event.Kind != agent.EventPreviewArtifact || event.Artifact == nil || event.Artifact.ArtifactID != artifactID {
			continue
		}
		event.Seq = record.Seq
		frame, err := encodePreviewEvent(event)
		if err != nil {
			return preview.Artifact{}, 0, err
		}
		wireEvent, err := preview.DecodeEvent(frame)
		if err != nil {
			return preview.Artifact{}, 0, err
		}
		if err := preview.ValidateEventCorrelation(wireEvent); err != nil {
			return preview.Artifact{}, 0, err
		}
		return *wireEvent.Artifact, record.Seq, nil
	}
	return preview.Artifact{}, 0, fmt.Errorf("preview artifact %s was not replayed", artifactID)
}

// encodePreviewEvent isolates the command from server package internals while
// preserving the canonical wire fields consumed by preview.DecodeEvent.
func encodePreviewEvent(event agent.Event) ([]byte, error) {
	return json.Marshal(map[string]any{
		"kind": "preview_artifact", "event_id": "replay", "seq": event.Seq,
		"session_id": event.SessionID, "turn_id": event.TurnID, "call_id": event.CallID,
		"artifact": event.Artifact,
	})
}

func tempDirIfEmpty(value, pattern string) (string, func(), error) {
	if strings.TrimSpace(value) != "" {
		abs, err := filepath.Abs(value)
		if err != nil {
			return "", func() {}, err
		}
		if err := os.MkdirAll(abs, 0o700); err != nil {
			return "", func() {}, err
		}
		return abs, func() {}, nil
	}
	dir, err := os.MkdirTemp("", pattern)
	if err != nil {
		return "", func() {}, err
	}
	return dir, func() {}, nil
}

func writeFallbackPNG(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	img := image.NewRGBA(image.Rect(0, 0, 1200, 800))
	for y := 0; y < 800; y++ {
		for x := 0; x < 1200; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8(32 + x*180/1200),
				G: uint8(48 + y*160/800),
				B: uint8(96 + (x+y)*80/2000),
				A: 255,
			})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func openTrustedViewer(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "linux":
		cmd = exec.Command("xdg-open", path)
	default:
		return fmt.Errorf("trusted viewer opening is not supported on %s", runtime.GOOS)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open trusted viewer: %w", err)
	}
	return nil
}

func writeSummary(w io.Writer, summary AcceptanceSummary) error {
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}

const r4aViewerHTML = `<main style="font:16px -apple-system,sans-serif;padding:24px;max-width:900px;margin:auto">
  <h1>R4a Backend Preview</h1>
  <p>This document was materialized by the CodeAgent preview runtime.</p>
  <div style="display:flex;gap:12px;margin:24px 0">
    <button id="outer">Outer screen</button>
    <button id="portrait">Inner portrait</button>
    <button id="landscape">Inner landscape</button>
  </div>
  <section id="stage" style="aspect-ratio:3/2;border-radius:20px;background:linear-gradient(135deg,#dbeafe,#fce7f3);display:grid;place-items:center;font-weight:600">Outer screen</section>
  <script>
    const stage = document.getElementById('stage');
    const labels = {outer:'Outer screen',portrait:'Inner portrait',landscape:'Inner landscape'};
    for (const [id,label] of Object.entries(labels)) {
      document.getElementById(id).addEventListener('click', () => { stage.textContent = label; });
    }
  </script>
</main>`
