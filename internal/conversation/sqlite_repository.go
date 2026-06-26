package conversation

import (
	"context"

	"code-agent/internal/session"
)

// sqliteRepository is the concrete ConversationRepository backed by session.Store
// (SQLite). It wraps the existing SQLiteStore and adds a Create method that bakes
// in session.Builder configuration (context window, compaction threshold, model
// name, per-workspace skills index).
type sqliteRepository struct {
	store            session.Store
	contextWindow    int
	compactThreshold int
	modelName        string

	// getSkillsIndex returns the L1 skill index for a given workspace root.
	// An empty return is fine — it means no skills were loaded.
	getSkillsIndex func(workspaceRoot string) string
}

// NewSQLiteRepository creates a ConversationRepository backed by the given
// session.Store. getSkillsIndex resolves the per-workspace skills prompt index
// (typically via WorkspaceRegistry); it may be nil if no skills are loaded.
func NewSQLiteRepository(store session.Store, contextWindow, compactThreshold int, modelName string, getSkillsIndex func(string) string) ConversationRepository {
	return &sqliteRepository{
		store:            store,
		contextWindow:    contextWindow,
		compactThreshold: compactThreshold,
		modelName:        modelName,
		getSkillsIndex:   getSkillsIndex,
	}
}

func (r *sqliteRepository) Create(ctx context.Context, workspacePath string) (*session.Session, error) {
	skillsIdx := ""
	if r.getSkillsIndex != nil {
		skillsIdx = r.getSkillsIndex(workspacePath)
	}
	sess, err := session.NewBuilder(workspacePath).
		WithBudget(r.contextWindow, r.compactThreshold).
		WithSkillsIndex(skillsIdx).
		Build()
	if err != nil {
		return nil, err
	}
	sess.Model = r.modelName
	sess.WorkspacePath = workspacePath
	if err := r.store.Save(ctx, sess); err != nil {
		return nil, err
	}
	return sess, nil
}

func (r *sqliteRepository) Load(ctx context.Context, id string) (*session.Session, error) {
	return r.store.Load(ctx, id)
}

func (r *sqliteRepository) Save(ctx context.Context, s *session.Session) error {
	return r.store.Save(ctx, s)
}

func (r *sqliteRepository) List(ctx context.Context) ([]session.Meta, error) {
	return r.store.List(ctx)
}

func (r *sqliteRepository) Delete(ctx context.Context, id string) error {
	return r.store.Delete(ctx, id)
}

func (r *sqliteRepository) Close() error {
	return r.store.Close()
}

// Compile-time check: sqliteRepository satisfies ConversationRepository.
var _ ConversationRepository = (*sqliteRepository)(nil)

// StoreEventAdapter wraps a session.Store as a ConversationEventStore by
// delegating Append → RecordEvent and Replay → SessionEvents. This is the
// SQLite-backed implementation used at startup; later PRs can swap it for
// Redis/Kafka without changing any consumer.
type StoreEventAdapter struct {
	Store session.Store
}

func (a *StoreEventAdapter) Append(ctx context.Context, e session.EventRecord) error {
	return a.Store.RecordEvent(ctx, e)
}

func (a *StoreEventAdapter) Replay(ctx context.Context, sessionID string) ([]session.EventRecord, error) {
	return a.Store.SessionEvents(ctx, sessionID)
}

// Compile-time check.
var _ ConversationEventStore = (*StoreEventAdapter)(nil)
