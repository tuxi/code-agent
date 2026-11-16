package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/preview"
	"code-agent/internal/tools"
)

type typedPreviewTool struct{ drafts []preview.Draft }

func (t typedPreviewTool) Name() string                 { return "runtime_tool" }
func (t typedPreviewTool) Description() string          { return "typed preview producer" }
func (t typedPreviewTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t typedPreviewTool) Execute(context.Context, tools.ExecutionContext, json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{Content: "ok", Output: json.RawMessage(`{"kind":"not_a_preview"}`), Previews: t.drafts}, nil
}

type capturedPreviewProducer struct {
	request preview.ProduceRequest
	result  preview.ProduceResult
}

func (p *capturedPreviewProducer) Produce(_ context.Context, request preview.ProduceRequest) preview.ProduceResult {
	p.request = request
	return p.result
}

type captureEmitter struct{ events []Event }

func (e *captureEmitter) Emit(event Event) { e.events = append(e.events, event) }

func TestTypedPreviewSideChannelUsesRuntimeIdentity(t *testing.T) {
	registry := tools.NewRegistry()
	tool := typedPreviewTool{drafts: []preview.Draft{{ArtifactKey: "typed", Kind: preview.KindInteractiveHTML, Title: "Title"}}}
	if err := registry.RegisterBound(tool, tools.Binding{Name: tool.Name(), ModelVisibility: tools.VisibilityDirect, Executor: tools.ExecutorServer, Capability: "active-cap"}); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Tools: registry, WorkspaceRoot: t.TempDir()}
	plan := toolCallPlan{
		call:  model.ToolCall{ID: "call_1", Function: model.FunctionCall{Name: tool.Name(), Arguments: `{}`}},
		input: json.RawMessage(`{}`), wireInput: json.RawMessage(`{}`), tool: tool,
		known: true, valid: true, executor: "server", binding: tools.Binding{Name: tool.Name(), Capability: "active-cap"},
	}
	result := runner.runToolCall(context.Background(), plan)
	if len(result.previews) != 1 || result.previews[0].ArtifactKey != "typed" {
		t.Fatalf("typed preview side-channel = %+v", result.previews)
	}

	producer := &capturedPreviewProducer{}
	runner.PreviewProducer = producer
	runner.emitSessionID = "session_preview"
	runner.emitTurnID = "turn_preview"
	runner.emitPreviewArtifacts(context.Background(), plan, nil, result.previews, false)
	if producer.request.ProducerID != tool.Name() || producer.request.Capability != "active-cap" {
		t.Fatalf("producer identity = %q/%q", producer.request.ProducerID, producer.request.Capability)
	}
	if producer.request.CallID != "call_1" || len(producer.request.Drafts) != 1 {
		t.Fatalf("producer request = %+v", producer.request)
	}
}

func TestPreviewArtifactEmitsCompleteSnapshot(t *testing.T) {
	root := filepath.Join("..", "..", "docs", "capability-runtime-upgrade", "fixtures", "preview", "v1")
	data, err := os.ReadFile(filepath.Join(root, "pending.json"))
	if err != nil {
		t.Fatal(err)
	}
	var artifact preview.Artifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatal(err)
	}
	artifact.SessionID = "session_preview"
	artifact.TurnID = "turn_preview"
	artifact.CallID = "call_1"
	hash, err := preview.RevisionSHA256(artifact)
	if err != nil {
		t.Fatal(err)
	}
	artifact.RevisionSHA256 = hash
	producer := &capturedPreviewProducer{result: preview.ProduceResult{Artifacts: []preview.Artifact{artifact}}}
	emitter := &captureEmitter{}
	runner := &Runner{PreviewProducer: producer, Emitter: emitter, emitSessionID: "session_preview", emitTurnID: "turn_preview"}
	plan := toolCallPlan{call: model.ToolCall{ID: "call_1", Function: model.FunctionCall{Name: "runtime_tool"}}, binding: tools.Binding{Capability: "active-cap"}}
	runner.emitPreviewArtifacts(context.Background(), plan, nil, []preview.Draft{{ArtifactKey: "typed"}}, false)
	if len(emitter.events) != 1 || emitter.events[0].Kind != EventPreviewArtifact || emitter.events[0].Artifact == nil {
		t.Fatalf("preview events = %+v", emitter.events)
	}
	if emitter.events[0].Artifact.RevisionSHA256 != hash || emitter.events[0].SessionID != "session_preview" || emitter.events[0].TurnID != "turn_preview" {
		t.Fatalf("preview event snapshot = %+v", emitter.events[0])
	}
}

