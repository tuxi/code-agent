package mcpbridge

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"time"

	"code-agent/internal/collab"
	"code-agent/internal/executionevidence"
)

// Update contains no transcript, tool arguments or command output. The cursor
// is a durable Session cursor; lifecycle is still reconstructed independently.
type Update struct {
	SessionID       string     `json:"session_id"`
	TurnID          string     `json:"turn_id"`
	Cursor          int64      `json:"cursor"`
	Changed         bool       `json:"changed"`
	ExecutionStatus string     `json:"execution_status"`
	Terminal        bool       `json:"terminal"`
	Attention       *Attention `json:"attention,omitempty"`
	HandoffTool     string     `json:"handoff_tool,omitempty"`
}

func (c *Client) WaitForUpdate(ctx context.Context, id, turn string, cursor int64, timeout time.Duration) (Update, error) {
	if timeout < 0 || timeout > 30*time.Second || cursor < 0 {
		return Update{}, errors.New("nonnegative cursor and timeout between 0 and 30 seconds required")
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	readCtx := ctx
	if timeout > 0 {
		readCtx = waitCtx
	}
	var last *Update
	for {
		r, err := c.Read(readCtx, id, turn, cursor)
		if err != nil {
			if ctx.Err() == nil && waitCtx.Err() != nil && last != nil {
				return *last, nil
			}
			return Update{}, err
		}
		u := Update{SessionID: id, TurnID: turn, Cursor: r.Cursor, ExecutionStatus: r.Status, Terminal: r.Terminal, Attention: r.Attention}
		for _, event := range r.Events {
			switch event.Kind {
			case "turn_accepted", "turn_started", "turn_resumed", "turn_queued", "turn_paused", "turn_finished", "turn_failed", "turn_cancelled", "tool_finished":
				u.Changed = true
			}
		}
		if r.Terminal {
			u.HandoffTool = "codeagent_get_handoff"
		}
		if u.Changed || r.Terminal || r.AttentionRequired || timeout == 0 {
			return u, nil
		}
		// Advancing over text/model deltas prevents returning them next time;
		// callers never have to carry or repeatedly read the transcript.
		cursor = r.Cursor
		last = &u
		select {
		case <-ctx.Done():
			return Update{}, ctx.Err()
		case <-waitCtx.Done():
			return u, nil
		case <-time.After(500 * time.Millisecond):
		}
	}
}

type Admission struct {
	Outcome    string   `json:"outcome"`
	SessionID  string   `json:"session_id"`
	RequestID  string   `json:"request_id"`
	Receipt    *Receipt `json:"receipt,omitempty"`
	NextAction string   `json:"next_action"`
}

// LookupAdmission never submits. Absence from a bounded, complete observation
// does not prove non-admission: a concurrent request may still be in flight.
func (c *Client) LookupAdmission(ctx context.Context, id, request string) (Admission, error) {
	if err := c.checkSession(id); err != nil {
		return Admission{}, err
	}
	if !validID(request) {
		return Admission{}, errors.New("valid request_id required")
	}
	if c.reattached {
		if err := c.verifyRuntimeIdentity(ctx); err != nil {
			return Admission{}, err
		}
	}
	events, err := c.events(ctx, id, 0)
	if err != nil {
		return Admission{}, err
	}
	a := Admission{Outcome: "unknown", SessionID: id, RequestID: request, NextAction: "Do not allocate a new request_id. Retry only the identical payload and request_id in the original Bridge process; after restart reconcile durable history first."}
	for _, e := range events {
		if e.Kind != "turn_accepted" || e.RequestID != request {
			continue
		}
		if !validID(e.TurnID) || e.Seq <= 0 || a.Receipt != nil {
			return Admission{}, errors.New("ambiguous durable admission")
		}
		a.Receipt = &Receipt{true, id, e.TurnID, request, e.Seq}
		a.Outcome, a.NextAction = "admitted", "Observe this exact Turn; do not resubmit."
	}
	return a, nil
}

type EvidenceRef struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	SourceSeq int64  `json:"source_seq"`
	CallID    string `json:"call_id"`
	Tool      string `json:"tool"`
}

type HandoffCommand struct {
	Command    string      `json:"command"`
	Status     string      `json:"status,omitempty"`
	ExitCode   *int        `json:"exit_code"`
	Decision   string      `json:"decision,omitempty"`
	DurationMS int64       `json:"duration_ms"`
	Evidence   EvidenceRef `json:"evidence"`
}

