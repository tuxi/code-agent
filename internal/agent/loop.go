package agent

import (
	"code-agent/internal/assetref"
	"code-agent/internal/hooks"
	"code-agent/internal/model"
	"code-agent/internal/observation"
	"code-agent/internal/reflection"
	"code-agent/internal/session"
	"code-agent/internal/tools"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// cooldownSample records the cache performance of one model call for the
// adaptive compaction cooldown (see effectiveCooldownRatio).
type cooldownSample struct {
	promptTokens int // total prompt tokens for the request
	cachedTokens int // portion served from the provider's prompt cache
}

type Runner struct {
	Model       model.Provider
	ModelName   string
	Temperature float64
	Tools       *tools.Registry
	MaxSteps    int

	// MaxWebSearches caps web_search calls within one user turn (0 = default).
	// A search-happy model that keeps reformulating instead of answering gets a
	// "stop searching" result past the cap; the counter resets each turn.
	MaxWebSearches int

	// MaxParallelTools caps how many independent, read-only tool calls in one
	// batch run concurrently (P8.8). <= 1 (the default) restores the strictly
	// sequential loop. Side-effecting calls are always serialized regardless.
	MaxParallelTools int

	// Approver gates side-effecting tool calls. If nil, side-effecting tools are
	// denied (see approve()). Read-only tools never consult it.
	Approver Approver

	// PathAccessApprover gates read-only access to paths outside the workspace.
	// When non-nil, read tools (read_file, list_files, grep) request user approval
	// for external paths instead of hard-rejecting them. When nil (headless mode,
	// legacy clients), external paths are rejected — the existing behaviour.
	PathAccessApprover tools.PathAccessApprover

	// ClientWaiter blocks the turn goroutine while a client-executed tool runs.
	// When nil (no client connected, or headless mode), all tools run server-side.
	// v1.1: see docs/protocols/agent-wire-v1.1-client-tool-execution.md §5.
	ClientWaiter ClientToolWaiter

	// ClientToolTimeout is the lease timeout for a single client tool call.
	// Zero uses a 2-minute default.
	ClientToolTimeout time.Duration

	// Observer enriches each tool result into a structured Observation (P4.1).
	// Nil-safe: when unset, raw tool results are appended unchanged.
	Observer Observer

	// Reflector runs a one-shot self-check at the finalize boundary (P4.3).
	// Nil-safe: when unset, the model's first "done" is accepted as before.
	Reflector Reflector

	// Hook runs user-configured pre/post-tool commands (8.5). Nil-safe: when unset,
	// tools run exactly as before.
	Hook ToolHook

	// HookRunner is the full hook runner for turn-level events: context
	// transforms (before each LLM call) and stop decisions (after_turn).
	// Nil-safe: when unset, these events are silent no-ops.
	HookRunner *hooks.Runner

	// StopPolicy is the finalize decision point (8.5): consulted when the model
	// returns a text-only message and wants to finish. Nil-safe: nil selects the
	// built-in default policy (build verify + change review + todo gate),
	// preserving the loop's pre-policy behavior exactly. A configured policy
	// REPLACES the default — the operator owns the stop decision. The step
	// budget remains the hard backstop behind any policy.
	StopPolicy StopPolicy

	// TrustPolicy resolves project trust. Nil means the project is trusted
	// unconditionally (backward compatible — existing deployments have no
	// trust gating).
	TrustPolicy TrustPolicy

	// Stream, when true AND the provider supports it, streams final-answer text
	// and provider-visible reasoning via their distinct delta events. The returned
	// Response remains identical to the non-streamed one.
	Stream bool

	// RemindSkills, when true, injects a one-shot ephemeral reminder on the first
	// model call of each turn to check the Skills list and load a matching skill
	// (P6). It makes skill-loading consistent across models rather than depending
	// on a model's agency. Set by the CLI when the project has skills.
	RemindSkills bool

	// RemindParallel, when true, injects a first-call reminder teaching the model
	// WHEN to fan out independent work into concurrent tool/`task` calls vs. keep
	// dependent work serial (P8.8 §7). Set only when MaxParallelTools > 1 — with
	// parallelism off, fanning out just burns tokens with no speedup, so the
	// guidance is withheld.
	RemindParallel bool

	// VerifyCommand is the project's real build/test command. When set, the
	// finalize self-check runs it once (P4.3-R Move 2, option 2a) for a turn that
	// changed verifiable code without verifying it: a pass confirms the change, a
	// fail re-prompts with the real failure. Empty disables the runtime verify —
	// the runtime then never asserts "unverified" (the retired mechanical guess).
	VerifyCommand string

	// RemindHypothesis, when true, fires a one-shot pre-mutation self-check
	// (P4.3-R Move 3): when the model is about to edit code AFTER a failure
	// surfaced this turn — the paper-over hot zone — the turn's premature edit is
	// dropped (not persisted, not executed) and re-prompted with an ephemeral
	// nudge to state a root-cause hypothesis first. Mirror image of the finalize
	// self-check, fired at the start line instead of the finish line. Nil-safe:
	// when false, edits proceed exactly as before.
	RemindHypothesis bool

	// PlanApprover handles plan-level approval (plan → approve → execute).
	// When nil, propose_plan auto-approves (test/headless path). Set by the
	// REPL, TUI, or server to gate plan execution behind a human decision.
	PlanApprover PlanApprover

	// AskUserApprover presents a clarification question to the user when the
	// model encounters ambiguity (the Human-in-the-Loop Clarification Query).
	// When nil, ask_user returns a fallback message so unattended runs don't
	// block. Set by the REPL, TUI, or server like PlanApprover.
	AskUserApprover AskUserApprover

	// SessionIndex provides cross-workspace session discovery for list_sessions
	// and read_session tools. Nil means these tools are unavailable (index.db
	// failed to open or this runtime does not support cross-session operations).
	SessionIndex   tools.SessionIndex
	SessionControl tools.SessionControl

	// PlanState tracks the planning workflow phase. Exported so the TUI and REPL
	// can toggle plan mode manually (Ctrl+P, /plan).
	PlanState PlanStatus

	// activePlan is the current plan, populated when propose_plan is called.
	activePlan *Plan

	// planTitle is set by enter_plan_mode's input or /plan command.
	planTitle string

	// PlanTools is the restricted toolset used during Planning/Proposing states.
	// Built from planModeToolNames at construction time. Exported so the cmd
	// layer can wire it from planModeToolNames after runner construction.
	PlanTools *tools.Registry

	// lastAssistantText stores the model's most recent ordinary assistant
	// content so propose_plan can use it as a compatibility fallback without
	// confusing user-visible text with provider reasoning.
	lastAssistantText string

	plannedMutation         bool
	independentReviewPassed bool
	mutatedVerifiableCode   bool // true when this turn touched verifiable code (not docs/data)
	changeReviewCount       int  // passing change_review completions since the last mutation; re-armed by any new mutation

	// todos is the model's current task checklist (8.4), materialized on the
	// runner so the loop can consult it at the finalize boundary (the todo gate).
	// Per-turn state: reset at the start of drive() and rewritten on each
	// successful todo_write, in model order on the main goroutine.
	todos []tools.Todo

	Compactor session.Compactor

	// ContextEditor clears stale tool results and thinking blocks before
	// compaction runs. Nil means skip this step.
	ContextEditor *session.ContextEditor

	// CompactKeepTokens mirrors the compactor's verbatim-tail budget for tier-0
	// pruning (P12.c): messages outside this approximate-token protection window
	// get their old tool outputs truncated and think-blocks stripped before an
	// LLM summarize is even considered. 0 disables pruning. Wired from
	// cfg.CompactKeepTokens(mc), the same source as the compactor's budget.
	CompactKeepTokens int

	// gitCache avoids redundant git(1) invocations on every model request within
	// a turn. Nil leaves the legacy per-request GitInfo() path intact.
	GitCache *session.FastGitProvider

	// cooldownSamples is a sliding window of (promptTokens, cachedTokens) pairs
	// from recent model calls, used by effectiveCooldownRatio to tune the
	// compaction cooldown threshold against observed cache performance.
	cooldownSamples    []cooldownSample
	cooldownWindowSize int // cap on cooldownSamples; default 8

	// Checkpointer, if set, persists the session mid-turn at each consistent loop
	// boundary (v1.2 §2). Best-effort: a checkpoint error never fails the turn — the
	// caller's turn-boundary Save is the backstop. Set by the serve/embedded path
	// (which holds the repository); CLI/TUI leave it nil and save only at the
	// turn boundary as before.
	Checkpointer Checkpointer

	// Emitter, if set, receives the turn's event stream (thinking, tool calls,
	// compaction, model latency). The loop emits; it never writes to stdout
	// itself, so the UI is fully decoupled from the runtime.
	Emitter Emitter

	// WorkspaceRoot is the absolute project root directory for this runner.
	// It is set at construction and used to build ExecutionContext for each
	// tool call. For the serve path, this comes from the WorkspaceInstance;
	// for REPL/TUI, from cfg.Workspace.Root.
	WorkspaceRoot string

	// AssetUploader turns locally materialized screenshot files into
	// Gateway-owned references. Nil leaves non-Gateway executions unchanged.
	AssetUploader model.AssetUploader
	// AutoUploadCaptureAssets requires an explicit host opt-in. Merely wiring a
	// Gateway uploader never authorizes screenshots or photos to leave the
	// workspace.
	AutoUploadCaptureAssets bool
	assetUploadCache        map[string]model.GatewayAssetRef
	UserAssetsSupported     bool

	// Correlation IDs stamped onto every emitted event. Set per RunTurn (which is
	// sequential on a Runner), so an event always carries which session and turn
	// produced it.
	emitSessionID    string
	emitTurnID       string
	emitInvocationID string // set at the start of each model call; stamped on all events
	emitRequestID    string // stable Agent Wire submission identity for Gateway
	// ReservedTurnID is supplied by the transport when it accepted a queued turn.
	// Empty preserves the standalone runner's persisted session sequence behavior.
	ReservedTurnID string
	RequestID      string

	// SystemPromptOverride, when non-empty, temporarily replaces the first
	// message (system prompt) for the next model call only. After the call
	// completes it is cleared so subsequent calls revert to the base prompt.
	// Plan mode, sub-agents, and extension results use this to inject
	// phase-specific instructions without rebuilding the session.
	SystemPromptOverride string

	// TransformContext, when non-nil, is called on a COPY of the message list
	// right before every model request. The returned slice replaces the messages
	// for this call only; the session's persisted messages are never affected.
	// Extensions use this for last-mile modifications: injecting state, adding
	// reminders, or adapting the prompt per turn.
	TransformContext func(messages []model.Message) []model.Message

	// emitMu serializes r.emit so concurrent tool workers (P8.8) can't race the
	// downstream emitter.
	emitMu sync.Mutex
}

// effectiveCooldownRatio returns the compaction cooldown threshold adapted to
// recent cache performance. When the provider's prompt cache is healthy (≥80%
// hit rate), the ratio rises toward 0.20 to protect the stable prefix. When
// the cache is cold (≤30% hit rate), the ratio drops toward 0.05 so the agent
// prioritizes reclaiming context over protecting a cache that isn't helping.
// Returns the base ratio (0.10) when there are too few samples.
func (r *Runner) effectiveCooldownRatio() float64 {
	const (
		baseRatio  = 0.10
		minRatio   = 0.05
		maxRatio   = 0.20
		highWater  = 0.80
		lowWater   = 0.30
		minSamples = 3
	)

	if len(r.cooldownSamples) < minSamples {
		return baseRatio
	}

	var sumHit, sumTotal int
	for _, s := range r.cooldownSamples {
		sumHit += s.cachedTokens
		sumTotal += s.promptTokens
	}
	if sumTotal == 0 {
		return baseRatio
	}

	hitRatio := float64(sumHit) / float64(sumTotal)
	switch {
	case hitRatio >= highWater:
		return maxRatio
	case hitRatio <= lowWater:
		return minRatio
	default:
		// Linear interpolation between [lowWater, highWater] → [minRatio, maxRatio].
		scale := (hitRatio - lowWater) / (highWater - lowWater)
		return minRatio + scale*(maxRatio-minRatio)
	}
}

// recordCacheSample appends a cache-performance sample from a model response
// to the sliding window. It keeps at most cooldownWindowSize samples (default 8).
func (r *Runner) recordCacheSample(promptTokens, cachedTokens int) {
	if r.cooldownWindowSize <= 0 {
		r.cooldownWindowSize = 8
	}
	r.cooldownSamples = append(r.cooldownSamples, cooldownSample{
		promptTokens: promptTokens,
		cachedTokens: cachedTokens,
	})
	if len(r.cooldownSamples) > r.cooldownWindowSize {
		r.cooldownSamples = r.cooldownSamples[len(r.cooldownSamples)-r.cooldownWindowSize:]
	}
}

// newInvocationID returns a globally-unique invocation identifier so the
// gateway can safely treat (user_id, invocation_id) as an idempotency key
// across process restarts and multiple Runtime instances.
func newInvocationID() string { return uuid.New().String() }

// nextSessionTurnID returns a session-scoped, monotonic turn identifier. Unlike
// a process-global counter (which resets on restart and can produce duplicates
// within a resumed session), the counter lives in sess.Metadata — it is persisted
// with the session and survives process restarts, so every turn_id is unique
// within the conversation forever.
func nextSessionTurnID(sess *session.Session) string {
	n := 0
	switch v := sess.Metadata["turn_seq"].(type) {
	case float64:
		n = int(v)
	case int:
		n = v
	}
	n++
	sess.Metadata["turn_seq"] = float64(n) // float64 for JSON round-trip
	return fmt.Sprintf("turn_%d", n)
}

// emit sends an event to the configured Emitter, if any. Nil-safe.
func (r *Runner) emit(e Event) {
	if r.Emitter == nil {
		return
	}
	e.At = time.Now()
	if e.SessionID == "" {
		e.SessionID = r.emitSessionID
	}
	if e.TurnID == "" {
		e.TurnID = r.emitTurnID
	}
	e.InvocationID = r.emitInvocationID
	// Serialize emits: with parallel tool execution (P8.8) a concurrent worker's
	// tool may stream stdout/stderr chunks through r.emit while the main
	// goroutine emits batch events. The lock keeps the downstream emitter (which
	// need not be concurrency-safe, e.g. a console renderer) from racing.
	r.emitMu.Lock()
	r.Emitter.Emit(e)
	r.emitMu.Unlock()
}

// TurnResult is the outcome of a single turn: the final answer the model
// produced and the tool steps taken to get there. The conversation itself lives
// on the Session, which accumulates across turns.
type TurnResult struct {
	// TurnID identifies this execution in the event stream. The lifecycle layer
	// uses it to emit a terminal event after the runner has returned an error.
	TurnID       string
	Deduplicated bool // true when request_id resolved to an already accepted turn
	Final        string
	Steps        []Step
	PromptTokens int

	// TokensUsed is the turn's CUMULATIVE token consumption: the sum over every
	// model call this turn of prompt+completion usage. It differs from
	// PromptTokens on purpose — PromptTokens is a GAUGE (the last call's context
	// size, which drives compaction), TokensUsed is a COUNTER (what a turn-budget
	// must accumulate). Summing PromptTokens across turns would conflate context
	// size with spend; a /goal budget reads TokensUsed.
	TokensUsed int

	// Billing Units are authoritative values returned by Gateway. Model and
	// managed-tool spend stay separate for diagnostics and add up to
	// BillingUnits; local tools never contribute Units.
	ModelBillingUnits int64
	ToolBillingUnits  int64
	BillingUnits      int64

	// Tool counters describe runtime work, not model intent. Calls blocked by
	// policy/budget are not executed; billable calls are successful managed
	// calls carrying a Gateway usage receipt.
	ExecutedToolCalls  int
	SucceededToolCalls int
	BillableToolCalls  int

	// billedToolCallIDs prevents a replayed managed result from being counted
	// twice if the same call is observed again within this drive.
	billedToolCallIDs map[string]struct{}

	// HitStepLimit is true when the turn exhausted MaxSteps and Final came from the
	// best-effort tool-free answer rather than the model finishing on its own. A
	// caller that delegates a turn (the subagent, 8.3) uses it to avoid passing off
	// a non-convergent run as a clean conclusion.
	HitStepLimit bool
}

const defaultMaxSteps = 24

// webSearchToolName is the search tool subject to the per-turn budget below.
const webSearchToolName = "web_search"

// defaultMaxWebSearches caps web_search calls per user turn. Search-happy models
// reformulate the same query many ways instead of answering; the cap forces them
// to stop and use what they have. Set above a typical real need so it only bites
// genuine thrash.
const defaultMaxWebSearches = 5

// toolCallSeq backs synthetic tool_call ids (see RunTurn). Process-global and
// monotonic so synthesized ids never collide within a session.
var toolCallSeq atomic.Uint64

func nextCallID() string {
	return fmt.Sprintf("call_%d", toolCallSeq.Add(1))
}

func normalizeToolAssets(refs []assets.Ref, workspaceRoot, turnID, callID string) []assets.Ref {
	if len(refs) == 0 {
		return refs
	}
	out := make([]assets.Ref, len(refs))
	copy(out, refs)
	workspaceID := ""
	if workspaceRoot != "" {
		workspaceID = assets.WorkspaceID(workspaceRoot)
	}
	for i := range out {
		if out[i].SourceTurnID == "" {
			out[i].SourceTurnID = turnID
		}
		if out[i].SourceCallID == "" {
			out[i].SourceCallID = callID
		}
		if out[i].WorkspaceID == "" && workspaceID != "" {
			out[i].WorkspaceID = workspaceID
		}
		if out[i].URI == "" && out[i].WorkspaceID != "" && out[i].WorkspaceRelativePath != "" {
			out[i].URI = assets.WorkspaceURI(out[i].WorkspaceID, out[i].WorkspaceRelativePath, out[i].Range)
		}
		if out[i].DisplayName == "" && out[i].WorkspaceRelativePath != "" {
			line := 0
			if out[i].Range != nil {
				line = out[i].Range.StartLine
			}
			out[i].DisplayName = assets.DisplayName(out[i].WorkspaceRelativePath, line)
		}
	}
	return out
}

// gatewayImageCaptureAssets converts image output from screenshot, camera, and
// screen-capture tools into Gateway-owned references so the VLM can consume them
// in subsequent turns. It covers both server-side MCP captures (screenshot_capture)
// and client-side captures (capture_photo, take_screenshot).
func (r *Runner) gatewayImageCaptureAssets(ctx context.Context, sess *session.Session, toolName string, refs []assets.Ref) ([]model.GatewayAssetRef, string) {
	if !r.AutoUploadCaptureAssets || r.AssetUploader == nil || !isImageCaptureTool(toolName) {
		return nil, ""
	}
	var out []model.GatewayAssetRef
	for i := range refs {
		ref := &refs[i]
		if !strings.HasPrefix(strings.ToLower(ref.MIMEType), "image/") || ref.AbsolutePath == "" {
			continue
		}
		localPath, err := r.safeWorkspaceAssetPath(ref.AbsolutePath)
		if err != nil {
			// Client-side captures (camera photo, screen capture) may write
			// to a temp directory outside the workspace. Stage the file
			// into the workspace so Gateway upload can proceed.
			if isClientImageCaptureTool(toolName) && r.WorkspaceRoot != "" {
				staged, copyErr := copyAssetToWorkspace(ref.AbsolutePath, r.WorkspaceRoot, ref.MIMEType)
				if copyErr != nil {
					return out, "[asset_unavailable] could not stage client image asset in workspace."
				}
				localPath = staged
				ref.AbsolutePath = staged
			} else {
				return out, "[asset_unavailable] image asset is outside the workspace."
			}
		}
		sha, size, err := fileSHA256(localPath)
		if err != nil {
			return out, "[asset_unavailable] image asset could not be prepared for Gateway."
		}
		scope := "gateway:unknown"
		if scoped, ok := r.AssetUploader.(model.AssetUploadScoper); ok {
			scope = scoped.AssetUploadScope(ctx)
		}
		cacheKey := scope + ":" + ref.ID + ":" + sha
		contentKey := scope + ":" + sha
		if sess != nil && sess.GatewayAssetCache != nil {
			if cached, ok := sess.GatewayAssetCache[cacheKey]; ok {
				out = append(out, cached)
				continue
			}
			if cached, ok := sess.GatewayAssetCache[contentKey]; ok {
				out = append(out, cached)
				continue
			}
		}
		if r.assetUploadCache != nil {
			if cached, ok := r.assetUploadCache[cacheKey]; ok {
				out = append(out, cached)
				continue
			}
		}
		filename := filepath.Base(localPath)
		assetClass, businessType := imageCaptureAssetClasses(toolName)
		gatewayRef, err := r.AssetUploader.UploadAsset(ctx, model.AssetUpload{
			Path: localPath, AssetClass: assetClass, AssetKind: "image", BusinessType: businessType,
			Filename: filename, MIMEType: ref.MIMEType, SizeBytes: size, SHA256: sha,
		})
		if err != nil {
			return out, "[asset_unavailable] image upload to Gateway failed."
		}
		if r.assetUploadCache == nil {
			r.assetUploadCache = make(map[string]model.GatewayAssetRef)
		}
		r.assetUploadCache[cacheKey] = gatewayRef
		if sess != nil {
			if sess.GatewayAssetCache == nil {
				sess.GatewayAssetCache = make(map[string]model.GatewayAssetRef)
			}
			sess.GatewayAssetCache[cacheKey] = gatewayRef
			sess.GatewayAssetCache[contentKey] = gatewayRef
		}
		if ref.Metadata == nil {
			ref.Metadata = map[string]any{}
		}
		ref.Metadata["gateway_asset_id"] = strconv.FormatInt(gatewayRef.AssetID, 10)
		ref.Metadata["gateway_sha256"] = gatewayRef.SHA256
		out = append(out, gatewayRef)
	}
	if len(out) == 0 {
		return nil, "[asset_unavailable] no uploadable image asset found."
	}
	return deduplicateGatewayAssets(out), ""
}

func deduplicateGatewayAssets(refs []model.GatewayAssetRef) []model.GatewayAssetRef {
	if len(refs) < 2 {
		return refs
	}
	seen := make(map[string]bool, len(refs))
	out := make([]model.GatewayAssetRef, 0, len(refs))
	for _, ref := range refs {
		key := strconv.FormatInt(ref.AssetID, 10) + "\x00" + ref.SHA256
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ref)
	}
	return out
}

