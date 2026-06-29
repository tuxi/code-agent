// Package fluxstore provides SQLite-backed implementations of flux Store interfaces.
// This is the consumer-side adapter pattern: flux defines interfaces, code-agent provides SQLite.
package fluxstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"

	"flux/runtime"
	"flux/store"
)

// Store is a SQLite-backed implementation of both flux's WorkflowStore and AwaitStore.
// Uses the same SQLite database file as code-agent's session store.
type Store struct {
	db *sql.DB
	mu sync.RWMutex
}

// Ensure Store implements both interfaces.
var _ store.WorkflowStore = (*Store)(nil)
var _ store.AwaitStore = (*Store)(nil)

// New creates a Store backed by the given SQLite database.
// The db should already be opened and migrated (via code-agent's session/sqlite).
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// ── WorkflowStore ──

func (s *Store) CreateRun(ctx context.Context, meta store.RunMeta) (*store.WorkflowRun, error) {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO flux_runs (id, conversation_id, goal, status) VALUES (?, ?, ?, 'running')`,
		meta.ID, meta.ConversationID, meta.Goal,
	)
	if err != nil {
		return nil, fmt.Errorf("create run: %w", err)
	}
	return &store.WorkflowRun{ID: meta.ID, ConversationID: meta.ConversationID, Goal: meta.Goal, Status: "running"}, nil
}

func (s *Store) LoadRun(ctx context.Context, runID string) (*store.WorkflowRun, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, conversation_id, goal, status FROM flux_runs WHERE id = ?`, runID)
	r := &store.WorkflowRun{}
	if err := row.Scan(&r.ID, &r.ConversationID, &r.Goal, &r.Status); err != nil {
		return nil, fmt.Errorf("load run: %w", err)
	}
	return r, nil
}

func (s *Store) UpdateRunStatus(ctx context.Context, runID string, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE flux_runs SET status = ? WHERE id = ?`, status, runID)
	return err
}

func (s *Store) CreateTask(ctx context.Context, runID string, meta store.TaskMeta) (*store.Task, error) {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO flux_tasks (id, run_id, parent_id, root_id, status) VALUES (?, ?, ?, ?, 'running')`,
		meta.ID, runID, meta.ParentID, meta.RootID,
	)
	if err != nil {
		return nil, fmt.Errorf("create task: %w", err)
	}
	return &store.Task{ID: meta.ID, RunID: runID, ParentID: meta.ParentID, RootID: meta.RootID, Status: "running"}, nil
}

