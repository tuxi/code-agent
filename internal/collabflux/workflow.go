package collabflux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"code-agent/internal/collab"
	"github.com/tuxi/flux-workflow/definition"
	"github.com/tuxi/flux-workflow/domain"
	"github.com/tuxi/flux-workflow/repository/query"
	flux "github.com/tuxi/flux-workflow/runtime"
	"github.com/tuxi/flux-workflow/tool"
)

const CollaborationWorkflow = "coding_collaboration_v1"
const stageTool = "coding_task_stage_v1"
const TurnProvider = "coding_task_turn_v1"
const ReviewProvider = "coding_task_review_v1"

// CollaborationDefinition expands a bounded DAG. It has no loop edge or Retry
// operation. Historical v1/v2 workflow definitions are never replaced.
func CollaborationDefinition(rounds int) (*definition.WorkflowDefinition, error) {
	if rounds < 1 || rounds > 5 {
		return nil, collab.ErrInvalid
	}
	d := &definition.WorkflowDefinition{Name: CollaborationWorkflow, Nodes: []definition.NodeDefinition{{Name: "start", Type: definition.NodeStart}, {Name: "end", Type: definition.NodeEnd}}}
	edge := func(from, to, condition string) {
		kind := definition.EdgeNormal
		if condition != "" {
			kind = definition.EdgeCondition
		}
		d.Edges = append(d.Edges, definition.EdgeDefinition{From: from, To: to, Type: kind, Condition: condition})
	}
	for n := 1; n <= rounds; n++ {
		suffix := "_" + strconv.Itoa(n)
		for _, stage := range []string{"dispatch", "capture", "gate", "finalize", "attention"} {
			d.Nodes = append(d.Nodes, definition.NodeDefinition{Name: stage + suffix, Type: definition.NodeTool, Config: map[string]any{"tool": stageTool}, InputMapping: map[string]string{"run_id": "input.run_id", "epoch": "input.control_epoch", "iteration": strconv.Itoa(n), "stage": "'" + stage + "'"}})
		}
		for _, kind := range []string{"turn", "review"} {
			provider := TurnProvider
			correlation := map[string]any{"run_id": "run_id", "epoch": "epoch", "iteration": "iteration", "dispatch_id": "dispatch_id"}
			mapping := map[string]string{"provider": "'" + provider + "'", "run_id": "input.run_id", "epoch": "input.control_epoch", "iteration": strconv.Itoa(n), "dispatch_id": "dispatch" + suffix + ".dispatch_id"}
			if kind == "review" {
				provider = ReviewProvider
				mapping["provider"] = "'" + provider + "'"
				mapping["artifact_id"] = "capture" + suffix + ".artifact_id"
				correlation["artifact_id"] = "artifact_id"
			}
			d.Nodes = append(d.Nodes, definition.NodeDefinition{Name: "wait_" + kind + suffix, Type: definition.NodeAwait, Config: map[string]any{"await_type": "external", "source": "webhook_or_poll", "provider": "provider", "correlation": correlation}, InputMapping: mapping})
		}
		if n == 1 {
			edge("start", "dispatch"+suffix, "")
		}
		edge("dispatch"+suffix, "wait_turn"+suffix, "dispatch"+suffix+".status == 'admitted'")
		edge("dispatch"+suffix, "attention"+suffix, "dispatch"+suffix+".status != 'admitted'")
		edge("wait_turn"+suffix, "capture"+suffix, "wait_turn"+suffix+".status == 'completed'")
		edge("wait_turn"+suffix, "attention"+suffix, "wait_turn"+suffix+".status != 'completed'")
		edge("capture"+suffix, "wait_review"+suffix, "capture"+suffix+".status == 'captured'")
		edge("capture"+suffix, "attention"+suffix, "capture"+suffix+".status != 'captured'")
		edge("wait_review"+suffix, "gate"+suffix, "")
		edge("gate"+suffix, "finalize"+suffix, "gate"+suffix+".action == 'pass'")
		if n < rounds {
			edge("gate"+suffix, fmt.Sprintf("dispatch_%d", n+1), "gate"+suffix+".action == 'repair'")
		}
		edge("gate"+suffix, "attention"+suffix, "gate"+suffix+".action != 'pass' && gate"+suffix+".action != 'repair'")
		edge("attention"+suffix, "end", "")
		edge("finalize"+suffix, "end", "")
	}
	return d, nil
}

