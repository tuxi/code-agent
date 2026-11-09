package tui

import (
	"strings"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/preview"
)

func TestPreviewArtifactRendersCLIEntryAndTimelineItem(t *testing.T) {
	artifact := preview.Artifact{
		ArtifactID: "preview_test", Revision: 2, Status: preview.StatusReady,
		Kind: preview.KindInteractiveHTML, Title: "Preview title", Summary: "Preview summary",
	}
	event := agent.Event{Kind: agent.EventPreviewArtifact, Artifact: &artifact, At: time.Now()}

	var conversation Conversation
	messages := conversation.Apply(event)
	if len(messages) != 1 || !strings.Contains(messages[0].Content, "Preview title") || !strings.Contains(messages[0].Content, "revision 2") {
		t.Fatalf("conversation preview entry = %+v", messages)
	}
	var timeline Timeline
	timeline.Apply(event)
	if len(timeline.Items) != 1 || timeline.Items[0].Kind != ItemPreview || timeline.Items[0].Name != "Preview title" {
		t.Fatalf("timeline preview item = %+v", timeline.Items)
	}
	lines := renderEntry(timeline.Items[0], 80)
	if len(lines) == 0 || !strings.Contains(lines[0], "preview") || !strings.Contains(lines[0], "ready") {
		t.Fatalf("rendered preview entry = %v", lines)
	}
}

type fakeViewerResolver struct{ calls int }

func (f *fakeViewerResolver) Resolve(string, string, int64) (preview.Resource, error) {
	f.calls++
	return preview.Resource{}, nil
}

type fakeAssetResolver struct{ calls int }

func (f *fakeAssetResolver) ResolveContent(string, string, int64, string) (preview.Resource, error) {
	f.calls++
	return preview.Resource{}, nil
}

func TestPreviewEntryUsesInProcessResolvers(t *testing.T) {
	artifact := preview.Artifact{
		ArtifactID: "preview_test", SessionID: "session_test", Revision: 1,
		Status: preview.StatusReady, Kind: preview.KindInteractiveHTML, Title: "Verified",
		Fallback: &preview.Fallback{AssetID: "asset_fallback", Text: "fallback"},
	}
	viewer := &fakeViewerResolver{}
	asset := &fakeAssetResolver{}
	conversation := Conversation{ViewerResolver: viewer, AssetResolver: asset}
	messages := conversation.Apply(agent.Event{Kind: agent.EventPreviewArtifact, Artifact: &artifact})
	if viewer.calls != 1 || asset.calls != 1 {
		t.Fatalf("resolver calls = viewer %d asset %d", viewer.calls, asset.calls)
	}
	if len(messages) != 1 || !strings.Contains(messages[0].Content, "viewer verified") {
		t.Fatalf("preview entry = %+v", messages)
	}
}

func TestPreviewRevisionReplacesExistingBlock(t *testing.T) {
	var conversation Conversation
	first := preview.Artifact{ArtifactID: "preview_same", Revision: 1, Status: preview.StatusPending, Kind: preview.KindInteractiveHTML, Title: "First"}
	second := preview.Artifact{ArtifactID: "preview_same", Revision: 2, Status: preview.StatusReady, Kind: preview.KindInteractiveHTML, Title: "Second"}
	one := conversation.Apply(agent.Event{Kind: agent.EventPreviewArtifact, Artifact: &first})
	two := conversation.Apply(agent.Event{Kind: agent.EventPreviewArtifact, Artifact: &second})
	if len(one) != 1 || len(two) != 1 || one[0].ID != two[0].ID || !strings.Contains(two[0].Content, "Second") {
		t.Fatalf("preview replacement = first %+v second %+v", one, two)
	}
	if len(conversation.Messages()) != 1 {
		t.Fatalf("preview revision created a duplicate transcript block: %d", len(conversation.Messages()))
	}
	var timeline Timeline
	timeline.Apply(agent.Event{Kind: agent.EventPreviewArtifact, Artifact: &first})
	timeline.Apply(agent.Event{Kind: agent.EventPreviewArtifact, Artifact: &second})
	if len(timeline.Items) != 1 || timeline.Items[0].ArtifactRevision != 2 || timeline.Items[0].Name != "Second" {
		t.Fatalf("timeline preview replacement = %+v", timeline.Items)
	}
}
