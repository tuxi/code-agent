package conversation

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"code-agent/internal/session"
)

// rebindTable holds host-supplied fresh absolute paths for external workspaces,
// keyed by conversation id, for the current process launch only. It is volatile by
// design: on the next launch the host re-supplies via Rebind before opening streams.
type rebindTable struct {
	mu sync.RWMutex
	m  map[string]string
}

func newRebindTable() *rebindTable { return &rebindTable{m: map[string]string{}} }

func (t *rebindTable) set(id, abs string) {
	t.mu.Lock()
	t.m[id] = abs
	t.mu.Unlock()
}

func (t *rebindTable) get(id string) (string, bool) {
	t.mu.RLock()
	abs, ok := t.m[id]
	t.mu.RUnlock()
	return abs, ok
}

// sqliteRepository is the concrete ConversationRepository backed by
// session.SessionStore. It wraps a store and adds a Create method that bakes
// in session.Builder configuration (context window, compaction threshold, model
// name, per-workspace skills index).
type sqliteRepository struct {
	store            session.SessionStore
	contextWindow    int
	compactThreshold int
	modelName        string

	// currentWorkspaceDir is the workspaceDir for THIS process launch (cfg.Workspace.Root).
	// It re-anchors persisted workspace refs on load so a session's workspace survives an
	// iOS sandbox-path change. Empty on macOS, which falls back to absolute behavior.
	currentWorkspaceDir string

	// rebinds holds per-conversation host-supplied absolute paths for external
	// workspaces (this launch only). Nil/unused when currentWorkspaceDir is empty.
	rebinds *rebindTable

	// getSkillsIndex returns the L1 skill index for a given workspace root.
	// An empty return is fine — it means no skills were loaded.
	getSkillsIndex func(workspaceRoot string) string
}

// NewSQLiteRepository creates a ConversationRepository backed by the given
// SessionStore. currentWorkspaceDir is this launch's workspace root, used to
// relativize on create and re-anchor on load (see docs/ios_workspace_path_spec.md);
// pass "" on hosts with stable absolute paths (macOS) to keep absolute behavior.
// getSkillsIndex resolves the per-workspace skills prompt index (typically via
// WorkspaceRegistry); it may be nil if no skills are loaded.
func NewSQLiteRepository(store session.SessionStore, contextWindow, compactThreshold int, modelName, currentWorkspaceDir string, getSkillsIndex func(string) string) ConversationRepository {
	return &sqliteRepository{
		store:               store,
		contextWindow:       contextWindow,
		compactThreshold:    compactThreshold,
		modelName:           modelName,
		currentWorkspaceDir: currentWorkspaceDir,
		rebinds:             newRebindTable(),
		getSkillsIndex:      getSkillsIndex,
	}
}

func (r *sqliteRepository) Create(ctx context.Context, workspacePath, workspaceExtID string) (*session.Session, error) {
	return r.create(ctx, "", workspacePath, workspaceExtID)
}

func (r *sqliteRepository) CreateWithID(ctx context.Context, id, workspacePath, workspaceExtID string) (*session.Session, error) {
	return r.create(ctx, id, workspacePath, workspaceExtID)
}

func (r *sqliteRepository) create(ctx context.Context, id, workspacePath, workspaceExtID string) (*session.Session, error) {
	skillsIdx := ""
	if r.getSkillsIndex != nil {
		skillsIdx = r.getSkillsIndex(workspacePath)
	}
	sess, err := session.NewBuilder(workspacePath).
		WithID(id).
		WithBudget(r.contextWindow, r.compactThreshold).
		WithSkillsIndex(skillsIdx).
		Build()
	if err != nil {
		return nil, err
	}
	sess.Model = r.modelName
	sess.WorkspacePath = workspacePath
	// Persist the portable identity only when this launch supplies a workspaceDir to
	// anchor against (iOS). On desktop (empty) the absolute path is stable and stored
	// verbatim — no relativization, no normalization — preserving prior behavior.
	if r.currentWorkspaceDir != "" {
		sess.Workspace = session.ToWorkspaceRef(workspacePath, r.currentWorkspaceDir, workspaceExtID)
		// The host just supplied this launch's absolute path; record it so an external
		// session is immediately resolvable without a separate rebind round-trip.
		if sess.Workspace.Root == session.RootExternal && workspacePath != "" {
			r.rebinds.set(sess.ID, workspacePath)
		}
	}
	if err := r.store.Save(ctx, sess); err != nil {
		return nil, err
	}
	return sess, nil
}