// isImageCaptureTool reports whether toolName is a tool whose image output
// should be uploaded to Gateway for VLM consumption. It covers:
//   - screenshot_capture (including MCP-prefixed variants)
//   - capture_photo (client-side camera capture)
//   - take_screenshot (client-side screen capture)
func isImageCaptureTool(toolName string) bool {
	if toolName == "screenshot_capture" || strings.HasSuffix(toolName, "__screenshot_capture") {
		return true
	}
	switch toolName {
	case "capture_photo", "take_screenshot":
		return true
	}
	return false
}

// imageCaptureAssetClasses maps an image-capture tool name to Gateway
// (AssetClass, BusinessType) pairs for telemetry downstream.
func imageCaptureAssetClasses(toolName string) (string, string) {
	if toolName == "capture_photo" {
		return "agent_photo", "agent_photo"
	}
	return "agent_screenshot", "agent_screenshot"
}

// isClientImageCaptureTool reports whether toolName is a client-side image
// capture tool (as opposed to a server-side MCP screenshot_capture).
func isClientImageCaptureTool(toolName string) bool {
	switch toolName {
	case "capture_photo", "take_screenshot":
		return true
	}
	return false
}

// copyAssetToWorkspace copies an image file from an arbitrary location into
// .codeagent/assets/client/ under workspaceRoot and returns the new absolute path.
func copyAssetToWorkspace(sourcePath, workspaceRoot, mimeType string) (string, error) {
	ext := extensionForImageMIME(mimeType)
	rel := filepath.ToSlash(filepath.Join(".codeagent", "assets", "client", filepath.Base(sourcePath)))
	if filepath.Ext(rel) == "" || filepath.Ext(rel) != ext {
		rel = strings.TrimSuffix(rel, filepath.Ext(rel)) + ext
	}
	abs := filepath.Join(workspaceRoot, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	src, err := os.Open(sourcePath)
	if err != nil {
		return "", err
	}
	defer src.Close()
	dst, err := os.Create(abs)
	if err != nil {
		return "", err
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return "", err
	}
	return abs, nil
}

// extensionForImageMIME returns the file extension (with dot) for common image
// MIME types. Falls back to ".bin" for unknown types.
func extensionForImageMIME(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0])) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/heic", "image/heif":
		return ".heic"
	default:
		return ".bin"
	}
}

