package agent

import (
	"context"
	"encoding/json"
	"testing"

	"code-agent/internal/assetref"
	"code-agent/internal/model"
	"code-agent/internal/tools"
)

type assetTool struct{}

func (assetTool) Name() string                 { return "asset_tool" }
func (assetTool) Description() string          { return "returns a file asset" }
func (assetTool) InputSchema() json.RawMessage { return tools.Object(nil).JSON() }
func (assetTool) Execute(_ context.Context, ec tools.ExecutionContext, _ json.RawMessage) (tools.ToolResult, error) {
	ref := assets.Ref{
		ID:                    "asset_from_tool",
		Kind:                  "file_location",
		WorkspaceRelativePath: "Sources/App.swift",
		Range:                 &assets.Range{StartLine: 5},
		DisplayName:           "App.swift:5",
		SourceTurnID:          ec.TurnID,
		SourceCallID:          ec.CallID,
	}
	return tools.ToolResult{
		Content: "Sources/App.swift:5: let value = 42",
		Assets:  []assets.Ref{ref},
	}, nil
}

func TestAnnotateTextWithAssets(t *testing.T) {
	text := "✅ See Sources/App.swift:5 and App.swift:5."
	refs := []assets.Ref{{
		ID:                    "asset_1",
		Kind:                  "file_location",
		WorkspaceRelativePath: "Sources/App.swift",
		Range:                 &assets.Range{StartLine: 5},
		DisplayName:           "App.swift:5",
		SourceTurnID:          "turn_1",
		SourceCallID:          "call_1",
	}}
	got := annotateTextWithAssets(text, refs)
	if len(got) != 2 {
		t.Fatalf("annotations = %d, want 2: %+v", len(got), got)
	}
	if got[0].Text != "Sources/App.swift:5" || got[0].AssetID != "asset_1" {
		t.Fatalf("first annotation = %+v", got[0])
	}
	if got[0].StartByte != len("✅ See ") {
		t.Fatalf("start_byte = %d", got[0].StartByte)
	}
	if got[0].StartUTF16 != 6 {
		t.Fatalf("start_utf16 = %d, want 6", got[0].StartUTF16)
	}
	if got[1].Text != "App.swift:5" {
		t.Fatalf("second annotation = %+v", got[1])
	}
}

func TestAnnotateDirectoryAndListedFiles(t *testing.T) {
	text := "当前工作目录下有 `AI热点速递_2026年6月28-30日.md`、`skills/`、`skills/README.md`。"
	refs := []assets.Ref{
		{
			ID:                    "asset_file",
			Kind:                  "file",
			WorkspaceRelativePath: "AI热点速递_2026年6月28-30日.md",
			DisplayName:           "AI热点速递_2026年6月28-30日.md",
		},
		{
			ID:                    "asset_dir",
			Kind:                  "directory",
			WorkspaceRelativePath: "skills",
			DisplayName:           "skills/",
		},
		{
			ID:                    "asset_readme",
			Kind:                  "file",
			WorkspaceRelativePath: "skills/README.md",
			DisplayName:           "README.md",
		},
	}
	got := annotateTextWithAssets(text, refs)
	want := map[string]string{
		"AI热点速递_2026年6月28-30日.md": "asset_file",
		"skills/":                 "asset_dir",
		"skills/README.md":        "asset_readme",
	}
	seen := map[string]bool{}
	for _, ann := range got {
		if want[ann.Text] == "" {
			continue
		}
		seen[ann.Text] = true
		if ann.AssetID != want[ann.Text] {
			t.Fatalf("annotation %q -> %q, want %q; all=%+v", ann.Text, ann.AssetID, want[ann.Text], got)
		}
	}
	for text := range want {
		if !seen[text] {
			t.Fatalf("missing annotation %q; all=%+v", text, got)
		}
	}
}

