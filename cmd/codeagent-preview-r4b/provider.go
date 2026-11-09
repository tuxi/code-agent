package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"code-agent/internal/model"
)

type previewTriggerProvider struct {
	prompt string
	mu     sync.Mutex
	calls  int
}

func (p *previewTriggerProvider) Complete(ctx context.Context, request model.Request) (model.Response, error) {
	if err := ctx.Err(); err != nil {
		return model.Response{}, err
	}
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()

	if len(request.Messages) > 0 && request.Messages[len(request.Messages)-1].Role == model.RoleTool {
		return model.Response{
			Content:      "R4b live preview turn completed. The preview artifact was produced by ui_preview and persisted through the normal Agent Wire event stream.",
			FinishReason: "stop",
		}, nil
	}
	prompt := lastUserText(request.Messages)
	if prompt != p.prompt {
		return model.Response{
			Content:      "R4b readiness only accepts the advertised turn_prompt. Send that exact prompt over a normal Agent Wire turn to trigger ui_preview.",
			FinishReason: "stop",
		}, nil
	}
	args, err := json.Marshal(map[string]any{
		"artifact_key":           "ui-adaptation:r4b-live-turn",
		"title":                  "R4b Live Backend Preview",
		"summary":                "A live preview artifact produced by ui_preview through a normal Runtime turn.",
		"viewer_html":            r4bViewerHTML,
		"fallback_path":          "assets/fallback.png",
		"fallback_text":          "Interactive preview unavailable. Showing generated static evidence.",
		"alt":                    "R4b generated fallback",
		"preferred_aspect_ratio": 1.5,
		"min_height":             360,
	})
	if err != nil {
		return model.Response{}, err
	}
	return model.Response{
		ToolCalls: []model.ToolCall{{
			ID: fmt.Sprintf("r4b_preview_call_%d", call), Type: "function",
			Function: model.FunctionCall{Name: "ui_preview", Arguments: string(args)},
		}},
		FinishReason: "tool_calls",
	}, nil
}

func lastUserText(messages []model.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == model.RoleUser {
			return strings.TrimSpace(messages[i].Content)
		}
	}
	return ""
}

const r4bViewerHTML = `<main style="font:16px -apple-system,sans-serif;padding:24px;max-width:900px;margin:auto">
  <h1>R4b Live Backend Preview</h1>
  <p>Produced through a normal Runtime turn and streamed over Agent Wire.</p>
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