func fileSHA256(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), n, nil
}

func (r *Runner) safeWorkspaceAssetPath(raw string) (string, error) {
	if r.WorkspaceRoot == "" {
		return "", fmt.Errorf("workspace root is required")
	}
	logicalRoot, err := filepath.Abs(r.WorkspaceRoot)
	if err != nil {
		return "", err
	}
	path, err := filepath.Abs(raw)
	if err != nil || (path != logicalRoot && !strings.HasPrefix(path, logicalRoot+string(filepath.Separator))) {
		return "", fmt.Errorf("asset path escapes workspace")
	}
	physicalRoot, err := filepath.EvalSymlinks(logicalRoot)
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil || (path != physicalRoot && !strings.HasPrefix(path, physicalRoot+string(filepath.Separator))) {
		return "", fmt.Errorf("asset path escapes workspace through symlink")
	}
	return path, nil
}

// RunTurn runs one turn of the agent against a persistent session: it appends
// the user's input to the session history, then drives the uniform loop —
// call the model (with tool schemas); if it returns no tool calls, that text is
// the final answer; otherwise execute every tool call, append each result to
// the session, and loop — until a final answer or the per-turn step limit.
//
// The session's Messages survive the call, so the next turn sees this turn's
// full history. The loop contains no per-tool logic and no decision state
// machine: the model owns control flow, the runtime executes and gates tools.
func (r *Runner) RunTurn(ctx context.Context, sess *session.Session, userInput string) (TurnResult, error) {
	return r.RunTurnWithAssets(ctx, sess, userInput, nil)
}

// RunTurnWithAssets carries already-uploaded, Gateway-owned user assets without
// reading their bytes or URLs in the Runtime.
func (r *Runner) RunTurnWithAssets(ctx context.Context, sess *session.Session, userInput string, gatewayAssets []model.GatewayAssetRef) (TurnResult, error) {
	return r.RunTurnWithAllAssets(ctx, sess, userInput, gatewayAssets, nil)
}