func TestAnnotateMediaURI(t *testing.T) {
	uri := "desktop-control://artifacts/artifact_123"
	got := annotateTextWithAssets("临时路径: "+uri, []assets.Ref{{
		ID:       "asset_image_alias",
		Kind:     "image",
		URI:      uri,
		MIMEType: "image/png",
		Metadata: map[string]string{
			"materialized": "true",
		},
	}})
	if len(got) != 1 {
		t.Fatalf("annotations = %+v, want one", got)
	}
	if got[0].Text != uri || got[0].AssetID != "asset_image_alias" || got[0].Kind != "image" {
		t.Fatalf("annotation = %+v", got[0])
	}
}

func TestAnnotateLineMentionsWhenTurnHasOneFile(t *testing.T) {
	text := "`HTMLBuilder` 只在 **一个文件** 中被引用：\n\n" +
		"**`CodePractice.playground/Pages/01-SwiftDeepDive.xcplaygroundpage/Contents.swift`**\n\n" +
		"- **第 99 行** — 定义：`struct HTMLBuilder {`\n" +
		"- **第 109 行** — 作为 result builder 属性使用。"
	refs := []assets.Ref{
		{
			ID:                    "asset_line_99",
			Kind:                  "file_location",
			WorkspaceRelativePath: "CodePractice.playground/Pages/01-SwiftDeepDive.xcplaygroundpage/Contents.swift",
			Range:                 &assets.Range{StartLine: 99},
			DisplayName:           "Contents.swift:99",
			SourceTurnID:          "turn_1",
			SourceCallID:          "call_1",
		},
		{
			ID:                    "asset_line_109",
			Kind:                  "file_location",
			WorkspaceRelativePath: "CodePractice.playground/Pages/01-SwiftDeepDive.xcplaygroundpage/Contents.swift",
			Range:                 &assets.Range{StartLine: 109},
			DisplayName:           "Contents.swift:109",
			SourceTurnID:          "turn_1",
			SourceCallID:          "call_1",
		},
	}
	got := annotateTextWithAssets(text, refs)
	if len(got) != 3 {
		t.Fatalf("annotations = %d, want path + two line mentions: %+v", len(got), got)
	}
	want := map[string]string{
		"第 99 行":  "asset_line_99",
		"第 109 行": "asset_line_109",
	}
	for _, ann := range got {
		if want[ann.Text] != "" && want[ann.Text] != ann.AssetID {
			t.Fatalf("annotation %q -> %q, want %q; all=%+v", ann.Text, ann.AssetID, want[ann.Text], got)
		}
	}
}

func TestAnnotateLineMentionsRequiresContextWhenMultipleFiles(t *testing.T) {
	refs := []assets.Ref{
		{
			ID:                    "asset_a_10",
			Kind:                  "file_location",
			WorkspaceRelativePath: "Sources/A.swift",
			Range:                 &assets.Range{StartLine: 10},
			DisplayName:           "A.swift:10",
		},
		{
			ID:                    "asset_b_10",
			Kind:                  "file_location",
			WorkspaceRelativePath: "Sources/B.swift",
			Range:                 &assets.Range{StartLine: 10},
			DisplayName:           "B.swift:10",
		},
	}
	if got := annotateTextWithAssets("第 10 行需要修改。", refs); len(got) != 0 {
		t.Fatalf("ambiguous line mention annotations = %+v, want none", got)
	}
	got := annotateTextWithAssets("See Sources/B.swift. 第 10 行需要修改。", refs)
	if len(got) != 2 {
		t.Fatalf("annotations = %+v, want file + nearby line", got)
	}
	if got[1].Text != "第 10 行" || got[1].AssetID != "asset_b_10" {
		t.Fatalf("nearby line annotation = %+v", got[1])
	}
}