func (r *sqliteRepository) Load(ctx context.Context, id string) (*session.Session, error) {
	sess, err := r.store.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.currentWorkspaceDir == "" {
		return sess, nil // desktop: absolute workspace path is stable, return as stored
	}
	// Lazy migrate legacy rows (absolute path, no ref) into a portable ref, then
	// re-anchor the absolute path for this launch. Best-effort persist so the next
	// load sees a non-empty Root and skips this — idempotent (see spec §9).
	if sess.Workspace.Root == "" && sess.WorkspacePath != "" {
		sess.Workspace = session.MigrateLegacyWorkspacePath(sess.WorkspacePath, r.currentWorkspaceDir)
		_ = r.store.Save(ctx, sess)
	}
	if abs, ok := r.reanchor(id, sess.Workspace); ok {
		sess.WorkspacePath = abs
	}
	return sess, nil
}

// reanchor resolves a portable ref into this launch's absolute path, passing any
// host-supplied rebind for an external session. It reports ok=false (leaving the
// caller's stored WorkspacePath untouched) for an unanchored ref or an external ref
// that needs host rebind — the session is still listed and loadable; only its
// workspace binding is stale until the host re-supplies it.
func (r *sqliteRepository) reanchor(id string, ref session.WorkspaceRef) (string, bool) {
	if ref.Root == "" {
		return "", false
	}
	hostAbs := ""
	if ref.Root == session.RootExternal {
		hostAbs, _ = r.rebinds.get(id)
	}
	abs, err := ref.Resolve(r.currentWorkspaceDir, hostAbs)
	if err != nil || abs == "" {
		return "", false
	}
	return abs, true
}

func (r *sqliteRepository) Rebind(ctx context.Context, id, absPath string) error {
	if absPath == "" {
		return fmt.Errorf("rebind: empty workspace_path")
	}
	if _, err := os.Stat(absPath); err != nil {
		return fmt.Errorf("rebind: workspace path not accessible: %w", err)
	}
	// Confirm the session exists so a typo'd id surfaces as an error, not a silent no-op.
	if _, err := r.store.Load(ctx, id); err != nil {
		return err
	}
	r.rebinds.set(id, absPath)
	return nil
}

func (r *sqliteRepository) NeedsRebind(ctx context.Context, id string) (bool, error) {
	if r.currentWorkspaceDir == "" {
		return false, nil // desktop: absolute paths are stable
	}
	sess, err := r.store.Load(ctx, id)
	if err != nil {
		return false, err
	}
	ref := sess.Workspace
	if ref.Root == "" && sess.WorkspacePath != "" {
		ref = session.MigrateLegacyWorkspacePath(sess.WorkspacePath, r.currentWorkspaceDir)
	}
	if ref.Root != session.RootExternal {
		return false, nil // workspace-rooted (or empty): runtime self-anchors
	}
	// External: resolvable iff reanchor succeeds — i.e. the host already rebound it
	// this launch, or its ext_id is itself a stable absolute path (desktop-style).
	_, ok := r.reanchor(id, ref)
	return !ok, nil
}

func (r *sqliteRepository) Save(ctx context.Context, s *session.Session) error {
	return r.store.Save(ctx, s)
}