// Stages is trusted host assembly, registered only in the dedicated managed
// execution domain before workers start. It starts neither workers nor models.
// Dispatch must use TurnExecutor.AcceptManagedDispatch, preserving approvals.
type Stages struct {
	Runtime               *flux.Runtime
	Tasks                 collab.Repository
	Managed               collab.ManagedRepository
	Reviews               collab.ReviewRepository
	Actor                 collab.Principal
	Dispatch              func(context.Context, collab.ManagedTurnRef) error
	Checkpoint            func(string, string)
	ValidateExecutorModel func(context.Context, string) error
}

func (s *Stages) Register() error {
	if s.Runtime == nil || s.Tasks == nil || s.Managed == nil || s.Reviews == nil || !s.Actor.LocalSessions || s.Dispatch == nil {
		return collab.ErrUnavailable
	}
	s.Runtime.ToolRegistry().Register(s)
	return nil
}
func (*Stages) Name() string        { return stageTool }
func (*Stages) Description() string { return "Host-only coding task stage; not an Agent tool" }
func (*Stages) InputSchema() tool.DataSchema {
	return tool.DataSchema{Fields: map[string]tool.FieldSchema{"run_id": {Type: "string", Required: true}, "epoch": {Type: "string", Required: true}, "iteration": {Type: "number", Required: true}, "stage": {Type: "string", Required: true}}}
}
func (*Stages) OutputSchema() tool.DataSchema { return tool.DataSchema{} }
func (*Stages) Mode() tool.ExecutionMode      { return tool.SyncExecution }

func iteration(v any) (int, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return 0, collab.ErrInvalid
	}
	f, err := strconv.ParseFloat(string(b), 64)
	if err != nil || f < 1 || f > 5 || float64(int(f)) != f {
		return 0, collab.ErrInvalid
	}
	return int(f), nil
}

func (s *Stages) boundRun(ctx context.Context, id, epoch string, taskID, versionID int64) (collab.ManagedRun, error) {
	run, err := s.Managed.Run(ctx, s.Actor, id)
	if err != nil {
		return run, err
	}
	if !s.Actor.LocalSessions || run.ReviewPolicy == nil || run.Epoch != epoch || run.FluxTaskID != strconv.FormatInt(taskID, 10) || run.Workflow.VersionID != strconv.FormatInt(versionID, 10) || run.Workflow.Name != CollaborationWorkflow {
		return run, collab.ErrConflict
	}
	def, err := CollaborationDefinition(run.MaxIterations)
	if err != nil || def.Hash() != run.Workflow.Hash {
		return run, collab.ErrConflict
	}
	version, err := query.NewWorkflowVersionRepository(s.Runtime.DB()).Get(ctx, versionID)
	if err != nil {
		return run, err
	}
	if version.Hash != run.Workflow.Hash {
		return run, collab.ErrConflict
	}
	var stored definition.WorkflowDefinition
	if json.Unmarshal(version.DefinitionJSON, &stored) != nil || stored.Hash() != run.Workflow.Hash {
		return run, collab.ErrConflict
	}
	return run, nil
}