func TestAnnotateMarkdownTableLineNumberCells(t *testing.T) {
	text := "HTMLBuilder 在项目中只有 **2 处引用**，都在同一个文件里：\n\n" +
		"| 行号 | 文件 | 内容 |\n" +
		"|------|------|------|\n" +
		"| 99 | `CodePractice.playground/Pages/01-SwiftDeepDive.xcplaygroundpage/Contents.swift` | `struct HTMLBuilder {` — 定义 |\n" +
		"| 109 | 同上 | `@HTMLBuilder content: () -> String) -> String {` — 使用 |\n"
	refs := []assets.Ref{
		{
			ID:                    "asset_line_99",
			Kind:                  "file_location",
			WorkspaceRelativePath: "CodePractice.playground/Pages/01-SwiftDeepDive.xcplaygroundpage/Contents.swift",
			Range:                 &assets.Range{StartLine: 99},
			DisplayName:           "Contents.swift:99",
			SourceTurnID:          "turn_1",
			SourceCallID:          "call_1",
		},
		{
			ID:                    "asset_line_109",
			Kind:                  "file_location",
			WorkspaceRelativePath: "CodePractice.playground/Pages/01-SwiftDeepDive.xcplaygroundpage/Contents.swift",
			Range:                 &assets.Range{StartLine: 109},
			DisplayName:           "Contents.swift:109",
			SourceTurnID:          "turn_1",
			SourceCallID:          "call_1",
		},
	}
	got := annotateTextWithAssets(text, refs)
	want := map[string]string{
		"99": "asset_line_99",
		"CodePractice.playground/Pages/01-SwiftDeepDive.xcplaygroundpage/Contents.swift": "asset_line_99",
		"109": "asset_line_109",
	}
	var matched int
	for _, ann := range got {
		if want[ann.Text] == "" {
			continue
		}
		matched++
		if ann.AssetID != want[ann.Text] {
			t.Fatalf("table line annotation %q -> %q, want %q; all=%+v", ann.Text, ann.AssetID, want[ann.Text], got)
		}
	}
	if matched != 3 {
		t.Fatalf("matched table cells = %d, want 3; all=%+v", matched, got)
	}
}

func TestAnnotateLooseMarkdownTablePathCellsFollowRowLine(t *testing.T) {
	path := "CodePractice.playground/Pages/01-SwiftDeepDive.xcplaygroundpage/Contents.swift"
	text := "HTMLBuilder 有 2 处引用，都在同一个文件中：\n" +
		"TABLE\n" +
		"# | 文件 | 行号 | 上下文\n" +
		"1 | " + path + " | 99 | struct HTMLBuilder { — 结构体定义\n" +
		"2 | " + path + " | 109 | @HTMLBuilder content: () -> String) -> String { — 作为 result builder 属性使用\n"
	refs := []assets.Ref{
		{
			ID:                    "asset_line_99",
			Kind:                  "file_location",
			WorkspaceRelativePath: path,
			Range:                 &assets.Range{StartLine: 99},
			DisplayName:           "Contents.swift:99",
			SourceTurnID:          "turn_1",
			SourceCallID:          "call_1",
		},
		{
			ID:                    "asset_line_109",
			Kind:                  "file_location",
			WorkspaceRelativePath: path,
			Range:                 &assets.Range{StartLine: 109},
			DisplayName:           "Contents.swift:109",
			SourceTurnID:          "turn_1",
			SourceCallID:          "call_1",
		},
	}
	got := annotateTextWithAssets(text, refs)
	var secondPathSeen bool
	for _, ann := range got {
		if ann.Text != path {
			continue
		}
		if secondPathSeen {
			if ann.AssetID != "asset_line_109" {
				t.Fatalf("second path annotation = %+v, want asset_line_109; all=%+v", ann, got)
			}
			return
		}
		secondPathSeen = true
		if ann.AssetID != "asset_line_99" {
			t.Fatalf("first path annotation = %+v, want asset_line_99; all=%+v", ann, got)
		}
	}
	t.Fatalf("did not find two path annotations: %+v", got)
}

