// Package previewmatrix exposes the built-in ui_preview_matrix tool. It binds
// the P1-G aggregation backend to a model-callable surface: the model names the
// reviewed layout revision, the target matrix, the states, and one controlled
// preview reference per cell; the runtime resolves every reference inside the
// current session, builds and persists the sealed ui_preview_matrix.v1 document,
// and returns a stable, path-sanitized projection.
package previewmatrix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"code-agent/internal/preview"
	"code-agent/internal/preview/matrix"
	"code-agent/internal/tools"
	previewtool "code-agent/internal/tools/preview"
	"code-agent/schemas"
)

// Capability is the runtime-owned capability that produced the aggregated
// preview revisions. It matches ui_preview/uilayout so matrix rows stay in the
// same capability family.
const Capability = "ui-adaptation"

const (
	// previewProducerID is the binding name of the built-in producer that owns
	// preview artifact keys. Artifact identity is derived from the runtime
	// namespace, never accepted from the model.
	previewProducerID = "ui_preview"
	// maxEntries bounds one request before the backend enforces its own budget.
	maxEntries = matrix.MaxEntries
)

var (
	matrixIDPattern    = regexp.MustCompile(`^matrix_[A-Za-z0-9._:-]{1,180}$`)
	publicIDPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,191}$`)
	revisionSHAPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

type Tool struct{}

func NewTool() *Tool { return &Tool{} }

func (*Tool) Name() string { return "ui_preview_matrix" }

func (*Tool) Description() string {
	return "Aggregate independent ui_preview revisions into one sealed ui_preview_matrix.v1 document and persist it under .codeagent/assets/ui-preview-matrix/. " +
		"Pass the reviewed ui_layout artifact/revision/revision_sha256, the target matrix (id must equal matrix_id), the screen states, and one entry per target/state cell. " +
		"A completed cell references a preview through preview_ref: use artifact_key (the key you passed to ui_preview) or an explicit artifact_id+revision that was produced in THIS session; an optional revision_sha256 must match the stored revision. " +
		"The runtime derives and verifies artifact identity — internal paths, cross-session references, and forged artifact IDs are rejected. " +
		"Omit preview_ref and error for a missing cell; set error to declare a failed cell. Failed and missing cells are preserved exactly as the backend records them. " +
		"Returns matrix id/revision/digest, coverage, manifest and fallback asset identity, and the resolved preview references per completed cell."
}

func (*Tool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"matrix_id": {
			Type:        "string",
			Description: "Matrix identity, matching matrix_<id>. The same value is the persisted matrix artifact identity.",
		},
		"layout": {
			Type:        "object",
			Description: "Reviewed ui_layout revision this matrix aggregates. revision_sha256 must equal the persisted layout revision digest.",
			Properties: map[string]tools.Property{
				"artifact_id":     {Type: "string", Description: "ui_layout artifact id (layout_...)."},
				"revision":        {Type: "integer", Description: "Positive layout revision."},
				"revision_sha256": {Type: "string", Description: "sha256:<64 hex> layout revision digest reported by ui_layout."},
			},
			Required: []string{"artifact_id", "revision", "revision_sha256"},
		},
		"target_matrix": {
			Type:        "object",
			Description: "Target set shared by every cell. id must equal matrix_id; targets must be declared by the layout revision.",
			Properties: map[string]tools.Property{
				"id": {Type: "string", Description: "Target matrix identity; must equal matrix_id."},
				"targets": {
					Type:        "array",
					Description: "Responsive targets (duo_folded, duo_unfolded, iphone_portrait, iphone_landscape, ipad_portrait, ipad_landscape).",
					Items:       &tools.Property{Type: "string"},
				},
			},
			Required: []string{"id", "targets"},
		},
		"states": {
			Type:        "array",
			Description: "Screen states to aggregate, in the layout's declaration order. An empty array means every state the layout declares.",
			Items:       &tools.Property{Type: "string"},
		},
		"entries": {
			Type:        "array",
			Description: "One entry per target/state cell. Duplicate cells and artifacts shared by two cells are rejected.",
			Items: &tools.Property{
				Type: "object",
				Properties: map[string]tools.Property{
					"target": {Type: "string", Description: "Cell target; must belong to target_matrix.targets."},
					"state":  {Type: "string", Description: "Cell state; must belong to states."},
					"preview_ref": {
						Type:        "object",
						Description: "Controlled reference to one preview revision produced in this session. Exactly one of artifact_key or artifact_id.",
						Properties: map[string]tools.Property{
							"artifact_key":    {Type: "string", Description: "artifact_key previously passed to ui_preview."},
							"artifact_id":     {Type: "string", Description: "preview_<...> artifact id; requires revision."},
							"revision":        {Type: "integer", Description: "Optional positive revision. Omitted with artifact_key means the latest revision."},
							"revision_sha256": {Type: "string", Description: "Optional expected sha256:<64 hex> revision digest; a mismatch rejects the cell."},
						},
					},
					"error": {
						Type:        "object",
						Description: "Declared failure for this cell. Mutually exclusive with preview_ref.",
						Properties: map[string]tools.Property{
							"code":    {Type: "string", Description: "Failure kind from the closed matrix error set."},
							"message": {Type: "string", Description: "Bounded human-readable failure message."},
						},
						Required: []string{"code", "message"},
					},
				},
				Required: []string{"target", "state"},
			},
		},
	}, "matrix_id", "layout", "target_matrix", "states", "entries").Closed().JSON()
}

func (*Tool) OutputSchema() json.RawMessage { return schemas.UIPreviewMatrixResultV1 }

func (*Tool) ToolBinding() tools.Binding {
	return tools.Binding{
		Name:            "ui_preview_matrix",
		ModelVisibility: tools.VisibilityDirect,
		Executor:        tools.ExecutorServer,
		Capability:      Capability,
	}
}

type input struct {
	MatrixID     string            `json:"matrix_id"`
	Layout       layoutInput       `json:"layout"`
	TargetMatrix targetMatrixInput `json:"target_matrix"`
	States       []string          `json:"states"`
	Entries      []entryInput      `json:"entries"`
}

type layoutInput struct {
	ArtifactID     string `json:"artifact_id"`
	Revision       int64  `json:"revision"`
	RevisionSHA256 string `json:"revision_sha256"`
}

type targetMatrixInput struct {
	ID      string   `json:"id"`
	Targets []string `json:"targets"`
}

type entryInput struct {
	Target     string           `json:"target"`
	State      string           `json:"state"`
	PreviewRef *previewRefInput `json:"preview_ref"`
	Error      *entryErrorInput `json:"error"`
}

type previewRefInput struct {
	ArtifactKey    string `json:"artifact_key"`
	ArtifactID     string `json:"artifact_id"`
	Revision       *int64 `json:"revision"`
	RevisionSHA256 string `json:"revision_sha256"`
}

type entryErrorInput struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (t *Tool) Execute(ctx context.Context, ec tools.ExecutionContext, raw json.RawMessage) (tools.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return tools.ToolResult{}, err
	}
	var in input
	if err := decodeStrict(raw, &in); err != nil {
		return tools.FailureResult("invalid_arguments", "ui_preview_matrix input is invalid: "+err.Error()), nil
	}
	if strings.TrimSpace(ec.WorkspaceRoot) == "" {
		return tools.FailureResult("unavailable", "ui_preview_matrix requires a workspace root"), nil
	}
	if !publicIDPattern.MatchString(strings.TrimSpace(ec.SessionID)) {
		return tools.FailureResult("unavailable", "ui_preview_matrix requires a session identity"), nil
	}
	if ec.PreviewStore == nil {
		return tools.FailureResult("unavailable", "ui_preview_matrix requires the preview store"), nil
	}
	if !matrixIDPattern.MatchString(in.MatrixID) {
		return tools.FailureResult("invalid_arguments", "ui_preview_matrix matrix_id must match matrix_<id>"), nil
	}
	if in.TargetMatrix.ID != in.MatrixID {
		return tools.FailureResult("invalid_arguments", fmt.Sprintf(
			"ui_preview_matrix target_matrix.id %q must equal matrix_id %q", in.TargetMatrix.ID, in.MatrixID)), nil
	}
	if !revisionSHAPattern.MatchString(in.Layout.RevisionSHA256) {
		return tools.FailureResult("invalid_arguments",
			"ui_preview_matrix layout.revision_sha256 must be sha256:<64 lowercase hex>"), nil
	}
	if len(in.Entries) > maxEntries {
		return tools.FailureResult("invalid_arguments", fmt.Sprintf(
			"ui_preview_matrix accepts at most %d entries", maxEntries)), nil
	}
	root, err := filepath.Abs(ec.WorkspaceRoot)
	if err != nil {
		return tools.FailureResult("invalid_arguments", "ui_preview_matrix workspace could not be resolved"), nil
	}

	cells, failure := t.resolveCells(ctx, ec, in)
	if failure != nil {
		return *failure, nil
	}
	request := matrix.Request{
		SessionID:     ec.SessionID,
		MatrixID:      in.MatrixID,
		WorkspaceRoot: root,
		Layout:        matrix.LayoutRef{ArtifactID: in.Layout.ArtifactID, Revision: in.Layout.Revision},
		Targets:       append([]string(nil), in.TargetMatrix.Targets...),
		States:        append([]string(nil), in.States...),
		Cells:         cells,
	}
	document, err := matrix.NewBuilder(ec.PreviewStore).Build(ctx, request)
	if err != nil {
		return buildFailure(err), nil
	}
	if !strings.EqualFold(document.Layout.RevisionSHA256, in.Layout.RevisionSHA256) {
		return tools.FailureResult("invalid_arguments", fmt.Sprintf(
			"ui_preview_matrix layout revision digest does not match: the stored revision is %s, the request declared %s",
			document.Layout.RevisionSHA256, in.Layout.RevisionSHA256)), nil
	}
	persisted, err := matrix.Persist(ctx, root, document)
	if err != nil {
		return buildFailure(err), nil
	}
	manifest := matrix.BuildManifest(document)
	manifestBytes, err := manifest.Marshal()
	if err != nil {
		return tools.FailureResult("runtime", "ui_preview_matrix manifest could not be encoded"), nil
	}
	out := buildResult(document, manifest, manifestBytes, persisted)
	output, err := out.marshal()
	if err != nil {
		return tools.FailureResult("runtime", "ui_preview_matrix result could not be encoded"), nil
	}
	verb := "Built"
	if !persisted.Created {
		verb = "Reused"
	}
	return tools.ToolResult{Content: modelContent(verb, out), Output: output}, nil
}

// resolveCells turns model-provided references into backend CellRefs. A
// malformed reference is a request error; a well-formed reference that does not
// resolve, belongs to another session, or fails its digest is recorded as a
// failed cell so the expected cell is never dropped.
func (t *Tool) resolveCells(ctx context.Context, ec tools.ExecutionContext, in input) ([]matrix.CellRef, *tools.ToolResult) {
	cells := make([]matrix.CellRef, 0, len(in.Entries))
	for index, entry := range in.Entries {
		cell := entry.Target + "/" + entry.State
		if entry.PreviewRef != nil && entry.Error != nil {
			failure := tools.FailureResult("invalid_arguments", fmt.Sprintf(
				"ui_preview_matrix cell %s declares both preview_ref and error", cell))
			return nil, &failure
		}
		if entry.Error != nil {
			cells = append(cells, matrix.CellRef{
				Target: entry.Target,
				State:  entry.State,
				Error:  &matrix.EntryError{Code: entry.Error.Code, Message: entry.Error.Message},
			})
			continue
		}
		if entry.PreviewRef == nil {
			cells = append(cells, matrix.CellRef{Target: entry.Target, State: entry.State})
			continue
		}
		ref, err := validatePreviewRef(*entry.PreviewRef)
		if err != nil {
			failure := tools.FailureResult("invalid_arguments", fmt.Sprintf(
				"ui_preview_matrix entry %d (%s): %v", index, cell, err))
			return nil, &failure
		}
		artifact, resolveErr := t.resolvePreviewRef(ctx, ec, ref)
		if resolveErr != nil {
			cells = append(cells, matrix.CellRef{
				Target: entry.Target,
				State:  entry.State,
				Error:  previewFailure(resolveErr),
			})
			continue
		}
		if artifact.SessionID != ec.SessionID {
			cells = append(cells, matrix.CellRef{
				Target: entry.Target,
				State:  entry.State,
				Error: &matrix.EntryError{
					Code:    string(preview.CodeArtifactMissing),
					Message: "Preview artifact does not belong to this session.",
				},
			})
			continue
		}
		if ref.RevisionSHA256 != "" && !strings.EqualFold(ref.RevisionSHA256, artifact.RevisionSHA256) {
			cells = append(cells, matrix.CellRef{
				Target: entry.Target,
				State:  entry.State,
				Error: &matrix.EntryError{
					Code:    string(preview.CodeDigestMismatch),
					Message: "Preview revision digest does not match the supplied preview_ref.",
				},
			})
			continue
		}
		cells = append(cells, matrix.CellRef{
			Target:            entry.Target,
			State:             entry.State,
			PreviewArtifactID: artifact.ArtifactID,
			PreviewRevision:   artifact.Revision,
		})
	}
	return cells, nil
}

func validatePreviewRef(ref previewRefInput) (previewRefInput, error) {
	key := strings.TrimSpace(ref.ArtifactKey)
	id := strings.TrimSpace(ref.ArtifactID)
	switch {
	case key == "" && id == "":
		return ref, fmt.Errorf("preview_ref requires artifact_key or artifact_id")
	case key != "" && id != "":
		return ref, fmt.Errorf("preview_ref must not combine artifact_key and artifact_id")
	}
	if id != "" {
		if !publicIDPattern.MatchString(id) {
			return ref, fmt.Errorf("preview_ref.artifact_id is not a valid preview artifact id")
		}
		if ref.Revision == nil || *ref.Revision < 1 {
			return ref, fmt.Errorf("preview_ref.revision is required with artifact_id")
		}
	}
	if key != "" && !publicIDPattern.MatchString(key) {
		return ref, fmt.Errorf("preview_ref.artifact_key is not a valid artifact key")
	}
	if ref.Revision != nil && *ref.Revision < 1 {
		return ref, fmt.Errorf("preview_ref.revision must be at least 1")
	}
	if ref.RevisionSHA256 != "" && !revisionSHAPattern.MatchString(ref.RevisionSHA256) {
		return ref, fmt.Errorf("preview_ref.revision_sha256 must be sha256:<64 lowercase hex>")
	}
	return ref, nil
}

func (t *Tool) resolvePreviewRef(ctx context.Context, ec tools.ExecutionContext, ref previewRefInput) (preview.Artifact, error) {
	if id := strings.TrimSpace(ref.ArtifactID); id != "" {
		return ec.PreviewStore.GetArtifact(ctx, ec.SessionID, id, *ref.Revision)
	}
	namespace := preview.Namespace{
		SessionID:   ec.SessionID,
		Capability:  previewtool.Capability,
		ProducerID:  previewProducerID,
		ArtifactKey: strings.TrimSpace(ref.ArtifactKey),
	}
	if ref.Revision != nil {
		return ec.PreviewStore.GetArtifact(ctx, ec.SessionID, preview.ArtifactID(namespace), *ref.Revision)
	}
	artifact, found, err := ec.PreviewStore.Latest(ctx, namespace)
	if err != nil {
		return preview.Artifact{}, err
	}
	if !found {
		return preview.Artifact{}, preview.NewError(preview.CodeArtifactMissing,
			"No preview revision exists for this artifact_key.", false)
	}
	return artifact, nil
}

func previewFailure(err error) *matrix.EntryError {
	var previewErr *preview.Error
	if errors.As(err, &previewErr) && previewErr.Message != "" {
		code := string(previewErr.Code)
		if code == "" {
			code = string(preview.CodeArtifactMissing)
		}
		return &matrix.EntryError{Code: code, Message: previewErr.Message}
	}
	return &matrix.EntryError{
		Code:    string(preview.CodeStoreUnavailable),
		Message: "Preview store is unavailable.",
	}
}

func buildFailure(err error) tools.ToolResult {
	code := matrix.ErrorCode(err)
	kind := "runtime"
	switch code {
	case matrix.CodeInvalid, matrix.CodeLayoutInvalid, matrix.CodeLayoutMismatch:
		kind = "invalid_arguments"
	case matrix.CodeLayoutUnavailable:
		kind = "not_found"
	case matrix.CodeStoreUnavailable:
		kind = "unavailable"
	}
	return tools.FailureResult(kind, "ui_preview_matrix: "+err.Error())
}

func decodeStrict(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		return fmt.Errorf("input is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

var (
	_ tools.Tool                 = (*Tool)(nil)
	_ tools.BindingProvider      = (*Tool)(nil)
	_ tools.OutputSchemaProvider = (*Tool)(nil)
)