// RunTurnWithAllAssets keeps local attachment metadata in Runtime state and
// exposes only a safe textual manifest to Provider requests.
func (r *Runner) RunTurnWithAllAssets(ctx context.Context, sess *session.Session, userInput string, gatewayAssets []model.GatewayAssetRef, localAssets []model.LocalAssetRef) (TurnResult, error) {
	if len(gatewayAssets) > 0 && !r.UserAssetsSupported {
		return TurnResult{}, UserAssetRuntimeError{Code: "image_input_unsupported", Message: "The selected model cannot process image input"}
	}
	if len(gatewayAssets) > 0 && r.RequestID == "" {
		return TurnResult{}, errors.New("user assets require a stable request id")
	}
	if r.Model == nil {
		return TurnResult{}, errors.New("missing model provider")
	}
	if r.Tools == nil {
		r.Tools = tools.NewRegistry()
	}
	if r.MaxSteps <= 0 {
		r.MaxSteps = defaultMaxSteps
	}

	r.emitSessionID = sess.ID
	r.emitTurnID = r.ReservedTurnID
	if r.emitTurnID == "" {
		r.emitTurnID = nextSessionTurnID(sess)
	}
	r.emitInvocationID = "" // cleared each turn; set per model call
	r.emitRequestID = r.RequestID

	// Repair legacy provider-invalid history before appending the new user input.
	// The invalid assistant tool call and everything after it are not safe to send
	// back to an OpenAI-compatible provider.
	if repair := sess.TruncateInvalidToolCallTail(); repair != nil {
		r.emit(Event{
			Kind: EventSessionRepaired,
			Text: fmt.Sprintf(
				"Truncated %d messages from index %d: %v",
				repair.Removed,
				repair.FromIndex,
				repair.Reason,
			),
		})
	}

	// Repair sessions written by older runtimes before appending a new user
	// message. Empty assistant no-ops are invalid provider input but are never
	// needed to preserve a tool-call/result pairing.
	sess.RemoveEmptyAssistantNoOps()

	// Append the user's turn to the persistent session history.
	sess.Messages = append(sess.Messages, model.Message{
		Role:        model.RoleUser,
		Content:     userInput,
		Assets:      gatewayAssets,
		LocalAssets: append([]model.LocalAssetRef(nil), localAssets...),
	})
	sess.UpdatedAt = time.Now()
	r.emit(Event{
		Kind: EventTurnStarted, Text: userInput,
		UserAssets:  append([]model.GatewayAssetRef(nil), gatewayAssets...),
		LocalAssets: append([]model.LocalAssetRef(nil), localAssets...),
	})

	return r.drive(ctx, sess)
}

// RunPreparedTurn executes the last user message after TurnExecutor atomically
// persisted it with the durable inbox's running transition.
func (r *Runner) RunPreparedTurn(ctx context.Context, sess *session.Session) (TurnResult, error) {
	if r.Model == nil {
		return TurnResult{}, errors.New("missing model provider")
	}
	if len(sess.Messages) == 0 || sess.Messages[len(sess.Messages)-1].Role != model.RoleUser {
		return TurnResult{}, errors.New("prepared turn is missing its user message")
	}
	if r.Tools == nil {
		r.Tools = tools.NewRegistry()
	}
	if r.MaxSteps <= 0 {
		r.MaxSteps = defaultMaxSteps
	}
	r.emitSessionID = sess.ID
	r.emitTurnID = r.ReservedTurnID
	if r.emitTurnID == "" {
		r.emitTurnID = nextSessionTurnID(sess)
	}
	r.emitInvocationID = ""
	r.emitRequestID = r.RequestID
	user := sess.Messages[len(sess.Messages)-1]
	if len(user.Assets) > 0 && !r.UserAssetsSupported {
		return TurnResult{}, UserAssetRuntimeError{Code: "image_input_unsupported", Message: "The selected model cannot process image input"}
	}
	if len(user.Assets) > 0 && r.RequestID == "" {
		return TurnResult{}, errors.New("user assets require a stable request id")
	}
	r.emit(Event{
		Kind: EventTurnStarted, Text: user.Content,
		UserAssets:  append([]model.GatewayAssetRef(nil), user.Assets...),
		LocalAssets: append([]model.LocalAssetRef(nil), user.LocalAssets...),
	})
	return r.drive(ctx, sess)
}

func withLocalAssetManifests(messages []model.Message) []model.Message {
	var out []model.Message
	for i := range messages {
		if len(messages[i].LocalAssets) == 0 {
			continue
		}
		if out == nil {
			out = append([]model.Message(nil), messages...)
		}
		var manifest strings.Builder
		manifest.WriteString("\n\n[Local attachment manifest]\n")
		manifest.WriteString("The following files are stored in the workspace. Their contents are currently NOT visible to you. ")
		manifest.WriteString("Do not guess their contents. Use an appropriate client-side local tool such as analyze_local_image, read_pdf, or render_pdf_pages to inspect them.\n")
		for _, asset := range messages[i].LocalAssets {
			fmt.Fprintf(&manifest, "- path=%q; filename=%q; kind=%q; mime_type=%q; size_bytes=%d\n",
				asset.RelativePath, asset.Filename, asset.Kind, asset.MIMEType, asset.SizeBytes)
		}
		out[i].Content += manifest.String()
		// Defense in depth: a Provider inspecting Request directly receives no
		// structured local attachment metadata.
		out[i].LocalAssets = nil
	}
	if out == nil {
		return messages
	}
	return out
}

type UserAssetRuntimeError struct {
	Code    string
	Message string
}

func (e UserAssetRuntimeError) Error() string              { return e.Message }
func (e UserAssetRuntimeError) LifecycleErrorCode() string { return e.Code }

// ResumeTurn continues an interrupted turn from the persisted history WITHOUT
// appending a new user message (v1.2 §3.2). It is the resume counterpart to
// RunTurn: the session's Messages already end at a consistent boundary (a
// balanced tool batch, or a user message whose turn was cut off before the first
// model call — guaranteed by the cancel-mid-batch fill and per-iteration
// checkpoint), so re-entering the same loop simply re-issues the interrupted
// step. The caller (the serve/embedded lifecycle layer) invokes this for a
// session whose turn_status is paused.
func (r *Runner) ResumeTurn(ctx context.Context, sess *session.Session) (TurnResult, error) {
	if r.Model == nil {
		return TurnResult{}, errors.New("missing model provider")
	}
	if r.Tools == nil {
		r.Tools = tools.NewRegistry()
	}
	if r.MaxSteps <= 0 {
		r.MaxSteps = defaultMaxSteps
	}

	r.emitSessionID = sess.ID
	// Continue under a fresh turn id: the paused turn's events are already
	// persisted, and a new id keeps the resumed run's events unambiguous while
	// still correlating to the same session.
	r.emitTurnID = r.ReservedTurnID
	if r.emitTurnID == "" {
		r.emitTurnID = nextSessionTurnID(sess)
	}
	r.emitInvocationID = ""
	r.emitRequestID = r.RequestID

	if repair := sess.TruncateInvalidToolCallTail(); repair != nil {
		r.emit(Event{
			Kind: EventSessionRepaired,
			Text: fmt.Sprintf(
				"Truncated %d messages from index %d: %v",
				repair.Removed,
				repair.FromIndex,
				repair.Reason,
			),
		})
	}
	sess.RemoveEmptyAssistantNoOps()

	sess.UpdatedAt = time.Now()
	r.emit(Event{Kind: EventTurnResumed})

	return r.drive(ctx, sess)
}