func (s *Stages) Execute(ctx context.Context, in map[string]any, _ tool.ToolEmitter) (*tool.Result, error) {
	meta, ok := tool.ExecutionMetaFromContext(ctx)
	if !ok {
		return nil, collab.ErrForbidden
	}
	id, _ := in["run_id"].(string)
	epoch, _ := in["epoch"].(string)
	stage, _ := in["stage"].(string)
	n, err := iteration(in["iteration"])
	if err != nil {
		return nil, err
	}
	if meta.NodeName != fmt.Sprintf("%s_%d", stage, n) {
		return nil, collab.ErrForbidden
	}
	task, err := s.Runtime.Status(ctx, meta.TaskID)
	if err != nil {
		return nil, err
	}
	run, err := s.boundRun(ctx, id, epoch, meta.TaskID, task.WorkflowVersionID)
	if err != nil {
		return nil, err
	}
	if n > run.MaxIterations {
		return nil, collab.ErrConflict
	}
	// Pre-admission failures may fence the next iteration before a dispatch is
	// reserved. Acknowledge that existing fence without inventing an Attempt.
	if stage == "attention" && n == run.CurrentIteration+1 && (run.State == "waiting" || run.State == "recovery_required") {
		return tool.Success(map[string]any{"status": "attention"}), nil
	}
	if stage != "dispatch" && n != run.CurrentIteration {
		return nil, collab.ErrConflict
	}
	attention := func(reason string) (*tool.Result, error) {
		_, err := s.Reviews.WaitForHuman(ctx, s.Actor, id, reason)
		if err != nil {
			return nil, err
		}
		return tool.Success(map[string]any{"status": "attention", "action": "attention"}), nil
	}
	switch stage {
	case "dispatch":
		if run.ExecutorModel != "" && (s.ValidateExecutorModel == nil || s.ValidateExecutorModel(ctx, run.ExecutorModel) != nil) {
			return attention("executor_model_unavailable")
		}
		gateID := ""
		message := ""
		if n == 1 {
			snapshot, err := s.Tasks.Snapshot(ctx, s.Actor, run.TaskID)
			if err != nil {
				return nil, err
			}
			for _, artifact := range snapshot.ArtifactRefs {
				if artifact.ArtifactID == run.PlanArtifactID && artifact.ContentHash != nil && *artifact.ContentHash == run.PlanHash {
					message = "Implement the approved plan. Keep the Session's permissions and Runtime configuration.\n" + string(artifact.Content)
				}
			}
			if message == "" {
				return nil, collab.ErrConflict
			}
		} else {
			d, err := s.Reviews.Decision(ctx, s.Actor, id, n-1)
			if err != nil {
				return nil, err
			}
			if d.Action != "repair" {
				return nil, collab.ErrConflict
			}
			gateID = d.DecisionID
			message = "Repair the reviewed candidate according to these findings:\n" + strings.Join(d.Review.Findings, "\n")
		}
		message += "\nRequired foreground validation after the final edit:\n" + strings.Join(run.ReviewPolicy.ValidationCommands, "\n")
		_, err = s.Managed.ReserveDispatch(ctx, s.Actor, id, collab.ReserveDispatchInput{RequestID: fmt.Sprintf("dag-dispatch-%d", n), Epoch: epoch, Iteration: n, Message: message, Model: run.ExecutorModel, GateDecisionID: gateID})
		if err != nil {
			return attention("dispatch_requires_reconciliation")
		}
		if s.Checkpoint != nil {
			s.Checkpoint("dispatch_reserved", id)
		}
		d, err := s.Reviews.DispatchAt(ctx, s.Actor, id, n)
		if err != nil {
			return nil, err
		}
		if err = s.Dispatch(ctx, d.Ref); err != nil {
			return attention("admission_requires_reconciliation")
		}
		if s.Checkpoint != nil {
			s.Checkpoint("turn_admitted", id)
		}
		return tool.Success(map[string]any{"status": "admitted", "dispatch_id": d.Ref.DispatchID}), nil
	case "capture":
		d, err := s.Reviews.DispatchAt(ctx, s.Actor, id, n)
		if err != nil {
			return nil, err
		}
		artifact, err := s.Reviews.CaptureBundle(ctx, s.Actor, d.Ref)
		if err != nil {
			return attention("capture_requires_attention")
		}
		if s.Checkpoint != nil {
			s.Checkpoint("bundle_captured", id)
		}
		return tool.Success(map[string]any{"status": "captured", "artifact_id": artifact.ArtifactID}), nil
	case "gate":
		d, err := s.Reviews.Decision(ctx, s.Actor, id, n)
		if err != nil {
			return nil, err
		}
		if _, err = s.Reviews.ApplyGate(ctx, s.Actor, id, d.DecisionID); err != nil {
			return nil, err
		}
		if s.Checkpoint != nil {
			s.Checkpoint("gate_consumed", id)
		}
		d, err = s.Reviews.Decision(ctx, s.Actor, id, n)
		if err != nil {
			return nil, err
		}
		return tool.Success(map[string]any{"action": d.Action}), nil
	case "finalize":
		d, err := s.Reviews.Decision(ctx, s.Actor, id, n)
		if err != nil {
			return nil, err
		}
		receipt, err := s.Reviews.FinalizeRun(ctx, s.Actor, id, d.DecisionID)
		if err != nil {
			return nil, err
		}
		return tool.Success(map[string]any{"status": receipt.Status}), nil
	case "attention":
		if run.State == "waiting" || run.State == "recovery_required" {
			return tool.Success(map[string]any{"status": "attention"}), nil
		}
		return attention("workflow_requires_attention")
	default:
		return nil, collab.ErrInvalid
	}
}