func TestAnnotateMarkdownTableCompressedPathAndMultipleLineNumbers(t *testing.T) {
	fullPath := "Examples/VideoEditorDemo/Sources/VideoEditorView.swift"
	displayPath := "Examples/VideoEditorDemo/.../VideoEditorView.swift"
	text := "引用方（Examples）\n" +
		"TABLE\n" +
		"文件 | 行 | 用途\n" +
		displayPath + " | 16, 70, 119 | @State private var editorStore: EditorStore?，创建实例 EditorStore(timeline:videoURL:)\n"
	refs := []assets.Ref{
		{
			ID:                    "asset_line_16",
			Kind:                  "file_location",
			WorkspaceRelativePath: fullPath,
			Range:                 &assets.Range{StartLine: 16},
			DisplayName:           "VideoEditorView.swift:16",
		},
		{
			ID:                    "asset_line_70",
			Kind:                  "file_location",
			WorkspaceRelativePath: fullPath,
			Range:                 &assets.Range{StartLine: 70},
			DisplayName:           "VideoEditorView.swift:70",
		},
		{
			ID:                    "asset_line_119",
			Kind:                  "file_location",
			WorkspaceRelativePath: fullPath,
			Range:                 &assets.Range{StartLine: 119},
			DisplayName:           "VideoEditorView.swift:119",
		},
	}
	got := annotateTextWithAssets(text, refs)
	want := map[string]string{
		displayPath: "asset_line_16",
		"16":        "asset_line_16",
		"70":        "asset_line_70",
		"119":       "asset_line_119",
	}
	seen := map[string]bool{}
	for _, ann := range got {
		if want[ann.Text] == "" {
			continue
		}
		seen[ann.Text] = true
		if ann.AssetID != want[ann.Text] {
			t.Fatalf("annotation %q -> %q, want %q; all=%+v", ann.Text, ann.AssetID, want[ann.Text], got)
		}
	}
	for text := range want {
		if !seen[text] {
			t.Fatalf("missing annotation %q; all=%+v", text, got)
		}
	}
}

func TestAnnotateMarkdownTableLineNumberCellsRequireLineHeader(t *testing.T) {
	text := "| 数量 | 文件 |\n|------|------|\n| 99 | Sources/A.swift |\n"
	refs := []assets.Ref{{
		ID:                    "asset_line_99",
		Kind:                  "file_location",
		WorkspaceRelativePath: "Sources/A.swift",
		Range:                 &assets.Range{StartLine: 99},
		DisplayName:           "A.swift:99",
	}}
	got := annotateTextWithAssets(text, refs)
	for _, ann := range got {
		if ann.Text == "99" {
			t.Fatalf("numeric table cell without line header was annotated: %+v", got)
		}
	}
}

func TestRunTurnEmitsAssistantTextAnnotations(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(assetTool{}); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []model.Response{
		{
			ToolCalls: []model.ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: model.FunctionCall{Name: "asset_tool", Arguments: "{}"},
			}},
			FinishReason: "tool_calls",
		},
		{Content: "Open `App.swift:5` for the important line.", FinishReason: "stop"},
	}}
	em := &capturingEmitter{}
	runner := &Runner{
		Model:         provider,
		Tools:         reg,
		MaxSteps:      5,
		Emitter:       em,
		WorkspaceRoot: "/Users/x/project",
	}
	if _, err := runner.RunTurn(context.Background(), newSession(), "find asset"); err != nil {
		t.Fatal(err)
	}
	finished, ok := em.first(EventTurnFinished)
	if !ok {
		t.Fatal("turn_finished not emitted")
	}
	if len(finished.TextAnnotations) != 1 {
		t.Fatalf("annotations = %+v, want one", finished.TextAnnotations)
	}
	ann := finished.TextAnnotations[0]
	if ann.Text != "App.swift:5" || ann.AssetID != "asset_from_tool" || ann.SourceCallID != "call_1" {
		t.Fatalf("annotation = %+v", ann)
	}
}