// drive runs the uniform agent loop over the session's current history until a
// final answer or the per-turn step limit. It is shared by RunTurn (which
// appends the user message first) and ResumeTurn (which does not), so both paths
// execute identical control flow.
func (r *Runner) drive(ctx context.Context, sess *session.Session) (TurnResult, error) {
	turn := TurnResult{TurnID: r.emitTurnID}

	// Reflection (P4.3) per-turn state: at most one self-check pass, and the
	// ephemeral nudge to apply on the next request once it fires.
	r.changeReviewCount = 0 // one passing review per mutation; re-armed on any new mutation
	r.todos = nil           // per-turn checklist; rewritten on each todo_write
	pendingHarness := ""
	// Stop policy (8.5): the finalize decision point. The configured policy wins;
	// nil selects the built-in default — one instance per turn, so its one-shot
	// flags reset here.
	// StopPolicy is nil by default: the model controls when to stop. A
	// configured policy (VerifyGate, TodoGate, after_turn hook, etc.) replaces
	// this trust-the-model default. The step budget remains the hard backstop.
	stopPol := r.StopPolicy

	// Pre-mutation self-check (P4.3-R Move 3) per-turn state: at most one
	// hypothesis nudge before an edit that follows a failure, and the ephemeral
	// nudge to apply on the next request once it fires.
	hypothesized := false
	pendingHypothesis := ""

	// Per-turn web_search budget. Counts continuously across this turn (it resets
	// when Run returns and the next user turn starts a fresh counter). A
	// search-happy model reformulating the same query is cut off past the cap.
	webSearches := 0
	var turnAssets []assets.Ref

	for i := 0; i < r.MaxSteps; i++ {
		// A canceled context (Ctrl-C) must stop the turn at the step boundary
		// without waiting for the next HTTP call to time out.
		if err := ctx.Err(); err != nil {
			return turn, err
		}
		// Compact before each model call, not just at the turn boundary: a single
		// turn with many tool calls can grow the prompt past the threshold mid-loop.
		if err := r.maybeCompact(ctx, sess); err != nil {
			return turn, err
		}

		// Recompute the toolset each iteration: plan mode can be entered or
		// exited mid-turn by enter_plan_mode / propose_plan, so the tool list
		// must reflect the current planState.
		activeTools := r.Tools
		if (r.PlanState == PlanStatusPlanning || r.PlanState == PlanStatusProposing) && r.PlanTools != nil {
			activeTools = r.PlanTools
		}
		toolDefs := toolDefinitions(activeTools)
		advertised := make(map[string]bool, len(toolDefs))
		for _, d := range toolDefs {
			advertised[d.Function.Name] = true
		}

		// Convergence nudge: once a turn has made many tool calls, steer a model
		// that lacks agentic restraint toward answering. The nudge is ephemeral —
		// appended only to this request, never persisted — so it keeps applying
		// pressure without polluting history. (max_steps still backstops it.)
		msgs := sess.Messages
		if n := len(turn.Steps); r.PlanState != PlanStatusPlanning && n >= r.nudgeThreshold() {
			msgs = withConvergenceNudge(sess.Messages, n)
		}
		// Stop-policy nudge (8.5): apply the finalize decision's message once,
		// ephemerally — the same non-persisted mechanism as the convergence nudge.
		if pendingHarness != "" {
			msgs = appendEphemeralUser(msgs, pendingHarness)
			pendingHarness = ""
		}
		// Pre-mutation self-check nudge (P4.3-R Move 3): same ephemeral mechanism,
		// fired before an edit that follows a failure rather than at the finish line.
		if pendingHypothesis != "" {
			msgs = appendEphemeralUser(msgs, pendingHypothesis)
			pendingHypothesis = ""
		}
		// Skills reminder (P6): on the first model call of a turn, remind the model
		// to load a matching skill. Ephemeral, and the model still decides — this
		// makes skill-loading consistent across models instead of depending on a
		// model's agency to act on the index unprompted.
		if i == 0 && r.RemindSkills {
			msgs = appendEphemeralUser(msgs, skillsReminder)
		}
		// Parallelism guidance (P8.8 §7): on the first call, teach the model to
		// fan out independent-heavy work and keep dependent/trivial work serial.
		// Only when parallel execution is actually enabled.
		if i == 0 && r.RemindParallel {
			msgs = appendEphemeralUser(msgs, parallelReminder)
		}
		// Plan mode (read-only): steer the model to produce a plan, not changes. The
		// read-only toolset already prevents edits; this shapes the output.
		// Plan mode: when in Planning state, inject the planning guidance prompt.
		// The restricted toolset already prevents edits; this shapes the output.
		if r.PlanState == PlanStatusPlanning {
			msgs = appendEphemeralUser(msgs, planningPrompt)
		}
		msgs = withLocalAssetManifests(msgs)

		r.emitInvocationID = newInvocationID()
		r.emit(Event{Kind: EventModelStarted})
		modelStart := time.Now()
		var streamedText, streamedReasoning strings.Builder
		resp, err := r.complete(ctx, model.Request{
			Model:       r.ModelName,
			Temperature: r.Temperature,
			Messages:    msgs,
			Tools:       toolDefs,
		}, &streamedText, &streamedReasoning)

		// Ordinary assistant content and provider reasoning are independent
		// channels. Keep the former only for propose_plan's legacy content fallback;
		// publish the latter as the authoritative, persisted thinking snapshot.
		assistantText := resp.Content
		if assistantText == "" && err != nil {
			assistantText = strings.TrimSpace(streamedText.String())
		}
		if assistantText != "" {
			r.lastAssistantText = assistantText
		}
		reasoning := resp.ReasoningContent
		if reasoning == "" && err != nil {
			reasoning = strings.TrimSpace(streamedReasoning.String())
		}
		// A clean cancellation is resumed and regenerated, so persisting its
		// incomplete reasoning would add noise. Other failures retain the partial
		// snapshot for diagnostics and faithful transcript replay.
		if reasoning != "" && !errors.Is(err, context.Canceled) {
			r.emit(Event{Kind: EventThinking, Text: reasoning})
		}
		// Always emit ModelFinished, even on error: it pairs with ModelStarted, so
		// a live renderer's "Thinking…" ticker is always stopped (no leaked timer).
		// Errors that return from this call always terminate the turn. Keep this
		// pairing event error-free: the executor emits the same failure once as
		// turn_failed, the authoritative terminal event. Putting Err on both
		// events creates two persisted, user-visible copies of one failure.
		r.emit(Event{
			Kind:             EventModelFinished,
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
			BillingUnits:     resp.Usage.BillingUnits,
			Elapsed:          time.Since(modelStart),
		})
		turn.ModelBillingUnits += resp.Usage.BillingUnits
		turn.BillingUnits += resp.Usage.BillingUnits
		if err != nil {
			return turn, err
		}

		// Some OpenAI-compatible providers occasionally return a tool call with
		// an empty id. Assign a stable, unique id here so the echoed assistant
		// message and the tool result messages reference the SAME non-empty
		// tool_call_id — otherwise the API rejects the next request with
		// "insufficient tool messages following tool_calls message".
		for j := range resp.ToolCalls {
			if resp.ToolCalls[j].ID == "" {
				resp.ToolCalls[j].ID = nextCallID()
			}
			if resp.ToolCalls[j].Type == "" {
				resp.ToolCalls[j].Type = "function"
			}
		}

		// A provider may end a successful HTTP/SSE exchange without emitting
		// either text or tool calls. Never persist that no-op as an assistant
		// message: OpenAI-compatible providers reject it on the next request.
		if err := resp.ValidateAssistantTurn(); err != nil {
			return turn, err
		}

		turn.PromptTokens = resp.Usage.PromptTokens
		turn.TokensUsed += resp.Usage.PromptTokens + resp.Usage.CompletionTokens
		sess.PromptTokens = resp.Usage.PromptTokens
		// This call's prompt size is the true post-compaction size if a compaction
		// just ran, so finalize its observability stat here. A measurement still at
		// or above the threshold means the compaction was ineffective — the session
		// cools down (see NeedCompaction) and the event says so out loud, because
		// the silent alternative is an endless compact-measure-compact loop.
		if stat := sess.FinalizeCompaction(resp.Usage.PromptTokens); stat != nil {
			r.emit(Event{
				Kind:         EventCompacted,
				BeforeTokens: stat.BeforeTokens,
				AfterTokens:  stat.AfterTokens,
				SavedTokens:  stat.SavedTokens,
				Ratio:        stat.CompressionRatio,
				SummaryChars: stat.SummaryChars,
				Ineffective:  sess.CompactThreshold > 0 && stat.AfterTokens >= sess.CompactThreshold,
			})
		}

		// No tool calls => the model wants to finish.
		if !resp.HasToolCalls() {
			// Planning is a compilation phase, not an answer-writing phase. Plain
			// assistant text cannot silently escape it: only propose_plan may move
			// the state machine forward.
			if r.PlanState == PlanStatusPlanning {
				pendingHarness = planningContinuationPrompt
				r.emit(Event{Kind: EventReflected, Text: pendingHarness})
				continue
			}
			// Stop policy (8.5): an optional finalize decision point. When nil
			// (the default), the model controls when to stop — its first "done"
			// is accepted immediately. A configured policy (VerifyGate, TodoGate,
			// after_turn hook, etc.) owns the judgment and may re-prompt with
			// an ephemeral nudge. The plan-state gate above stays in the loop
			// because it is a protocol invariant, not a judgment.
			if stopPol != nil {
				sc := StopContext{
					LastText:    resp.Content,
					Todos:       r.todos,
					Steps:       turn.Steps,
					PlanState:   r.PlanState,
					ToolCalls:   turn.ExecutedToolCalls,
					MaxSteps:    r.MaxSteps,
				}
				verdict, err := stopPol.Decide(ctx, sc)
				if err != nil {
					// Fail-closed: a policy that errors must not silently
					// accept the finish. The step budget backstops the delay.
					verdict = StopVerdict{Continue: true, Message: "[policy] The stop policy could not decide: " + err.Error()}
				}
				if verdict.Continue {
					if verdict.Message != "" {
						pendingHarness = verdict.Message
						r.emit(Event{Kind: EventReflected, Text: verdict.Message})
					}
					continue
				}
			}
			sess.Messages = append(sess.Messages, resp.AssistantMessage())
			sess.UpdatedAt = time.Now()
			turn.Final = resp.Content
			r.emit(Event{
				Kind:               EventTurnFinished,
				Text:               turn.Final,
				TextAnnotations:    annotateTextWithAssets(turn.Final, turnAssets),
				BillingUnits:       turn.BillingUnits,
				ModelBillingUnits:  turn.ModelBillingUnits,
				ToolBillingUnits:   turn.ToolBillingUnits,
				ExecutedToolCalls:  turn.ExecutedToolCalls,
				SucceededToolCalls: turn.SucceededToolCalls,
				BillableToolCalls:  turn.BillableToolCalls,
			})
			return turn, nil
		}

		// Pre-mutation self-check (P4.3-R Move 3): the model is about to edit code
		// and a failure has already surfaced this turn — the paper-over hot zone.
		// Before the edit lands, drop this response (do NOT persist or execute it —
		// same discipline as the finalize self-check) and re-prompt with a one-shot
		// ephemeral nudge to state a root-cause hypothesis first. The model decides
		// whether it was fixing the cause or papering over the symptom.
		if r.RemindHypothesis && !hypothesized && aboutToMutate(resp.ToolCalls) &&
			reflection.SawFailure(stepViews(turn.Steps)) {
			hypothesized = true
			pendingHypothesis = hypothesisReminder
			r.emit(Event{Kind: EventPreMutation, Text: hypothesisReminder})
			continue
		}

		// Tool-call path: the assistant turn must precede its tool results in
		// history (the API requires the tool_calls message before the answers).
		sess.Messages = append(sess.Messages, resp.AssistantMessage())
		sess.UpdatedAt = time.Now()

		// Execute the tool-call batch. Independent read-only calls run
		// concurrently (P8.8, bounded by MaxParallelTools); side-effecting calls
		// are serialized barriers; results commit in model order. With
		// MaxParallelTools <= 1 this is exactly the old sequential loop. A non-nil
		// return means the batch was cut short by cancellation — history is left
		// balanced (every call has a result) so the session stays resumable.
		if cancelErr := r.executeToolBatch(ctx, sess, &turn, resp.ToolCalls, activeTools, advertised, &webSearches, &turnAssets); cancelErr != nil {
			return turn, cancelErr
		}

		// Checkpoint at this consistent boundary (v1.2 §2): the tool batch is
		// complete and history is balanced, so a hard process kill (iOS jetsam)
		// before the next model call loses at most the in-progress step, not the
		// whole turn. Best-effort — never fails the turn.
		r.checkpoint(ctx, sess)
	}

	// Step limit reached. Don't discard the work: the model has gathered tool
	// results in the history, so give it one final tool-free call to answer from
	// what it has — instead of a useless "stopped" message that forces the user to
	// re-ask (and re-pay for the whole investigation).
	if r.PlanState == PlanStatusPlanning {
		turn.Final = "Planning paused at the step limit before a review-ready plan was proposed. Continue discovery, then call propose_plan."
		turn.HitStepLimit = true
		r.emit(Event{Kind: EventTurnFinished, Text: turn.Final})
		return turn, nil
	}
	if r.independentTaskAvailable() && r.plannedMutation && !r.independentReviewPassed &&
		(r.mutatedVerifiableCode || r.Reflector == nil) && r.changeReviewCount < 1 {
		turn.Final = "Step limit reached with unreviewed code changes. Continue with a change_review task before finalizing."
		turn.HitStepLimit = true
		r.emit(Event{Kind: EventTurnFinished, Text: turn.Final})
		return turn, nil
	}
	final, finalTokens, finalBillingUnits := r.finalAnswerAfterLimit(ctx, sess)
	turn.Final = final
	turn.TokensUsed += finalTokens
	turn.ModelBillingUnits += finalBillingUnits
	turn.BillingUnits += finalBillingUnits
	turn.HitStepLimit = true
	r.emit(Event{
		Kind:               EventTurnFinished,
		Text:               turn.Final,
		TextAnnotations:    annotateTextWithAssets(turn.Final, turnAssets),
		BillingUnits:       turn.BillingUnits,
		ModelBillingUnits:  turn.ModelBillingUnits,
		ToolBillingUnits:   turn.ToolBillingUnits,
		ExecutedToolCalls:  turn.ExecutedToolCalls,
		SucceededToolCalls: turn.SucceededToolCalls,
		BillableToolCalls:  turn.BillableToolCalls,
	})
	return turn, nil
}