// Handoff is an index into existing evidence, not another Task protocol or a
// model-authored completion report. Manual artifacts remain mutable.
type Handoff struct {
	ValidationProvenance
	TaskID            string                    `json:"task_id,omitempty"`
	TaskRevision      string                    `json:"task_revision,omitempty"`
	TaskAcceptance    string                    `json:"task_acceptance,omitempty"`
	SessionID         string                    `json:"session_id"`
	TurnID            string                    `json:"turn_id"`
	RequestID         string                    `json:"request_id,omitempty"`
	Cursor            int64                     `json:"cursor"`
	AttemptID         *string                   `json:"attempt_id"`
	ExecutionStatus   string                    `json:"execution_status"`
	ValidationStatus  string                    `json:"validation_status"`
	AcceptanceStatus  string                    `json:"acceptance_status"`
	Validation        *executionevidence.Report `json:"validation,omitempty"`
	EvidenceNote      string                    `json:"evidence_note,omitempty"`
	Baseline          string                    `json:"baseline_commit"`
	EvidenceSource    string                    `json:"evidence_source"`
	DeliveryCommit    string                    `json:"delivery_commit,omitempty"`
	HeadCommit        string                    `json:"head_commit,omitempty"`
	HeadMoved         bool                      `json:"head_moved"`
	WorktreeDirty     bool                      `json:"worktree_dirty"`
	EvidenceComplete  bool                      `json:"evidence_complete"`
	DiffSHA256        string                    `json:"diff_sha256"`
	DiffTruncated     bool                      `json:"diff_truncated"`
	ChangedFiles      []string                  `json:"changed_files"`
	FilesTruncated    bool                      `json:"files_truncated"`
	Commands          []HandoffCommand          `json:"commands"`
	CommandsTruncated bool                      `json:"commands_truncated"`
	UnparsedCallIDs   []string                  `json:"unparsed_command_call_ids"`
	ArtifactTool      string                    `json:"artifact_tool"`
	Note              string                    `json:"note"`
}

func (c *Client) HandoffForTask(ctx context.Context, id, turn, task string) (Handoff, error) {
	if task == "" {
		return c.Handoff(ctx, id, turn)
	}
	snapshot, err := c.ReadTask(ctx, task)
	if err != nil {
		return Handoff{}, err
	}
	h, err := c.Handoff(ctx, id, turn)
	if err != nil {
		return Handoff{}, err
	}
	if err = c.bindHandoffTask(&h, snapshot); err != nil {
		return Handoff{}, err
	}
	return h, nil
}

func (c *Client) bindHandoffTask(h *Handoff, snapshot collab.Snapshot) error {
	if c.binding == nil {
		return collab.ErrForbidden
	}
	for _, attempt := range snapshot.Attempts {
		source := attempt.SourceRef
		if source.SessionID != h.SessionID || source.TurnID != h.TurnID {
			continue
		}
		if h.AttemptID != nil || attempt.TaskID != snapshot.TaskID || source.Kind != "session_event" || source.ServerID != c.binding.verifiedServerID || source.StoreID != c.binding.verifiedStoreID {
			return collab.ErrConflict
		}
		id := attempt.AttemptID
		h.AttemptID = &id
	}
	if h.AttemptID == nil {
		return collab.ErrNotFound
	}
	h.TaskID, h.TaskRevision, h.TaskAcceptance = snapshot.TaskID, snapshot.Revision, snapshot.TaskAcceptance
	// Task acceptance is authoritative for the Task, not an assertion that this
	// mutable workspace hash is the candidate that passed managed Finalize.
	return nil
}

