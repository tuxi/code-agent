package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/tools"
	"code-agent/internal/tools/filesystem"
)

type provenanceEmitter struct{ events []Event }

func (e *provenanceEmitter) Emit(event Event) { e.events = append(e.events, event) }

func TestRequestProvenanceCapturesActualTransformedRequest(t *testing.T) {
	sess, err := session.NewBuilder(t.TempDir()).WithNoContextFiles(true).WithSystemPrompt("original").WithSkillsIndex("skill index").Build()
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	_ = registry.Register(filesystem.NewCreateFileTool())
	_ = registry.RegisterInternal(tools.NewClientProxyTool("hidden", "hidden", json.RawMessage(`{"type":"object"}`)))
	provider := &scriptedProvider{responses: []model.Response{{Content: "done", Usage: model.Usage{PromptTokens: 40}}}}
	sink := &provenanceEmitter{}
	r := &Runner{Model: provider, ModelName: "actual-model", Tools: registry, Emitter: sink, MaxSteps: 1, TransformContext: func(messages []model.Message) []model.Message {
		messages[0].Content = "hook changed the prompt"
		return messages
	}}
	if _, err := r.RunTurn(context.Background(), sess, "hello"); err != nil {
		t.Fatal(err)
	}
	var snapshot *RequestContextSnapshot
	for _, event := range sink.events {
		if event.Kind == EventModelRequest {
			snapshot = event.ContextSnapshot
			if snapshot == nil || snapshot.SnapshotID != event.InvocationID {
				t.Fatal("missing request identity")
			}
		}
	}
	if snapshot == nil || snapshot.Model != "actual-model" || snapshot.HistoryRevision == "" || len(snapshot.Tools) != 1 || snapshot.Tools[0].Name != "create_file" || snapshot.Tools[0].SourceID == "" {
		t.Fatalf("request projection: %+v", snapshot)
	}
	if snapshot.Categories["skills"] != 0 {
		t.Fatal("hook-removed index counted as active")
	}
	total := 0
	for _, n := range snapshot.Categories {
		total += n
	}
	if total != snapshot.TotalEstimatedTokens || total <= 0 {
		t.Fatal("estimate mismatch")
	}
	for _, source := range snapshot.Sources {
		if source.Kind == "system_prompt" {
			body := session.ContextCatalog(sess)[source.ID].Content
			if body == "original" || body == "" {
				t.Fatal("captured pre-transform prompt")
			}
		}
	}

	if sess.Messages[0].Content == "hook changed the prompt" {
		t.Fatal("persisted context changed")
	}
}

func TestOutputEvidenceRealWriteAndReadAssetsRemainSeparate(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	_ = registry.Register(filesystem.NewCreateFileTool())
	provider := &scriptedProvider{responses: []model.Response{{ToolCalls: []model.ToolCall{{ID: "write-call", Type: "function", Function: model.FunctionCall{Name: "create_file", Arguments: `{"path":"output.txt","content":"hello"}`}}}}, {Content: "done"}}}
	sink := &provenanceEmitter{}
	r := &Runner{Model: provider, Tools: registry, Emitter: sink, WorkspaceRoot: root, MaxSteps: 2, Approver: allowApprover{}}
	sess, _ := session.NewBuilder(root).WithNoContextFiles(true).Build()
	if _, err := r.RunTurn(context.Background(), sess, "create a file"); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(root, "output.txt")); err != nil || string(body) != "hello" {
		t.Fatal("fixture did not write", err, string(body))
	}
	found := false
	for _, event := range sink.events {
		if event.Kind == EventToolFinished && event.CallID == "write-call" {
			found = true
			if event.OutputEvidenceVersion != 1 || len(event.FileWrites) != 1 || len(event.Assets) == 0 || event.Assets[0].ID == "" {
				t.Fatalf("write lost evidence: %+v", event)
			}
		}
	}
	if !found {
		t.Fatal("missing write lifecycle")
	}
}

type provenanceOriginTool struct{ tools.Tool }

func (provenanceOriginTool) ToolOrigin() tools.ToolOrigin {
	return tools.ToolOrigin{Kind: "mcp", ConnectorID: "mcp:actual-server"}
}
func TestRequestProvenanceUsesOriginAndDetectsPrunedSkillBody(t *testing.T) {
	sess, _ := session.NewBuilder(t.TempDir()).WithNoContextFiles(true).Build()
	skill := session.RegisterContextSource(sess, "skills", "loaded skill", "", "original loaded body")
	sess.Metadata[session.ContextSkillCallsKey] = map[string]string{"skill-call": skill.ID}
	registry := tools.NewRegistry()
	_ = registry.Register(provenanceOriginTool{Tool: tools.NewClientProxyTool("ordinary_name", "lookup", json.RawMessage(`{"type":"object"}`))})
	r := &Runner{Tools: registry, contextSession: sess, emitInvocationID: "request"}
	request := model.Request{Messages: []model.Message{{Role: model.RoleTool, ToolCallID: "skill-call", Content: "original loaded body"}}, Tools: toolDefinitions(registry)}
	snapshot := r.captureRequestContext(request)
	if len(snapshot.Tools) != 1 || snapshot.Tools[0].Origin != "mcp" || snapshot.Tools[0].ConnectorID != "mcp:actual-server" || snapshot.Categories["mcp_tools"] <= 0 || snapshot.Categories["skills"] <= 0 {
		t.Fatalf("origin/loaded body: %+v", snapshot)
	}
	request.Messages[0].Content = "[old result cleared]"
	snapshot = r.captureRequestContext(request)
	if snapshot.Categories["skills"] != 0 {
		t.Fatal("pruned body remains active")
	}
	request.Tools = nil
	snapshot = r.captureRequestContext(request)
	if len(snapshot.Tools) != 0 || snapshot.Categories["mcp_tools"] != 0 {
		t.Fatal("tool-free request inherited previous tool catalog")
	}
}