// nudgeThreshold is the tool-call count at which the convergence nudge starts —
// half the step budget, with a floor so short budgets don't nudge too eagerly.
func (r *Runner) nudgeThreshold() int {
	t := r.MaxSteps / 2
	if t < 6 {
		t = 6
	}
	return t
}

// maxWebSearches is the per-turn web_search budget (configurable, with a default).
func (r *Runner) maxWebSearches() int {
	if r.MaxWebSearches > 0 {
		return r.MaxWebSearches
	}
	return defaultMaxWebSearches
}

// skillsReminder is the ephemeral first-call nudge (P6). It is phrased to be
// safe across turns ("not already loaded") so a skill loaded in an earlier turn
// is not redundantly re-loaded.
const skillsReminder = "[reminder] Before you act: check the Skills list in the system prompt. " +
	"If this task matches a skill you have not already loaded, call load_skill(name) and follow " +
	"it first — that is reading project guidance, not extra investigation."

// parallelReminder is the first-call orchestration nudge (P8.8 §7), injected
// only when parallel execution is enabled. It is the harder half of the
// parallelism work: the mechanism is worthless if the model does not know WHEN
// to fan out. Kept short — it is re-injected each turn.
const parallelReminder = "[reminder] Independent tool calls run in parallel. When the work splits into " +
	"INDEPENDENT subtasks that are each heavy (each would otherwise mean reading many files you " +
	"won't reuse, or take real time), issue their calls TOGETHER in one message — delegate several " +
	"`task` subagents at once, or read several files / run several searches at once. They run " +
	"concurrently. But do NOT fan out when: (a) the work is trivial (one small read — just do it), " +
	"or (b) a step DEPENDS on a previous step's result — for a dependent chain, do one call, use its " +
	"result, then the next. Parallelism is for breadth, not for depth. Fanning out dependent or " +
	"trivial work only multiplies cost."

// hypothesisReminder is the pre-mutation self-check nudge (P4.3-R Move 3),
// injected once when the model is about to edit code after a failure surfaced
// this turn. It reinforces the system prompt's "state your hypothesis BEFORE the
// deep dive" directive at the exact moment it matters — the edit that risks
// papering over a symptom.
const hypothesisReminder = "[reflection] A build or test failed this turn and you are about to edit code. " +
	"Before you do, state your root-cause hypothesis in one or two sentences: what is actually wrong, and why " +
	"this edit fixes the CAUSE — not just makes the failure disappear. If you are about to change a test or a " +
	"value so the failure goes away, stop and fix the source instead. If you are confident in the cause, say it, " +
	"then make your edit."

// aboutToMutate reports whether a response's tool calls include a project-file
// mutation — the trigger boundary for the pre-mutation self-check (Move 3).
func aboutToMutate(calls []model.ToolCall) bool {
	for _, c := range calls {
		if reflection.IsMutatingTool(c.Function.Name) {
			return true
		}
	}
	return false
}

// VerifyStatus is the honest tri-state outcome of a finalize verify run: the
// change passed, the change genuinely failed the verify, or the verify could
// not run at all (environment problem — never a verdict on the change).
type VerifyStatus int

const (
	VerifyPassed VerifyStatus = iota
	VerifyFailed
	VerifyCouldNotRun
)

// runFinalizeVerify runs the configured VerifyCommand once at the finalize
// boundary (P4.3-R Move 2, option 2a — the port of Claude Code's Stop hook) and
// classifies the outcome via the Observer. It is a runtime action, not a
// fabricated model turn: the real result is fed back to the model only on
// failure. Guards keep it safe — it declines (reporting Passed, i.e. "no
// objection") when run_command is unavailable or the command would mutate the
// workspace, so it never auto-runs a side-effecting command outside approval.
func (r *Runner) runFinalizeVerify(ctx context.Context, command string) (VerifyStatus, string) {
	tool, ok := r.Tools.Get("run_command")
	if !ok {
		return VerifyPassed, ""
	}
	input, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		return VerifyPassed, ""
	}
	if tools.HasSideEffectsFor(tool, input) {
		return VerifyPassed, "" // never auto-run a mutating command
	}
	result, execErr := r.executeTool(ctx, tool, nextCallID(), input)
	if execErr != nil {
		return VerifyCouldNotRun, "verify command could not run: " + execErr.Error()
	}
	// Classify via the structured Observation. FailureEnvironment (exit -1,
	// command never started) is a could-not-run — the runtime must NEVER report
	// it as "the verification FAILED" and tell the model to fix the cause.
	// With no Observer, an unclassifiable result is a pass (no objection).
	summary := ""
	if r.Observer != nil {
		obs := r.Observer.Observe("run_command", result.Content)
		if obs.FailureType == observation.FailureEnvironment {
			if obs.Summary == "" {
				obs.Summary = "command could not run (exit -1)"
			}
			return VerifyCouldNotRun, obs.Summary
		}
		summary = obs.Summary
		if reflection.SawFailure([]reflection.StepView{{Observation: obs.Render(result.Content)}}) {
			if summary == "" {
				summary = "verification failed"
			}
			return VerifyFailed, summary
		}
	}
	return VerifyPassed, summary
}