func (c *Client) Handoff(ctx context.Context, id, turn string) (Handoff, error) {
	// Hold the same send boundary while reading the index. External writers
	// are not locked; this is explicitly not a managed immutable bundle.
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	a, err := c.artifacts(ctx, id, turn)
	if err != nil {
		return Handoff{}, err
	}
	r, err := c.Read(ctx, id, turn, 0)
	if err != nil {
		return Handoff{}, err
	}
	if !r.Terminal {
		return Handoff{}, errors.New("Turn changed while collecting handoff")
	}
	if r.Status != a.TurnStatus || !reflect.DeepEqual(r.ValidationProvenance, a.ValidationProvenance) {
		return Handoff{}, bridgeError("evidence_incomplete", nil)
	}
	var tracked, untracked string
	var cut, cut2 bool
	if c.observeOnly {
		names, truncated, err := c.rangeFiles(ctx)
		if err != nil {
			return Handoff{}, err
		}
		cut = truncated
		tracked = strings.Join(names, "\x00")
		if tracked != "" {
			tracked += "\x00"
		}
	} else {
		tracked, cut, err = gitOutput(ctx, c.root, "diff", "--name-only", "-z", "--no-ext-diff", "--no-textconv", c.baseline, "--")
		if err != nil {
			return Handoff{}, err
		}
		untracked, cut2, err = gitOutput(ctx, c.root, "ls-files", "--others", "--exclude-standard", "-z")
		if err != nil {
			return Handoff{}, err
		}
	}
	h := Handoff{SessionID: id, TurnID: turn, ExecutionStatus: a.TurnStatus, ValidationStatus: "unknown", AcceptanceStatus: "not_reviewed", Validation: a.ExecutionEvidence, EvidenceNote: a.EvidenceNote, Baseline: a.Baseline, DiffSHA256: a.DiffSHA256, DiffTruncated: a.Truncated, FilesTruncated: cut || cut2, ChangedFiles: []string{}, Commands: []HandoffCommand{}, UnparsedCallIDs: a.UnparsedResults, ArtifactTool: "codeagent_get_artifacts", Note: "Current workspace evidence, not an immutable/per-Turn snapshot. Validation covers recorded Turn events only, not external changes or visual acceptance. attempt_id is unknown here; use Task Snapshot for authoritative associations/acceptance. Read referenced evidence on demand."}
	h.ValidationProvenance = r.ValidationProvenance
	h.EvidenceSource, h.EvidenceComplete = a.EvidenceSource, a.EvidenceComplete
	if c.observeOnly {
		h.DeliveryCommit, h.HeadCommit, h.HeadMoved, h.WorktreeDirty = a.DeliveryCommit, a.HeadCommit, a.HeadMoved, a.WorktreeDirty
		h.Note = "Recorded commit-range evidence: changed_files and diff cover baseline..delivery and are unaffected by the current HEAD, branch, or uncommitted changes. head_commit/head_moved/worktree_dirty are reported separately and excluded from the reviewed diff. attempt_id is unknown here; use Task Snapshot for authoritative associations/acceptance. Validation covers recorded Turn events only, not external changes or visual acceptance."
	}
	if h.Validation != nil {
		h.ValidationStatus = "partial"
		if h.Validation.Status == "evidence_complete" {
			h.ValidationStatus = "complete"
		}
	}
	files := map[string]bool{}
	// A capped -z response can end inside a filename. Do not report the
	// incomplete tail as if it were a real path.
	if cut {
		if at := strings.LastIndexByte(tracked, 0); at >= 0 {
			tracked = tracked[:at+1]
		} else {
			tracked = ""
		}
	}
	if cut2 {
		if at := strings.LastIndexByte(untracked, 0); at >= 0 {
			untracked = untracked[:at+1]
		} else {
			untracked = ""
		}
	}
	for _, name := range strings.Split(tracked+untracked, "\x00") {
		if name != "" {
			files[name] = true
		}
	}
	for name := range files {
		h.ChangedFiles = append(h.ChangedFiles, name)
	}
	sort.Strings(h.ChangedFiles)
	if len(h.ChangedFiles) > 200 {
		h.ChangedFiles = h.ChangedFiles[:200]
		h.FilesTruncated = true
	}
	h.Cursor = r.Cursor
	for _, e := range r.Events {
		if e.Kind == "turn_accepted" {
			h.RequestID = e.RequestID
		}
	}
	for _, command := range a.CommandResults {
		if len(h.Commands) == 64 {
			h.CommandsTruncated = true
			break
		}
		h.Commands = append(h.Commands, HandoffCommand{Command: command.Command, Status: command.Status, ExitCode: command.ExitCode, Decision: command.Decision, DurationMS: command.DurationMS, Evidence: EvidenceRef{id, turn, command.SourceSeq, command.CallID, "codeagent_read_session"}})
	}
	if h.DiffTruncated || h.FilesTruncated || h.CommandsTruncated {
		h.ValidationStatus = "partial"
	}
	verified, err := c.artifacts(ctx, id, turn)
	if err != nil {
		return Handoff{}, err
	}
	if verified.DiffSHA256 != a.DiffSHA256 || verified.Truncated != a.Truncated {
		return Handoff{}, errors.New("workspace changed while collecting handoff; stop writers and collect again")
	}
	if verified.TurnStatus != a.TurnStatus || !reflect.DeepEqual(verified.ValidationProvenance, a.ValidationProvenance) {
		return Handoff{}, bridgeError("evidence_incomplete", nil)
	}
	return h, nil
}
