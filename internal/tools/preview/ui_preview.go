// Package previewtool exposes the built-in ui_preview producer. It converts
// model-authored viewer fragments and a workspace raster fallback into the
// typed ToolResult preview side-channel; the agent loop remains tool-agnostic.
package previewtool

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"

	"code-agent/internal/assetref"
	"code-agent/internal/preview"
	"code-agent/internal/tools"
	"code-agent/internal/workspace"
)

const Capability = "ui-adaptation"

type Tool struct{}

func NewTool() *Tool { return &Tool{} }

func (*Tool) Name() string { return "ui_preview" }

func (*Tool) Description() string {
	return "Create an interactive UI preview from a trusted HTML fragment and a workspace raster fallback. Use this instead of pasting preview HTML into ordinary output."
}

func (*Tool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"artifact_key": {
			Type:        "string",
			Description: "Stable key unique within this capability producer.",
		},
		"title": {
			Type:        "string",
			Description: "Human-readable preview title.",
		},
		"summary": {
			Type:        "string",
			Description: "Short preview summary.",
		},
		"viewer_html": {
			Type:        "string",
			Description: "Self-contained HTML fragment. External resources, navigation, network access, forms, and bridge calls are forbidden.",
		},
		"fallback_path": {
			Type:        "string",
			Description: "Workspace-relative PNG, JPEG, or WebP fallback image.",
		},
		"fallback_text": {
			Type:        "string",
			Description: "Fallback description displayed when interactive rendering is unavailable.",
		},
		"alt": {
			Type:        "string",
			Description: "Alternative text for the fallback image.",
		},
		"preferred_aspect_ratio": {
			Type:        "number",
			Description: "Preferred viewer aspect ratio.",
		},
		"min_height": {
			Type:        "integer",
			Description: "Preferred minimum viewer height in points.",
		},
	}, "artifact_key", "title", "viewer_html", "fallback_path").Closed().JSON()
}

func (*Tool) ToolBinding() tools.Binding {
	return tools.Binding{
		Name:            "ui_preview",
		ModelVisibility: tools.VisibilityDirect,
		Executor:        tools.ExecutorServer,
		Capability:      Capability,
	}
}

type input struct {
	ArtifactKey          string  `json:"artifact_key"`
	Title                string  `json:"title"`
	Summary              string  `json:"summary"`
	ViewerHTML           string  `json:"viewer_html"`
	FallbackPath         string  `json:"fallback_path"`
	FallbackText         string  `json:"fallback_text"`
	Alt                  string  `json:"alt"`
	PreferredAspectRatio float64 `json:"preferred_aspect_ratio"`
	MinHeight            int     `json:"min_height"`
}

func (t *Tool) Execute(ctx context.Context, ec tools.ExecutionContext, raw json.RawMessage) (tools.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return tools.ToolResult{}, err
	}
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		return tools.FailureResult("invalid_arguments", "ui_preview input is invalid: "+err.Error()), nil
	}
	if strings.TrimSpace(in.ArtifactKey) == "" || strings.TrimSpace(in.Title) == "" {
		return tools.FailureResult("invalid_arguments", "ui_preview requires artifact_key and title"), nil
	}
	if strings.TrimSpace(in.ViewerHTML) == "" {
		return tools.FailureResult("invalid_arguments", "ui_preview requires viewer_html"), nil
	}
	if strings.TrimSpace(ec.WorkspaceRoot) == "" {
		return tools.FailureResult("unavailable", "ui_preview requires a workspace root"), nil
	}
	root, err := filepath.Abs(ec.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{}, err
	}
	target := workspace.ResolveToolPath(root, in.FallbackPath)
	target, err = filepath.Abs(target)
	if err != nil {
		return tools.ToolResult{}, err
	}
	switch workspace.ClassifyPath(root, target) {
	case workspace.PathOutsideWorkspace:
		return tools.FailureResult("blocked", "ui_preview fallback_path must stay inside the workspace"), nil
	case workspace.PathManagedWorktree:
		return tools.FailureResult("blocked", "ui_preview cannot read from the managed worktree tree"), nil
	}
	info, err := os.Stat(target)
	if err != nil {
		return tools.FailureResult("not_found", "ui_preview fallback image was not found: "+in.FallbackPath), nil
	}
	if !info.Mode().IsRegular() {
		return tools.FailureResult("invalid_arguments", "ui_preview fallback_path must be a regular file"), nil
	}
	mimeType := assets.MIMEType(target)
	switch strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0])) {
	case "image/png", "image/jpeg", "image/webp":
	default:
		return tools.FailureResult("invalid_arguments", "ui_preview fallback must be PNG, JPEG, or WebP"), nil
	}
	file, err := os.Open(target)
	if err != nil {
		return tools.ToolResult{}, err
	}
	width, height := 0, 0
	if config, _, decodeErr := image.DecodeConfig(file); decodeErr == nil {
		width, height = config.Width, config.Height
	}
	_ = file.Close()
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return tools.FailureResult("blocked", "ui_preview fallback_path could not be made workspace-relative"), nil
	}
	rel = filepath.ToSlash(rel)
	assetID := assets.StableID(ec.TurnID, ec.CallID, 0, "ui_preview", rel)
	fallbackText := strings.TrimSpace(in.FallbackText)
	if fallbackText == "" {
		fallbackText = "Interactive preview unavailable. Showing static evidence."
	}
	alt := strings.TrimSpace(in.Alt)
	if alt == "" {
		alt = in.Title
	}
	var presentation *preview.PresentationHint
	if in.PreferredAspectRatio > 0 || in.MinHeight > 0 {
		presentation = &preview.PresentationHint{
			Placement:            "conversation_inline",
			PreferredAspectRatio: in.PreferredAspectRatio,
			MinHeight:            in.MinHeight,
			InitialState:         "expanded",
		}
	}
	draft := preview.Draft{
		ArtifactKey: strings.TrimSpace(in.ArtifactKey),
		Kind:        preview.KindInteractiveHTML,
		Title:       strings.TrimSpace(in.Title),
		Summary:     strings.TrimSpace(in.Summary),
		ViewerHTML:  in.ViewerHTML,
		Assets: []preview.AssetDraft{{
			AssetID:  assetID,
			Role:     preview.RoleFallback,
			MIMEType: mimeType,
			Alt:      alt,
			Width:    width,
			Height:   height,
		}},
		Fallback: &preview.FallbackDraft{
			AssetID: assetID,
			Text:    fallbackText,
		},
		Presentation: presentation,
	}
	ref := assets.Ref{
		ID:                    assetID,
		Kind:                  "image",
		DisplayName:           filepath.Base(target),
		WorkspaceID:           assets.WorkspaceID(root),
		WorkspaceRelativePath: rel,
		MIMEType:              mimeType,
		SourceTurnID:          ec.TurnID,
		SourceCallID:          ec.CallID,
	}
	output, _ := json.Marshal(map[string]any{
		"artifact_key": draft.ArtifactKey,
		"kind":         draft.Kind,
		"title":        draft.Title,
		"fallback":     ref,
	})
	return tools.ToolResult{
		Content:  fmt.Sprintf("Prepared interactive preview %q.", draft.Title),
		Output:   output,
		Assets:   []assets.Ref{ref},
		Previews: []preview.Draft{draft},
	}, nil
}

var (
	_ tools.Tool            = (*Tool)(nil)
	_ tools.BindingProvider = (*Tool)(nil)
)