// planningPrompt is injected as an ephemeral user message when the model enters the
// Planning state (via enter_plan_mode or /plan). The restricted toolset already
// blocks project edits; this tells the model what to produce instead.
const planningPrompt = "[plan mode] You are in PLAN MODE. You can read, search, and write plan " +
	"files to .codeagent/plans/, but you CANNOT edit project files or run commands. " +
	"Treat this as a discovery and design phase: research thoroughly, then produce a concrete implementation plan. " +
	"Your plan should describe WHAT to do and WHY — do NOT write pseudo-code, function signatures, " +
	"switch/case blocks, or method call chains. Those are implementation details; the compiler and " +
	"tests verify them. If you find yourself writing 5+ lines of code-like content, stop: that " +
	"belongs in the implementation phase, not the plan.\n" +
	"Your plan should include:\n" +
	"1. Problem summary — what needs to be done\n" +
	"2. Files to change — list each file and what changes\n" +
	"3. Approach — the implementation strategy and key design decisions\n" +
	"4. Step-by-step order — the sequence of changes\n" +
	"5. Evidence — concrete workspace-relative files and findings that support the design\n" +
	"6. Constraints and unknowns — resolve blocking unknowns with ask_user; do not propose while any remain\n" +
	"7. Risks and edge cases — what could go wrong and how to handle it\n" +
	"8. Verification — exact tests/checks that will prove the implementation\n" +
	"When your plan is complete, write it to a markdown file under .codeagent/plans/, " +
	"then call propose_plan with plan_path, evidence_paths, verification, and blocking_unknowns. " +
	"Keep the accompanying text brief; the plan file is the authoritative content " +
	"submitted for approval. Do NOT make any project changes. " +
	"You may track your plan's steps with todo_write."

const planningContinuationPrompt = "[plan mode] Plain assistant text cannot finish PLAN MODE. " +
	"Continue discovery. Write the authoritative plan under .codeagent/plans/, " +
	"then call propose_plan with the readiness evidence."

// withConvergenceNudge returns a copy of msgs with a transient reminder appended,
// steering the model to answer now instead of over-investigating.
func withConvergenceNudge(msgs []model.Message, toolCalls int) []model.Message {
	return appendEphemeralUser(msgs, fmt.Sprintf("[reminder] You have already made %d tool calls and very"+
		" likely have enough to answer. Unless you are genuinely blocked, stop calling tools and give your"+
		" final answer now. Do not re-run similar queries to double-check.", toolCalls))
}

// appendEphemeralUser returns a copy of msgs with a transient user message
// appended. Both the convergence nudge and the reflection nudge use it: the
// message shapes the next request only and is never persisted to the session.
func appendEphemeralUser(msgs []model.Message, content string) []model.Message {
	out := make([]model.Message, len(msgs), len(msgs)+1)
	copy(out, msgs)
	return append(out, model.Message{Role: model.RoleUser, Content: content})
}

// staleDateLine matches the date line an older session.Builder baked into the
// persisted system message. Sessions created by that version carry a date frozen
// at creation time; withCurrentDate strips it so the model sees exactly one date,
// and it is today's.
var staleDateLine = regexp.MustCompile(`\n\nThe current date is \d{4}-\d{2}-\d{2} \([A-Za-z]+\)\.`)

// withCurrentDate returns msgs with today's date appended to the system message.
// Ephemeral like the nudges — applied per request, never persisted — so the date
// stays correct across midnight and on resumed sessions, instead of freezing at
// whatever day the session was created. Without it the model does not know the
// date at all and silently falls back to its training-era present (e.g. searching
// "news today" as a year-old date).
func withCurrentDate(msgs []model.Message, now time.Time) []model.Message {
	if len(msgs) == 0 || msgs[0].Role != model.RoleSystem {
		return msgs
	}
	out := make([]model.Message, len(msgs))
	copy(out, msgs)
	content := staleDateLine.ReplaceAllString(out[0].Content, "")
	out[0].Content = content + "\n\nThe current date is " + now.Format("2006-01-02 (Monday)") + "."
	return out
}

// injectGitInfo appends a fresh git status snapshot to the system message on
// every model request. When a GitCache is configured, repeated calls within the
// same turn reuse a cached snapshot (TTL + dirty-flag) instead of re-running
// git(1) on every model call.
func (r *Runner) injectGitInfo(msgs []model.Message) []model.Message {
	if len(msgs) == 0 || msgs[0].Role != model.RoleSystem {
		return msgs
	}
	var info string
	if r.GitCache != nil {
		info = r.GitCache.GitInfo()
	} else {
		info = session.GitInfo(r.WorkspaceRoot)
	}
	if info == "" {
		return msgs
	}
	out := make([]model.Message, len(msgs))
	copy(out, msgs)
	out[0].Content = out[0].Content + "\n\n" + info
	return out
}

const (
	stepLimitMessage = "Agent stopped: reached the step limit before finishing."
	stepLimitNudge   = "You've reached the step limit and cannot call more tools. Give your best final answer now, based on everything gathered so far."
)

// toolInterruptedObservation is the placeholder tool result written for a call
// that never ran because the turn was cancelled/suspended mid-batch. It keeps the
// assistant tool_calls message balanced (one result per call) so the persisted
// history is valid to resend, and tells the model the call did not execute so it
// re-issues if still needed (v1.2 §2.2).
const toolInterruptedObservation = "This tool call did not run: the turn was interrupted (app suspended or cancelled) before it executed. Re-issue the call if the result is still needed."

// finalAnswerAfterLimit makes one tool-free model call so the agent answers from
// what it already gathered when the step limit is hit. The nudge is ephemeral
// (not persisted); only the answer is appended to history, so the conversation
// continues cleanly.
// finalAnswerAfterLimit returns the best-effort answer AND the tokens that one
// call consumed, so the turn-level counter (TurnResult.TokensUsed) stays exact
// even on the step-limit path.
func (r *Runner) finalAnswerAfterLimit(ctx context.Context, sess *session.Session) (string, int, int64) {
	if ctx.Err() != nil {
		return stepLimitMessage, 0, 0
	}
	msgs := make([]model.Message, len(sess.Messages), len(sess.Messages)+1)
	copy(msgs, sess.Messages)
	msgs = append(msgs, model.Message{
		Role:    model.RoleUser,
		Content: stepLimitNudge,
	})
	msgs = withLocalAssetManifests(msgs)

	r.emitInvocationID = newInvocationID()
	r.emit(Event{Kind: EventModelStarted})
	start := time.Now()
	resp, err := r.complete(ctx, model.Request{
		Model:       r.ModelName,
		Temperature: r.Temperature,
		Messages:    msgs,
		// No Tools: the model must answer with text, not request more tools.
	}, nil, nil)
	if resp.ReasoningContent != "" && !errors.Is(err, context.Canceled) {
		r.emit(Event{Kind: EventThinking, Text: resp.ReasoningContent})
	}
	r.emit(Event{Kind: EventModelFinished, PromptTokens: resp.Usage.PromptTokens, CompletionTokens: resp.Usage.CompletionTokens, TotalTokens: resp.Usage.TotalTokens, BillingUnits: resp.Usage.BillingUnits, Elapsed: time.Since(start), Err: errString(err)})
	tok := resp.Usage.PromptTokens + resp.Usage.CompletionTokens
	// A leaked tool-call markup (deepseek, when forced to answer with no tools) is
	// not an answer — don't show the user noise or persist it; fall back cleanly.
	// The call still consumed tokens, so report them regardless.
	if err != nil || resp.Content == "" || LooksLikeToolCallLeak(resp.Content) {
		return stepLimitMessage, tok, resp.Usage.BillingUnits
	}

	sess.Messages = append(sess.Messages, model.Message{Role: model.RoleAssistant, Content: resp.Content})
	sess.PromptTokens = resp.Usage.PromptTokens
	sess.UpdatedAt = time.Now()
	return resp.Content, tok, resp.Usage.BillingUnits
}

// Checkpointer persists the session mid-turn, at the consistent boundary after
// each completed loop iteration (v1.2 §2). It bounds the blast radius of a hard
// process kill (iOS jetsam) to the in-progress step, whereas the caller's
// turn-boundary Save is the backstop. Implementations must be crash-safe and
// SHOULD detach cancellation (persist even as the turn is being suspended).
type Checkpointer interface {
	Checkpoint(ctx context.Context, sess *session.Session) error
}

// checkpoint persists the session at a consistent boundary. Nil-safe and
// best-effort: a checkpoint failure is swallowed so it never fails the turn — the
// turn-boundary Save surfaces persistent errors.
func (r *Runner) checkpoint(ctx context.Context, sess *session.Session) {
	if r.Checkpointer == nil {
		return
	}
	_ = r.Checkpointer.Checkpoint(ctx, sess)
}

