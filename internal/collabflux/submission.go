// Package collabflux adapts durable task commands to the pinned Flux runtime.
// It does not choose phases, authorize repair, or start workers.
package collabflux

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"code-agent/internal/collab"
	"github.com/tuxi/flux-workflow/definition"
	"github.com/tuxi/flux-workflow/domain"
	"github.com/tuxi/flux-workflow/repository/query"
	flux "github.com/tuxi/flux-workflow/runtime"
	"gorm.io/gorm"
)

type Adapter struct{ runtime *flux.Runtime }

func New(runtime *flux.Runtime) (*Adapter, error) {
	if runtime == nil {
		return nil, collab.ErrUnavailable
	}
	err := runtime.DB().Exec(`CREATE TABLE IF NOT EXISTS codeagent_submission_receipts(source_store_id TEXT NOT NULL,command_id TEXT NOT NULL,payload_hash TEXT NOT NULL,flux_task_id INTEGER NOT NULL UNIQUE,document TEXT NOT NULL,PRIMARY KEY(source_store_id,command_id))`).Error
	if err != nil {
		return nil, err
	}
	return &Adapter{runtime: runtime}, nil
}

// CommitSubmission creates a Flux task and its deduplication receipt in one
// transaction. Enqueue is intentionally separate: the local queue is volatile.
func (a *Adapter) CommitSubmission(ctx context.Context, c collab.ManagedCommand) (collab.SubmissionReceipt, error) {
	var receipt collab.SubmissionReceipt
	var run collab.ManagedRun
	if c.Kind != "submit_workflow" || c.CommandID == "" || c.StoreID == "" || c.PayloadHash != collab.PayloadHash(c.Payload) || json.Unmarshal(c.Payload, &run) != nil || run.RunID != c.RunID || run.CommandID != c.CommandID || run.StoreID != c.StoreID || run.TaskID == "" || run.Epoch != "1" || run.State != "prepared" {
		return receipt, collab.ErrInvalid
	}
	versionID, err := strconv.ParseInt(run.Workflow.VersionID, 10, 64)
	if err != nil || versionID <= 0 {
		return receipt, collab.ErrInvalid
	}
	err = a.runtime.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Take the SQLite writer before reading, including from another Adapter.
		if err := tx.Exec(`UPDATE codeagent_submission_receipts SET payload_hash=payload_hash WHERE source_store_id=? AND command_id=?`, c.StoreID, c.CommandID).Error; err != nil {
			return err
		}
		var row struct {
			PayloadHash string
			Document    string
		}
		err := tx.Table("codeagent_submission_receipts").Where("source_store_id=? AND command_id=?", c.StoreID, c.CommandID).Take(&row).Error
		if err == nil {
			if row.PayloadHash != c.PayloadHash {
				return collab.ErrConflict
			}
			return json.Unmarshal([]byte(row.Document), &receipt)
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		version, err := query.NewWorkflowVersionRepository(tx).Get(ctx, versionID)
		if err != nil {
			return err
		}
		var def definition.WorkflowDefinition
		if json.Unmarshal(version.DefinitionJSON, &def) != nil || def.Name != run.Workflow.Name || version.Hash != run.Workflow.Hash || def.Hash() != run.Workflow.Hash {
			return collab.ErrConflict
		}
		repo := query.NewTaskRepository(tx, nil)
		task := &domain.Task{Status: domain.TaskPending, WorkflowVersionID: version.ID, WorkflowDefinitionID: version.WorkflowID, InputJSON: c.Payload}
		if err = repo.Create(ctx, task); err != nil {
			return err
		}
		task.RootID = task.ID
		if err = repo.Update(ctx, task); err != nil {
			return err
		}
		receipt = collab.SubmissionReceipt{StoreID: c.StoreID, CommandID: c.CommandID, PayloadHash: c.PayloadHash, Workflow: run.Workflow, FluxTaskID: strconv.FormatInt(task.ID, 10)}
		b, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		return tx.Exec(`INSERT INTO codeagent_submission_receipts(source_store_id,command_id,payload_hash,flux_task_id,document) VALUES(?,?,?,?,?)`, c.StoreID, c.CommandID, c.PayloadHash, task.ID, string(b)).Error
	})
	if err != nil {
		return collab.SubmissionReceipt{}, err
	}
	return receipt, nil
}

// EnqueueKnownPending only redelivers a receipt's known pending identity. It
// never changes a running/failed task to pending and never invokes Flux Retry.
// Startup reconciliation and worker wiring belong to B2-c.
func (a *Adapter) EnqueueKnownPending(ctx context.Context, receipt collab.SubmissionReceipt) error {
	var raw string
	err := a.runtime.DB().WithContext(ctx).Raw(`SELECT document FROM codeagent_submission_receipts WHERE source_store_id=? AND command_id=?`, receipt.StoreID, receipt.CommandID).Row().Scan(&raw)
	if err != nil {
		return err
	}
	var stored collab.SubmissionReceipt
	if json.Unmarshal([]byte(raw), &stored) != nil || stored != receipt {
		return collab.ErrConflict
	}
	id, err := strconv.ParseInt(receipt.FluxTaskID, 10, 64)
	if err != nil || id <= 0 {
		return collab.ErrInvalid
	}
	task, err := a.runtime.Status(ctx, id)
	if err != nil {
		return err
	}
	if task.Status != domain.TaskPending {
		return collab.ErrRecoveryRequired
	}
	return a.runtime.Engine().TaskRepo().Enqueue(ctx, id)
}
