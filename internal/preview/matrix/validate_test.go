package matrix

import (
	"encoding/json"
	"strings"
	"testing"

	"code-agent/internal/preview"
)

func validMatrix(t *testing.T) Matrix {
	t.Helper()
	ws := newWorkspace(t, workspaceOptions{})
	store, builder := newTestStore(t)
	const matrixID = "matrix_p1g_valid"
	return mustBuild(t, builder, buildRequest(ws, matrixID, fullMatrix(t, ws, store, matrixID)))
}

func sealClone(t *testing.T, source Matrix, mutate func(*Matrix), reseal bool) Matrix {
	t.Helper()
	out := cloneMatrix(t, source)
	mutate(&out)
	if reseal {
		out.MatrixSHA256 = ""
		if err := out.Seal(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func TestValidateAcceptsBuiltMatrix(t *testing.T) {
	document := validMatrix(t)
	if err := document.Validate(); err != nil {
		t.Fatalf("valid matrix rejected: %v", err)
	}
	encoded, err := Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatalf("decode matrix: %v", err)
	}
	if decoded.MatrixSHA256 != document.MatrixSHA256 {
		t.Fatalf("round trip digest = %s, want %s", decoded.MatrixSHA256, document.MatrixSHA256)
	}
}

func TestValidateRejectsDocumentDrift(t *testing.T) {
	source := validMatrix(t)
	tests := []struct {
		name   string
		mutate func(*Matrix)
		reseal bool
		want   string
	}{
		{"coverage mismatch", func(m *Matrix) { m.Coverage.Completed = 3 }, true, "coverage"},
		{"coverage complete flag", func(m *Matrix) { m.Coverage.Complete = false }, true, "coverage"},
		{"omitted cell", func(m *Matrix) { m.Entries = m.Entries[:1] }, true, "missing cells cannot be omitted"},
		{"cell order", func(m *Matrix) {
			m.Entries[0], m.Entries[1] = m.Entries[1], m.Entries[0]
		}, true, "canonical cell order"},
		{"duplicate cell", func(m *Matrix) { m.Entries[1] = m.Entries[0] }, true, "canonical cell order"},
		{"completed without lineage", func(m *Matrix) { m.Entries[0].Lineage = nil }, true, "requires ui_preview_lineage.v2"},
		{"completed without fallback", func(m *Matrix) { m.Entries[0].FallbackAsset = nil }, true, "requires a fallback asset identity"},
		{"completed with error", func(m *Matrix) {
			m.Entries[0].Error = &EntryError{Code: preview.LineageInvalid, Message: "x"}
		}, true, "must not carry an error"},
		{"failed with fallback", func(m *Matrix) {
			m.Entries[0].Status = StatusFailed
			m.Entries[0].Error = &EntryError{Code: preview.LineageInvalid, Message: "x"}
		}, true, "must not carry lineage or a fallback"},
		{"failed without error", func(m *Matrix) {
			m.Entries[0].Status = StatusFailed
			m.Entries[0].Lineage = nil
			m.Entries[0].FallbackAsset = nil
		}, true, "requires an error"},
		{"failed with partial identity", func(m *Matrix) {
			m.Entries[0].Status = StatusFailed
			m.Entries[0].Error = &EntryError{Code: preview.LineageInvalid, Message: "x"}
			m.Entries[0].Lineage = nil
			m.Entries[0].FallbackAsset = nil
			m.Entries[0].PreviewMode = nil
		}, true, "partial preview identity"},
		{"missing with claims", func(m *Matrix) { m.Entries[0].Status = StatusMissing }, true, "must not claim"},
		{"lineage claims another cell", func(m *Matrix) { m.Entries[0].Lineage.Target = "iphone_portrait" }, true, "lineage claims"},
		{"lineage layout digest", func(m *Matrix) {
			m.Entries[0].Lineage.LayoutRevisionSHA256 = "sha256:" + strings.Repeat("1", 64)
		}, true, "matrix layout revision"},
		{"lineage matrix id", func(m *Matrix) { m.Entries[0].Lineage.TargetMatrix.ID = "matrix_other" }, true, "belongs to matrix"},
		{"lineage target set", func(m *Matrix) {
			m.Entries[0].Lineage.TargetMatrix.Targets = []string{"ipad_landscape", "iphone_portrait", "ipad_portrait"}
		}, true, "target set"},
		{"lineage evidence", func(m *Matrix) {
			evidence := strings.Repeat("d", 64)
			m.Entries[0].Lineage.EvidenceSHA256 = &evidence
		}, true, "matrix evidence"},
		{"target matrix id", func(m *Matrix) { m.TargetMatrix.ID = "matrix_other" }, true, "does not match matrix_id"},
		{"unsorted targets", func(m *Matrix) { m.TargetMatrix.Targets = []string{"iphone_portrait", "ipad_landscape"} }, true, "sorted and unique"},
		{"shared artifact", func(m *Matrix) {
			m.Entries[1].PreviewArtifactID = m.Entries[0].PreviewArtifactID
		}, true, "referenced by more than one cell"},
		{"unknown error code", func(m *Matrix) {
			m.Entries[0].Status = StatusFailed
			m.Entries[0].Lineage = nil
			m.Entries[0].FallbackAsset = nil
			m.Entries[0].Error = &EntryError{Code: "made_up", Message: "x"}
		}, true, "not a preview or lineage failure kind"},
		{"digest tampered", func(m *Matrix) { m.MatrixSHA256 = "sha256:" + strings.Repeat("0", 64) }, false, "matrix_sha256 does not match"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			document := sealClone(t, source, tc.mutate, tc.reseal)
			err := document.Validate()
			if err == nil {
				t.Fatalf("matrix mutation was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestValidateRejectsMalformedDocuments(t *testing.T) {
	valid := validMatrix(t)
	encoded, err := Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	var withUnknown map[string]any
	if err := json.Unmarshal(encoded, &withUnknown); err != nil {
		t.Fatal(err)
	}
	withUnknown["viewer_html"] = "<div></div>"
	unknownBytes, err := json.Marshal(withUnknown)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		data []byte
	}{
		{"empty document", []byte(`{}`)},
		{"invalid json", []byte(`{"schema":`)},
		{"unknown field", unknownBytes},
		{"wrong schema", []byte(`{"schema":"ui_preview_matrix.v2"}`)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateDocument(tc.data); err == nil {
				t.Fatalf("document was accepted")
			}
		})
	}
}