// maybeCompact compacts the session when it has grown past the token threshold.
// It is best-effort: a nil Compactor means the caller opted out.
//
// PromptTokens is deliberately NOT reset afterwards. The pre-compaction count is
// stale, but faking a 0-token state would hide a compaction that failed to get
// under the window. Instead the next model call (immediately after this, at the
// top of the loop) measures the true reduced size and refreshes PromptTokens —
// which also finalizes the observability stat. A compaction that changed nothing
// (the recent window already is the whole history) is not recorded, and a
// measured compaction that stayed over the threshold puts the session in
// cooldown (see Session.NeedCompaction) instead of retrying every iteration.
func (r *Runner) maybeCompact(ctx context.Context, sess *session.Session) error {
	if r.Compactor == nil || !sess.NeedCompaction() {
		return nil
	}

	// Adaptive cooldown (P12.b extended): require ≥N% growth past the
	// post-compaction size before re-triggering. N adapts to observed
	// prompt-cache performance — a healthy cache gets a higher bar (don't
	// break a working prefix), a cold cache gets a lower one (reclaim
	// context aggressively).
	if last := sess.LastCompaction(); last != nil && last.Finalized() {
		growthRequired := int(float64(sess.CompactThreshold) * r.effectiveCooldownRatio())
		if sess.PromptTokens < last.AfterTokens+growthRequired {
			return nil
		}
	}

	before := sess.PromptTokens

	// Context editing: clear stale tool results before the LLM summarizer
	// runs. Free (no LLM call), reduces tokens the model must read.
	if r.ContextEditor != nil {
		if n := r.ContextEditor.Edit(sess); n > 0 {
			r.emit(Event{Kind: EventContextEdited, Text: fmt.Sprintf("cleared %d stale tool results", n)})
		}
	}

	// Tier-0 (P12.c): deterministic pruning first — zero LLM cost. When its
	// estimated savings already put the prompt back under the threshold, skip
	// the summarize call entirely this round; the next model call measures the
	// truth either way, and if it disagrees this path re-runs with nothing left
	// to prune and falls through to the summarizer.
	if r.CompactKeepTokens > 0 {
		saved, truncated, toolMsgs := session.PruneOldContext(sess, r.CompactKeepTokens)
		if saved > 0 {
			savedTokens := saved / 4
			r.emit(Event{Kind: EventContextPruned, BeforeTokens: before, SavedTokens: savedTokens})
			fragmented := toolMsgs > 0 && float64(truncated)/float64(toolMsgs) > 0.3
			if !fragmented && before-savedTokens < sess.CompactThreshold {
				return nil
			}
		}
	}

	prevLen, prevSummary := len(sess.Messages), sess.Summary
	if err := r.Compactor.Compact(ctx, sess); err != nil {
		return err
	}
	if len(sess.Messages) == prevLen && sess.Summary == prevSummary {
		return nil // nothing was folded
	}
	sess.RecordCompaction(before, len(sess.Summary))
	return nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// complete calls the model and keeps final-answer text and provider-visible
// reasoning in separate channels. The optional builders retain partial streams
// when a provider fails before it can return its accumulated Response.
func (r *Runner) complete(ctx context.Context, req model.Request, streamedText, streamedReasoning *strings.Builder) (model.Response, error) {
	// Per-turn system prompt override: temporarily swap the system message for
	// this call, then clear so subsequent calls revert to the base prompt.
	// Plan mode, sub-agents, and extension results use this.
	if r.SystemPromptOverride != "" && len(req.Messages) > 0 && req.Messages[0].Role == model.RoleSystem {
		// Copy the slice so we don't mutate the caller's messages.
		cp := make([]model.Message, len(req.Messages))
		copy(cp, req.Messages)
		cp[0] = model.Message{Role: model.RoleSystem, Content: r.SystemPromptOverride}
		req.Messages = cp
		r.SystemPromptOverride = ""
	}

	// TransformContext hook: extensions can modify the full message list before
	// the request is sent. Applied after the override so hooks see the final state.
	// The hook receives a copy — it must not mutate the input slice or its
	// elements, but the returned slice replaces req.Messages for this call only.
	if r.TransformContext != nil {
		cp := make([]model.Message, len(req.Messages))
		copy(cp, req.Messages)
		req.Messages = r.TransformContext(cp)
	}

	// Context hooks (shell): fired before every LLM call. Chained: each
	// hook sees the previous hook's output. Runs after TransformContext so
	// programmatic transforms happen first, shell hooks refine second.
	if r.HookRunner != nil && r.HookRunner.HasContextHook() {
		req.Messages = ctxHookToModel(r.HookRunner.RunContextHooks(ctx, ctxHookFromModel(req.Messages), r.emitSessionID, r.emitTurnID))
	}

	// Every model call goes through here, so this is the one place the current
	// date and git status are injected — the main loop, the step-limit answer,
	// and subagents all get them without each call site remembering to.
	req.Messages = r.injectGitInfo(req.Messages)
	req.Messages = withCurrentDate(withReferenceProtocol(req.Messages), time.Now())
	req.SessionID = r.emitSessionID
	req.TurnID = r.emitTurnID
	req.RequestID = r.emitRequestID
	req.ExecutionID = r.emitInvocationID
	if r.Stream {
		if sp, ok := r.Model.(model.StreamingProvider); ok {
			resp, err := sp.CompleteStream(ctx, req, func(delta string) {
				if streamedText != nil {
					streamedText.WriteString(delta)
				}
				r.emit(Event{Kind: EventTokenDelta, Text: delta})
			}, func(delta string) {
				if streamedReasoning != nil {
					streamedReasoning.WriteString(delta)
				}
				r.emit(Event{Kind: EventReasoningDelta, Text: delta})
			})
			if err == nil {
				r.recordCacheSample(resp.Usage.PromptTokens, resp.Usage.CachedPromptTokens)
			}
			return resp, err
		}
	}
	resp, err := r.Model.Complete(ctx, req)
	if err == nil {
		r.recordCacheSample(resp.Usage.PromptTokens, resp.Usage.CachedPromptTokens)
	}
	return resp, err
}

// workflowPlanApproval returns a PlanApproval callback wired to the Runner's
// PlanApprover. Nil PlanApprover → auto-approve (headless/test path).
func (r *Runner) workflowPlanApproval(callID string) func(planID, title, content string) bool {
	if r.PlanApprover == nil {
		return nil // nil = auto-approve in flux_tool.go
	}
	return func(planID, title, content string) bool {
		plan := Plan{
			ID:      planID,
			Title:   title,
			Content: content,
		}
		decision := r.PlanApprover.ApprovePlan(plan)
		if decision == PlanApproved {
			r.emit(Event{
				Kind: EventPlanApproved, CallID: callID,
				Text: planID,
			})
		} else {
			r.emit(Event{
				Kind: EventPlanRejected, CallID: callID,
				Text: planID,
			})
		}
		return decision == PlanApproved
	}
}

func (r *Runner) executeTool(ctx context.Context, tool tools.Tool, callID string, input json.RawMessage) (tools.ToolResult, error) {
	// Capture turn identity before the closure so async workflow events
	// (emitted after Execute returns) carry the correct correlation IDs.
	turnID := r.emitTurnID
	sessionID := r.emitSessionID
	ec := tools.ExecutionContext{
		WorkspaceRoot:      r.WorkspaceRoot,
		SessionID:          sessionID,
		TurnID:             turnID,
		CallID:             callID,
		ExecutionID:        r.emitInvocationID,
		RequestID:          r.RequestID,
		PlanMode:           r.PlanState == PlanStatusPlanning || r.PlanState == PlanStatusProposing,
		PathAccessApprover: r.PathAccessApprover,
		Model:              r.ModelName,
		SessionIndex:       r.SessionIndex,
		SessionControl:     r.SessionControl,
		OnStdout: func(chunk string) {
			r.emit(Event{Kind: EventToolStdout, CallID: callID, Chunk: chunk})
		},
		OnStderr: func(chunk string) {
			r.emit(Event{Kind: EventToolStderr, CallID: callID, Chunk: chunk})
		},
		NestedExecutor: r,
		ToolRegistry:   r.Tools,
		OnWorkflowEvent: func(kind string, payload json.RawMessage) {
			r.emit(Event{Kind: EventKind(kind), CallID: callID, TurnID: turnID, SessionID: sessionID, Workflow: payload})
		},
		WorkflowPlanApproval: r.workflowPlanApproval(callID),
	}
	result, err := tool.Execute(ctx, ec, input)
	if err != nil {
		return tools.ToolResult{}, err
	}
	return result, nil
}

// executorFor determines which side executes a tool call. Empty string (or
// "server") means server-side execution; "client" means the client must
// execute it and deliver the result back.
func (r *Runner) executorFor(tool tools.Tool, known bool) string {
	if !known {
		return ""
	}
	if ct, ok := tool.(tools.ClientTool); ok && ct.ExecutionMode() == tools.ExecStrictClient {
		if r.ClientWaiter != nil {
			return "client"
		}
		// No client connected — fall through to server-side error handling.
	}
	return ""
}

// clientToolTimeout returns the configured client tool lease timeout, or a
// 2-minute default.
func (r *Runner) clientToolTimeout() time.Duration {
	if r.ClientToolTimeout > 0 {
		return r.ClientToolTimeout
	}
	return 2 * time.Minute
}