// ResolveAwait is type-dispatched. It never interprets a reported review as a
// Session event and never trusts a CompleteAwait verdict as authorization.
func (s *Stages) ResolveAwait(ctx context.Context, b *domain.AwaitBinding) (map[string]any, error) {
	if b == nil || b.Provider == nil || b.AwaitType != domain.AwaitTypeExternal {
		return nil, collab.ErrInvalid
	}
	if *b.Provider != TurnProvider && *b.Provider != ReviewProvider {
		return nil, collab.ErrUnavailable
	}
	id, _ := b.Correlation["run_id"].(string)
	epoch, _ := b.Correlation["epoch"].(string)
	n, err := iteration(b.Correlation["iteration"])
	if err != nil {
		return nil, err
	}
	kind := "turn"
	if *b.Provider == ReviewProvider {
		kind = "review"
	}
	if b.NodeName != fmt.Sprintf("wait_%s_%d", kind, n) {
		return nil, collab.ErrConflict
	}
	run, err := s.boundRun(ctx, id, epoch, b.TaskID, b.WorkflowVersionID)
	if err != nil {
		return nil, err
	}
	if n != run.CurrentIteration {
		return nil, collab.ErrConflict
	}
	d, err := s.Reviews.DispatchAt(ctx, s.Actor, id, n)
	if err != nil {
		return nil, err
	}
	dispatch, _ := b.Correlation["dispatch_id"].(string)
	if d.Ref.DispatchID != dispatch {
		return nil, collab.ErrConflict
	}
	if kind == "turn" {
		o, err := s.Reviews.ObserveTurn(ctx, s.Actor, d.Ref)
		if errors.Is(err, collab.ErrRecoveryRequired) {
			_, waitErr := s.Reviews.WaitForHuman(ctx, s.Actor, id, "turn_requires_reconciliation")
			if waitErr != nil {
				return nil, waitErr
			}
			return map[string]any{"status": "recovery_required"}, nil
		}
		if err != nil {
			return nil, err
		}
		if !o.Terminal {
			return nil, nil
		}
		if s.Checkpoint != nil {
			s.Checkpoint("turn_complete", id)
		}
		return map[string]any{"status": o.Status}, nil
	}
	artifact, _ := b.Correlation["artifact_id"].(string)
	if run.CurrentBundle == nil || artifact != run.CurrentBundle.ArtifactID {
		return nil, collab.ErrConflict
	}
	decision, err := s.Reviews.Decision(ctx, s.Actor, id, n)
	if errors.Is(err, collab.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"decision_id": decision.DecisionID}, nil
}
