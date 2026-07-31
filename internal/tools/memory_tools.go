package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"code-agent/internal/memory"
)

// memoryStores caches per-workspace memory stores so they survive across
// tool calls within the same conversation. In daemon mode this is the only
// source of truth — the global tool registry is shared, but each workspace
// needs its own Store.
var (
	memoryStoresMu sync.Mutex
	memoryStores   = map[string]*memory.Store{}
)

func getMemoryStore(workspaceRoot string) *memory.Store {
	if workspaceRoot == "" {
		return nil
	}
	memoryStoresMu.Lock()
	defer memoryStoresMu.Unlock()
	if s, ok := memoryStores[workspaceRoot]; ok {
		return s
	}
	s, err := memory.Open(filepath.Join(workspaceRoot, ".codeagent", "memory"))
	if err != nil {
		return nil
	}
	memoryStores[workspaceRoot] = s
	return s
}

// ── create_memory ─────────────────────────────────────────────────────

type createMemoryTool struct{}

func (t *createMemoryTool) Name() string        { return "create_memory" }
func (t *createMemoryTool) Description() string { return createMemoryDesc }
func (t *createMemoryTool) SideEffects() bool   { return true }
func (t *createMemoryTool) InputSchema() json.RawMessage {
	return Object(map[string]Property{
		"name":        {Type: "string", Description: "Short slug for this memory (e.g. go-test-conventions)"},
		"description": {Type: "string", Description: "One-line summary used for relevance matching when recalling"},
		"content":     {Type: "string", Description: "The memory content (markdown). Keep it concise and actionable."},
	}).JSON()
}

func (t *createMemoryTool) Execute(_ context.Context, ec ExecutionContext, raw json.RawMessage) (ToolResult, error) {
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Content     string `json:"content"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ToolResult{}, fmt.Errorf("create_memory: %w", err)
	}
	store := getMemoryStore(ec.WorkspaceRoot)
	if store == nil {
		return ToolResult{Content: "(memory store not available for this workspace)"}, nil
	}
	m, err := store.Create(in.Name, in.Description, in.Content)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("create_memory error: %v", err)}, nil
	}
	return ToolResult{Content: fmt.Sprintf("Created memory %q at %s", m.Name, m.FilePath)}, nil
}

const createMemoryDesc = "Create a new project memory. Use this to persist conventions, preferences, or lessons learned so they are recalled in future sessions."

// ── update_memory ─────────────────────────────────────────────────────

type updateMemoryTool struct{}

func (t *updateMemoryTool) Name() string        { return "update_memory" }
func (t *updateMemoryTool) Description() string { return updateMemoryDesc }
func (t *updateMemoryTool) SideEffects() bool   { return true }
func (t *updateMemoryTool) InputSchema() json.RawMessage {
	return Object(map[string]Property{
		"name":        {Type: "string", Description: "Name of the memory to update"},
		"description": {Type: "string", Description: "Updated one-line description"},
		"content":     {Type: "string", Description: "Updated markdown content"},
	}).JSON()
}

func (t *updateMemoryTool) Execute(_ context.Context, ec ExecutionContext, raw json.RawMessage) (ToolResult, error) {
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Content     string `json:"content"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ToolResult{}, fmt.Errorf("update_memory: %w", err)
	}
	store := getMemoryStore(ec.WorkspaceRoot)
	if store == nil {
		return ToolResult{Content: "(memory store not available for this workspace)"}, nil
	}
	if err := store.Update(in.Name, in.Description, in.Content); err != nil {
		return ToolResult{Content: fmt.Sprintf("update_memory error: %v", err)}, nil
	}
	return ToolResult{Content: fmt.Sprintf("Updated memory %q", in.Name)}, nil
}

const updateMemoryDesc = "Update an existing project memory. Use when a convention or preference has changed."

// ── delete_memory ─────────────────────────────────────────────────────

type deleteMemoryTool struct{}