func (r *sqliteRepository) List(ctx context.Context) ([]session.Meta, error) {
	metas, err := r.store.List(ctx)
	if err != nil {
		return nil, err
	}
	if r.currentWorkspaceDir == "" {
		return metas, nil // desktop: absolute paths stable, return as stored
	}
	// Re-anchor each listed workspace path so the conversation list shows paths valid
	// for THIS launch, not the frozen ones written at create time.
	for i := range metas {
		if abs, ok := r.reanchor(metas[i].ID, metas[i].Workspace); ok {
			metas[i].WorkspacePath = abs
		}
	}
	return metas, nil
}

func (r *sqliteRepository) SupportsConversationArchive() bool {
	_, ok := r.store.(session.ConversationArchiveStore)
	return ok
}

func (r *sqliteRepository) Archive(ctx context.Context, id string, at time.Time) (time.Time, error) {
	store, ok := r.store.(session.ConversationArchiveStore)
	if !ok {
		return time.Time{}, ErrConversationArchiveUnsupported
	}
	return store.Archive(ctx, id, at)
}

func (r *sqliteRepository) Restore(ctx context.Context, id string) error {
	store, ok := r.store.(session.ConversationArchiveStore)
	if !ok {
		return ErrConversationArchiveUnsupported
	}
	return store.Restore(ctx, id)
}

func (r *sqliteRepository) ListArchived(ctx context.Context, archived bool) ([]session.Meta, error) {
	if !r.SupportsConversationArchive() {
		return nil, ErrConversationArchiveUnsupported
	}
	metas, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]session.Meta, 0, len(metas))
	for _, meta := range metas {
		if (!meta.ArchivedAt.IsZero()) == archived {
			out = append(out, meta)
		}
	}
	return out, nil
}

func (r *sqliteRepository) Delete(ctx context.Context, id string) error {
	return r.store.Delete(ctx, id)
}

func (r *sqliteRepository) UpdateName(ctx context.Context, id string, name string) error {
	return r.store.UpdateName(ctx, id, name)
}

func (r *sqliteRepository) Close() error {
	return r.store.Close()
}

func (r *sqliteRepository) turnInputStore() (session.TurnInputStore, error) {
	store, ok := r.store.(session.TurnInputStore)
	if !ok {
		return nil, fmt.Errorf("conversation repository does not support durable turn inputs")
	}
	return store, nil
}

func (r *sqliteRepository) ReserveTurnInput(ctx context.Context, input session.TurnInput, accepted session.EventRecord) (session.TurnInput, bool, int64, error) {
	store, err := r.turnInputStore()
	if err != nil {
		return session.TurnInput{}, false, 0, err
	}
	return store.ReserveTurnInput(ctx, input, accepted)
}

func (r *sqliteRepository) StartTurnInput(ctx context.Context, input session.TurnInput, sess *session.Session) error {
	store, err := r.turnInputStore()
	if err != nil {
		return err
	}
	return store.StartTurnInput(ctx, input, sess)
}

func (r *sqliteRepository) SetTurnInputState(ctx context.Context, sessionID, requestID string, state session.TurnInputState) error {
	store, err := r.turnInputStore()
	if err != nil {
		return err
	}
	return store.SetTurnInputState(ctx, sessionID, requestID, state)
}

func (r *sqliteRepository) TurnInput(ctx context.Context, sessionID, requestID string) (session.TurnInput, error) {
	store, err := r.turnInputStore()
	if err != nil {
		return session.TurnInput{}, err
	}
	return store.TurnInput(ctx, sessionID, requestID)
}

func (r *sqliteRepository) RecoverableTurnInputs(ctx context.Context) ([]session.TurnInput, error) {
	store, err := r.turnInputStore()
	if err != nil {
		return nil, err
	}
	return store.RecoverableTurnInputs(ctx)
}

func (r *sqliteRepository) assetRefReleaseStore() (session.AssetRefReleaseStore, error) {
	store, ok := r.store.(session.AssetRefReleaseStore)
	if !ok {
		return nil, fmt.Errorf("conversation repository does not support asset-ref release outbox")
	}
	return store, nil
}

