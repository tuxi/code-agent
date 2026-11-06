package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"code-agent/internal/assetref"
	"code-agent/internal/session"
	"code-agent/internal/tools"
)

type ContinuationParker func(context.Context, *session.Session, []string, json.RawMessage) error

// This is execution state, not a replay instruction. Provider calls and command
// results already committed in history must never be executed a second time.
type continuationState struct {
	Version, NextStep, MaxSteps, MaxWebSearches                     int
	Turn                                                            TurnResult
	WebSearches                                                     int
	Assets                                                          []assets.Ref
	BilledCallIDs                                                   map[string]struct{}
	Hypothesized                                                    bool
	PendingHarness, PendingHypothesis                               string
	Todos                                                           []tools.Todo
	PlanState                                                       PlanStatus
	Plan                                                            *Plan
	PlanTitle, LastAssistantText                                    string
	PlannedMutation, IndependentReviewPassed, MutatedVerifiableCode bool
	ChangeReviewCount                                               int
}

// BackgroundCheckpointResult is the terminal projection for a stop that lands
// after admission but before the resumed runner can begin executing.
func BackgroundCheckpointResult(checkpoint json.RawMessage) (TurnResult, error) {
	var state continuationState
	if err := json.Unmarshal(checkpoint, &state); err != nil {
		return TurnResult{}, err
	}
	if state.Version != 1 {
		return TurnResult{}, errors.New("invalid continuation version")
	}
	return state.Turn, nil
}

func (r *Runner) CanContinueBackground() bool {
	// Unknown stateful stop policies need their own checkpoint contract.
	return r.ParkContinuation != nil && r.RuntimeInputs != nil && r.StopPolicy == nil
}

func (r *Runner) ContinuationIdentity() string {
	encoded, _ := json.Marshal([]any{r.ModelSelection.ConfigurationID, r.ModelName, r.WireModel, r.ProviderName, r.BaseURL, r.WorkspaceRoot, r.ReasoningEffort, r.Temperature, r.VerifyCommand, r.RemindHypothesis, r.UserAssetsSupported, r.VisionSupported, r.ContinuationPolicyIdentity})
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func (r *Runner) ContinueBackground(ctx context.Context, sess *session.Session, checkpoint json.RawMessage) (TurnResult, error) {
	if !r.CanContinueBackground() || r.Model == nil || r.Tools == nil || r.ReservedTurnID == "" {
		return TurnResult{}, errors.New("background continuation unavailable")
	}
	var state continuationState
	if err := json.Unmarshal(checkpoint, &state); err != nil {
		return TurnResult{}, err
	}
	if state.Version != 1 || state.Turn.TurnID != r.ReservedTurnID || state.NextStep < 1 || state.NextStep >= state.MaxSteps || state.MaxSteps > 100000 || state.WebSearches < 0 {
		return TurnResult{}, errors.New("invalid continuation state")
	}
	r.emitSessionID = sess.ID
	r.emitTurnID = r.ReservedTurnID
	r.emitRequestID = r.RequestID
	r.emitInvocationID = ""
	r.continuation = &state
	return r.drive(ctx, sess)
}

func (r *Runner) captureContinuation(state *continuationState) (json.RawMessage, error) {
	state.Version = 1
	state.MaxSteps = r.MaxSteps
	state.MaxWebSearches = r.MaxWebSearches
	state.Turn.WaitingBackground = false
	state.BilledCallIDs = state.Turn.billedToolCallIDs
	state.Todos = r.todos
	state.PlanState = r.PlanState
	state.Plan = r.activePlan
	state.PlanTitle = r.planTitle
	state.LastAssistantText = r.lastAssistantText
	state.PlannedMutation = r.plannedMutation
	state.IndependentReviewPassed = r.independentReviewPassed
	state.MutatedVerifiableCode = r.mutatedVerifiableCode
	state.ChangeReviewCount = r.changeReviewCount
	return json.Marshal(state)
}