func (t *deleteMemoryTool) Name() string        { return "delete_memory" }
func (t *deleteMemoryTool) Description() string { return deleteMemoryDesc }
func (t *deleteMemoryTool) SideEffects() bool   { return true }
func (t *deleteMemoryTool) InputSchema() json.RawMessage {
	return Object(map[string]Property{
		"name": {Type: "string", Description: "Name of the memory to delete"},
	}).JSON()
}

func (t *deleteMemoryTool) Execute(_ context.Context, ec ExecutionContext, raw json.RawMessage) (ToolResult, error) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ToolResult{}, fmt.Errorf("delete_memory: %w", err)
	}
	store := getMemoryStore(ec.WorkspaceRoot)
	if store == nil {
		return ToolResult{Content: "(memory store not available for this workspace)"}, nil
	}
	if err := store.Delete(in.Name); err != nil {
		return ToolResult{Content: fmt.Sprintf("delete_memory error: %v", err)}, nil
	}
	return ToolResult{Content: fmt.Sprintf("Deleted memory %q", in.Name)}, nil
}

const deleteMemoryDesc = "Delete a project memory. Use when a convention or preference is no longer relevant."

// ── list_memories ─────────────────────────────────────────────────────

type listMemoriesTool struct{}

func (t *listMemoriesTool) Name() string        { return "list_memories" }
func (t *listMemoriesTool) Description() string { return listMemoriesDesc }
func (t *listMemoriesTool) InputSchema() json.RawMessage {
	return Object(map[string]Property{}).JSON()
}

func (t *listMemoriesTool) Execute(_ context.Context, ec ExecutionContext, _ json.RawMessage) (ToolResult, error) {
	store := getMemoryStore(ec.WorkspaceRoot)
	if store == nil {
		return ToolResult{Content: "(memory store not available for this workspace)"}, nil
	}
	mems, err := store.List()
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("list_memories error: %v", err)}, nil
	}
	if len(mems) == 0 {
		return ToolResult{Content: "(no memories found)"}, nil
	}
	var b strings.Builder
	for _, m := range mems {
		fmt.Fprintf(&b, "**%s** — %s\n%s\n\n", m.Name, m.Description, m.Content)
	}
	return ToolResult{Content: b.String()}, nil
}

const listMemoriesDesc = "List all project memories. Use before starting a new task to check for relevant conventions or preferences."

// ── recall_memory ─────────────────────────────────────────────────────

type recallMemoryTool struct{}

func (t *recallMemoryTool) Name() string        { return "recall_memory" }
func (t *recallMemoryTool) Description() string { return recallMemoryDesc }
func (t *recallMemoryTool) InputSchema() json.RawMessage {
	return Object(map[string]Property{
		"query": {Type: "string", Description: "Search query. Tokens are matched against memory descriptions and content. Use keywords from the current task."},
	}).JSON()
}

func (t *recallMemoryTool) Execute(_ context.Context, ec ExecutionContext, raw json.RawMessage) (ToolResult, error) {
	var in struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ToolResult{}, fmt.Errorf("recall_memory: %w", err)
	}
	store := getMemoryStore(ec.WorkspaceRoot)
	if store == nil {
		return ToolResult{Content: "(memory store not available for this workspace)"}, nil
	}
	mems, err := store.Recall(in.Query, 5)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("recall_memory error: %v", err)}, nil
	}
	if len(mems) == 0 {
		return ToolResult{Content: "(no matching memories found)"}, nil
	}
	var b strings.Builder
	for _, m := range mems {
		fmt.Fprintf(&b, "**%s** — %s\n%s\n\n", m.Name, m.Description, m.Content)
	}
	return ToolResult{Content: b.String()}, nil
}

const recallMemoryDesc = "Search project memories by relevance to a query. Use keywords from the current task to find applicable conventions or preferences."

// ── Factory ───────────────────────────────────────────────────────────

// NewMemoryTools returns the memory management tools. The tools lazily
// initialize their per-workspace store on first use via ExecutionContext.
func NewMemoryTools() []Tool {
	return []Tool{
		&createMemoryTool{},
		&updateMemoryTool{},
		&deleteMemoryTool{},
		&listMemoriesTool{},
		&recallMemoryTool{},
	}
}