func (r *sqliteRepository) SupportsUserAssetsPersistence() bool {
	_, inbox := r.store.(session.TurnInputStore)
	_, outbox := r.store.(session.AssetRefReleaseStore)
	return inbox && outbox
}

func (r *sqliteRepository) EnqueueAssetRefRelease(ctx context.Context, release session.AssetRefRelease) error {
	store, err := r.assetRefReleaseStore()
	if err != nil {
		return err
	}
	return store.EnqueueAssetRefRelease(ctx, release)
}
func (r *sqliteRepository) DeleteWithAssetRefRelease(ctx context.Context, sessionID string, release session.AssetRefRelease) error {
	store, err := r.assetRefReleaseStore()
	if err != nil {
		return err
	}
	return store.DeleteWithAssetRefRelease(ctx, sessionID, release)
}
func (r *sqliteRepository) PendingAssetRefReleases(ctx context.Context, scope string, now time.Time) ([]session.AssetRefRelease, error) {
	store, err := r.assetRefReleaseStore()
	if err != nil {
		return nil, err
	}
	return store.PendingAssetRefReleases(ctx, scope, now)
}
func (r *sqliteRepository) RetryAssetRefRelease(ctx context.Context, sessionID string, attempts int, next time.Time) error {
	store, err := r.assetRefReleaseStore()
	if err != nil {
		return err
	}
	return store.RetryAssetRefRelease(ctx, sessionID, attempts, next)
}
func (r *sqliteRepository) CompleteAssetRefRelease(ctx context.Context, sessionID string) error {
	store, err := r.assetRefReleaseStore()
	if err != nil {
		return err
	}
	return store.CompleteAssetRefRelease(ctx, sessionID)
}

// Compile-time check: sqliteRepository satisfies ConversationRepository.
var _ ConversationRepository = (*sqliteRepository)(nil)
var _ ReservedConversationRepository = (*sqliteRepository)(nil)
var _ ArchivableConversationRepository = (*sqliteRepository)(nil)
var _ ConversationArchiveCapability = (*sqliteRepository)(nil)
var _ TurnInputRepository = (*sqliteRepository)(nil)
var _ AssetRefReleaseRepository = (*sqliteRepository)(nil)
var _ UserAssetsPersistenceCapability = (*sqliteRepository)(nil)

// StoreEventAdapter wraps a session.EventStore as a ConversationEventStore by
// delegating Append → RecordEvent and Replay → SessionEvents. This is the
// adapter used at startup; later PRs can swap the backing store for
// Redis/Kafka without changing any consumer.
type StoreEventAdapter struct {
	Store session.EventStore
}

func (a *StoreEventAdapter) Append(ctx context.Context, e session.EventRecord) (int64, error) {
	return a.Store.RecordEvent(ctx, e)
}

func (a *StoreEventAdapter) Replay(ctx context.Context, sessionID string) ([]session.EventRecord, error) {
	return a.Store.SessionEvents(ctx, sessionID)
}

func (a *StoreEventAdapter) ReplaySince(ctx context.Context, sessionID string, sinceSeq int64) ([]session.EventRecord, error) {
	return a.Store.SessionEventsSince(ctx, sessionID, sinceSeq)
}

func (a *StoreEventAdapter) Attention(ctx context.Context, sinceSequence int64) (session.EventAttentionSnapshot, error) {
	store, ok := a.Store.(session.EventAttentionStore)
	if !ok {
		return session.EventAttentionSnapshot{}, fmt.Errorf("event store does not support session attention snapshots")
	}
	return store.SessionEventAttention(ctx, sinceSequence)
}

func (a *StoreEventAdapter) SupportsAttentionSnapshot() bool {
	_, ok := a.Store.(session.EventAttentionStore)
	return ok
}

// Compile-time check.
var _ ConversationEventStore = (*StoreEventAdapter)(nil)
var _ ConversationAttentionStore = (*StoreEventAdapter)(nil)
var _ ConversationAttentionCapability = (*StoreEventAdapter)(nil)