type flexServerTool struct{}

func (flexServerTool) Name() string                 { return "flex_server" }
func (flexServerTool) Description() string          { return "flex server" }
func (flexServerTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (flexServerTool) Execute(context.Context, tools.ExecutionContext, json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{Content: "ok"}, nil
}

func TestFlexExecutorResolvesClientOrServer(t *testing.T) {
	runner := &Runner{ClientWaiter: &immediateClientWaiter{}}
	clientProxy := tools.NewClientProxyTool("flex_client", "", json.RawMessage(`{"type":"object"}`))
	if got := runner.executorFor(clientProxy, true, tools.Binding{Name: "flex_client", ModelVisibility: tools.VisibilityDirect, Executor: tools.ExecutorFlex}); got != "client" {
		t.Fatalf("client flex executor = %q", got)
	}
	if got := runner.executorFor(flexServerTool{}, true, tools.Binding{Name: "flex_server", ModelVisibility: tools.VisibilityDirect, Executor: tools.ExecutorFlex}); got != "server" {
		t.Fatalf("server flex executor = %q", got)
	}
}

func TestOutputNeverSynthesizesPreview(t *testing.T) {
	producer := &capturedPreviewProducer{}
	runner := &Runner{PreviewProducer: producer, Emitter: &captureEmitter{}}
	plan := toolCallPlan{call: model.ToolCall{ID: "call_1", Function: model.FunctionCall{Name: "view_image"}}, binding: tools.Binding{Name: "view_image", Capability: "cap"}}
	runner.emitPreviewArtifacts(context.Background(), plan, nil, nil, false)
	if producer.request.SessionID != "" {
		t.Fatalf("producer was called without a typed draft: %+v", producer.request)
	}
}

type strictClientFlexTool struct{ flexServerTool }

func (strictClientFlexTool) ExecutionMode() tools.ExecutionMode { return tools.ExecStrictClient }

func TestFlexStrictClientResolvesToClient(t *testing.T) {
	runner := &Runner{}
	if got := runner.executorFor(strictClientFlexTool{}, true, tools.Binding{Name: "strict_client_flex", ModelVisibility: tools.VisibilityDirect, Executor: tools.ExecutorFlex}); got != "client" {
		t.Fatalf("strict-client flex executor = %q, want client", got)
	}
}

// storeProvidingProducer is a PreviewProducer that also owns the immutable
// preview store, mirroring preview.Service.
type storeProvidingProducer struct{ store preview.Store }

func (storeProvidingProducer) Produce(context.Context, preview.ProduceRequest) preview.ProduceResult {
	return preview.ProduceResult{}
}

func (p storeProvidingProducer) PreviewStore() preview.Store { return p.store }

type previewStoreProbe struct{ got preview.Store }

func (p *previewStoreProbe) Name() string        { return "preview_store_probe" }
func (p *previewStoreProbe) Description() string { return "probe" }
func (p *previewStoreProbe) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (p *previewStoreProbe) Execute(_ context.Context, ec tools.ExecutionContext, _ json.RawMessage) (tools.ToolResult, error) {
	p.got = ec.PreviewStore
	return tools.ToolResult{Content: "ok"}, nil
}

func TestPreviewStoreReachesToolExecutionContext(t *testing.T) {
	store, err := preview.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := preview.NewService(store)
	if service.PreviewStore() != store {
		t.Fatal("preview.Service did not expose its store")
	}
	if got := previewStoreFor(service); got != store {
		t.Fatalf("previewStoreFor(service) = %v", got)
	}
	if got := previewStoreFor(&capturedPreviewProducer{}); got != nil {
		t.Fatalf("producer without a store returned %v", got)
	}
	if got := previewStoreFor(nil); got != nil {
		t.Fatalf("nil producer returned %v", got)
	}

	probe := &previewStoreProbe{}
	runner := &Runner{PreviewProducer: service}
	if _, err := runner.executeTool(context.Background(), probe, "call_preview_store", json.RawMessage(`{}`), false); err != nil {
		t.Fatal(err)
	}
	if probe.got != store {
		t.Fatalf("tool ExecutionContext.PreviewStore = %v, want the service store", probe.got)
	}
}