func (s *Store) LoadTask(ctx context.Context, taskID string) (*store.Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, run_id, parent_id, root_id, status FROM flux_tasks WHERE id = ?`, taskID)
	t := &store.Task{}
	if err := row.Scan(&t.ID, &t.RunID, &t.ParentID, &t.RootID, &t.Status); err != nil {
		return nil, fmt.Errorf("load task: %w", err)
	}
	return t, nil
}

func (s *Store) ListTasks(ctx context.Context, runID string) ([]store.Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, parent_id, root_id, status FROM flux_tasks WHERE run_id = ?`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Task
	for rows.Next() {
		var t store.Task
		if err := rows.Scan(&t.ID, &t.RunID, &t.ParentID, &t.RootID, &t.Status); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func (s *Store) PersistNode(ctx context.Context, taskID, nodeName string, state runtime.NodeState, output map[string]any) error {
	outJSON, _ := json.Marshal(output)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO flux_nodes (task_id, node_name, state, output) VALUES (?, ?, ?, ?)
		 ON CONFLICT(task_id, node_name) DO UPDATE SET state=excluded.state, output=excluded.output`,
		taskID, nodeName, int(state), string(outJSON),
	)
	return err
}

func (s *Store) LoadNodeStates(ctx context.Context, taskID string) ([]store.NodeRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT node_name, state, output FROM flux_nodes WHERE task_id = ?`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.NodeRecord
	for rows.Next() {
		var nr store.NodeRecord
		var state int
		var outJSON string
		if err := rows.Scan(&nr.NodeName, &state, &outJSON); err != nil {
			return nil, err
		}
		nr.State = runtime.NodeState(state)
		if outJSON != "" {
			_ = json.Unmarshal([]byte(outJSON), &nr.Output)
		}
		out = append(out, nr)
	}
	return out, nil
}

func (s *Store) SavePlan(ctx context.Context, taskID string, plan *runtime.Plan) error {
	if plan == nil {
		_, err := s.db.ExecContext(ctx, `DELETE FROM flux_plans WHERE task_id = ?`, taskID)
		return err
	}
	planJSON, _ := json.Marshal(plan)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO flux_plans (task_id, plan_json) VALUES (?, ?)
		 ON CONFLICT(task_id) DO UPDATE SET plan_json=excluded.plan_json`,
		taskID, string(planJSON),
	)
	return err
}

func (s *Store) LoadPlan(ctx context.Context, taskID string) (*runtime.Plan, error) {
	row := s.db.QueryRowContext(ctx, `SELECT plan_json FROM flux_plans WHERE task_id = ?`, taskID)
	var planJSON string
	if err := row.Scan(&planJSON); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	var plan runtime.Plan
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

// ── AwaitStore ──

func (s *Store) CreateBinding(ctx context.Context, binding store.AwaitBinding) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO flux_awaits (binding_id, task_id, node_name, provider_task_id, status, input_json)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		binding.BindingID, binding.TaskID, binding.NodeName, binding.ProviderTaskID,
		binding.Status, mustJSON(binding.Input),
	)
	return err
}

func (s *Store) ResolveBinding(ctx context.Context, bindingID string) (bool, error) {
	result, err := s.db.ExecContext(ctx,
		`UPDATE flux_awaits SET status = 'completed' WHERE binding_id = ? AND status = 'awaiting'`,
		bindingID,
	)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

func (s *Store) FindByProviderTaskID(ctx context.Context, providerTaskID string) (*store.AwaitBinding, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT binding_id, task_id, node_name, provider_task_id, status, input_json
		 FROM flux_awaits WHERE provider_task_id = ?`, providerTaskID)
	b := &store.AwaitBinding{}
	var inputJSON string
	if err := row.Scan(&b.BindingID, &b.TaskID, &b.NodeName, &b.ProviderTaskID, &b.Status, &inputJSON); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	_ = json.Unmarshal([]byte(inputJSON), &b.Input)
	return b, nil
}

func (s *Store) ListPending(ctx context.Context) ([]store.AwaitBinding, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT binding_id, task_id, node_name, provider_task_id, status, input_json
		 FROM flux_awaits WHERE status = 'awaiting'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.AwaitBinding
	for rows.Next() {
		var b store.AwaitBinding
		var inputJSON string
		if err := rows.Scan(&b.BindingID, &b.TaskID, &b.NodeName, &b.ProviderTaskID, &b.Status, &inputJSON); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(inputJSON), &b.Input)
		out = append(out, b)
	}
	return out, nil
}

func (s *Store) ListByTask(ctx context.Context, taskID string) ([]store.AwaitBinding, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT binding_id, task_id, node_name, provider_task_id, status, input_json
		 FROM flux_awaits WHERE task_id = ?`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.AwaitBinding
	for rows.Next() {
		var b store.AwaitBinding
		var inputJSON string
		if err := rows.Scan(&b.BindingID, &b.TaskID, &b.NodeName, &b.ProviderTaskID, &b.Status, &inputJSON); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(inputJSON), &b.Input)
		out = append(out, b)
	}
	return out, nil
}

// ── Schema ──

// Migrate creates the flux tables in the given database.
func Migrate(db *sql.DB) error {
	schema := []string{
		`CREATE TABLE IF NOT EXISTS flux_runs (
			id TEXT PRIMARY KEY,
			conversation_id TEXT,
			goal TEXT,
			status TEXT DEFAULT 'running'
		)`,
		`CREATE TABLE IF NOT EXISTS flux_tasks (
			id TEXT PRIMARY KEY,
			run_id TEXT,
			parent_id TEXT,
			root_id TEXT,
			status TEXT DEFAULT 'running'
		)`,
		`CREATE TABLE IF NOT EXISTS flux_nodes (
			task_id TEXT NOT NULL,
			node_name TEXT NOT NULL,
			state INTEGER DEFAULT 0,
			output TEXT DEFAULT '{}',
			PRIMARY KEY (task_id, node_name)
		)`,
		`CREATE TABLE IF NOT EXISTS flux_awaits (
			binding_id TEXT PRIMARY KEY,
			task_id TEXT,
			node_name TEXT,
			provider_task_id TEXT,
			status TEXT DEFAULT 'awaiting',
			input_json TEXT DEFAULT '{}'
		)`,
		`CREATE TABLE IF NOT EXISTS flux_plans (
			task_id TEXT PRIMARY KEY,
			plan_json TEXT
		)`,
	}
	for _, ddl := range schema {
		if _, err := db.Exec(ddl); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
