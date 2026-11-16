package agent

import (
	"context"

	"code-agent/internal/assetref"
	"code-agent/internal/preview"
)

// PreviewProducer is the runtime-owned typed preview materialization boundary.
// It never infers previews from tool names or ToolResult.Output.
type PreviewProducer interface {
	Produce(ctx context.Context, request preview.ProduceRequest) preview.ProduceResult
}

// PreviewStoreProvider is an optional capability of a PreviewProducer that owns
// an immutable preview store. Tools that resolve earlier revisions (such as
// ui_preview_matrix) receive that store through ExecutionContext; a producer
// without it leaves preview resolution unavailable.
type PreviewStoreProvider interface {
	PreviewStore() preview.Store
}

func previewStoreFor(producer PreviewProducer) preview.Store {
	provider, ok := producer.(PreviewStoreProvider)
	if !ok {
		return nil
	}
	return provider.PreviewStore()
}

func (r *Runner) emitPreviewArtifacts(ctx context.Context, p toolCallPlan, resultAssets []assets.Ref, previews []preview.Draft, clientOrigin bool) {
	if r.PreviewProducer == nil || len(previews) == 0 {
		return
	}
	producerID := p.binding.Name
	if producerID == "" {
		producerID = p.call.Function.Name
	}
	result := r.PreviewProducer.Produce(ctx, preview.ProduceRequest{
		SessionID:     r.emitSessionID,
		TurnID:        r.emitTurnID,
		CallID:        p.call.ID,
		WorkspaceRoot: r.WorkspaceRoot,
		ProducerID:    producerID,
		Capability:    p.binding.Capability,
		Assets:        resultAssets,
		ClientOrigin:  clientOrigin,
		Drafts:        previews,
	})
	for i := range result.Artifacts {
		artifact := result.Artifacts[i]
		r.emit(Event{
			Kind:     EventPreviewArtifact,
			CallID:   p.call.ID,
			Artifact: &artifact,
		})
	}
}
