package uicontract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestValidEvidenceFixtures(t *testing.T) {
	schema := readRepoFile(t, "schemas", "ui_evidence.v1.schema.json")
	for _, path := range fixtureFiles(t, "testdata", "ui-evidence", "v1", "valid") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			document := readFile(t, path)
			if err := ValidateAgainstSchema(schema, document); err != nil {
				t.Fatalf("schema: %v", err)
			}
			if err := ValidateUIEvidence(document); err != nil {
				t.Fatalf("semantics: %v", err)
			}
		})
	}
}

func TestInvalidEvidenceFixtures(t *testing.T) {
	schema := readRepoFile(t, "schemas", "ui_evidence.v1.schema.json")
	for _, path := range fixtureFiles(t, "testdata", "ui-evidence", "v1", "invalid") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			document := readFile(t, path)
			schemaErr := ValidateAgainstSchema(schema, document)
			semanticErr := ValidateUIEvidence(document)
			if schemaErr == nil && semanticErr == nil {
				t.Fatal("invalid evidence fixture passed both schema and semantic validation")
			}
		})
	}
}

func TestValidLayoutFixture(t *testing.T) {
	schema := readRepoFile(t, "schemas", "ui_layout.v1.schema.json")
	document := readRepoFile(t, "testdata", "ui-layout", "v1", "valid", "basic.json")
	evidence := readRepoFile(t, "testdata", "ui-evidence", "v1", "valid", "basic.json")

	if err := ValidateAgainstSchema(schema, document); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if err := ValidateUILayout(document, evidence); err != nil {
		t.Fatalf("semantics: %v", err)
	}
}

func TestLayoutEvidenceReferencesRequireEvidence(t *testing.T) {
	document := readRepoFile(t, "testdata", "ui-layout", "v1", "valid", "basic.json")
	if err := ValidateUILayout(document, nil); err == nil {
		t.Fatal("layout with evidence references passed without a ui_evidence.v1 document")
	}
}

func TestLayoutWithoutEvidenceReferencesNeedsNoEvidence(t *testing.T) {
	document := readRepoFile(t, "testdata", "ui-layout", "v1", "invalid", "duplicate-node-id.json")
	var decoded map[string]any
	if err := json.Unmarshal(document, &decoded); err != nil {
		t.Fatal(err)
	}
	root := decoded["root"].(map[string]any)
	children := root["children"].([]any)
	children[0].(map[string]any)["id"] = "n_child"
	root["children"] = children
	clean, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateUILayout(clean, nil); err != nil {
		t.Fatalf("layout without evidence references failed: %v", err)
	}
}

func TestInvalidLayoutFixtures(t *testing.T) {
	schema := readRepoFile(t, "schemas", "ui_layout.v1.schema.json")
	evidence := readRepoFile(t, "testdata", "ui-evidence", "v1", "valid", "basic.json")
	for _, path := range fixtureFiles(t, "testdata", "ui-layout", "v1", "invalid") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			document := readFile(t, path)
			schemaErr := ValidateAgainstSchema(schema, document)
			semanticErr := ValidateUILayout(document, evidence)
			if schemaErr == nil && semanticErr == nil {
				t.Fatal("invalid layout fixture passed both schema and semantic validation")
			}
		})
	}
}

func TestValidPreviewLineageFixture(t *testing.T) {
	schema := readRepoFile(t, "schemas", "ui_preview_lineage.v1.schema.json")
	document := readRepoFile(t, "testdata", "ui-preview-lineage", "v1", "valid", "basic.json")
	if err := ValidateAgainstSchema(schema, document); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if err := ValidatePreviewLineage(document); err != nil {
		t.Fatalf("semantics: %v", err)
	}
}

func TestInvalidPreviewLineageFixture(t *testing.T) {
	schema := readRepoFile(t, "schemas", "ui_preview_lineage.v1.schema.json")
	for _, path := range fixtureFiles(t, "testdata", "ui-preview-lineage", "v1", "invalid") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			document := readFile(t, path)
			schemaErr := ValidateAgainstSchema(schema, document)
			semanticErr := ValidatePreviewLineage(document)
			if schemaErr == nil && semanticErr == nil {
				t.Fatal("invalid preview lineage fixture passed both schema and semantic validation")
			}
		})
	}
}

func TestViewImageResultFixtures(t *testing.T) {
	schema := readRepoFile(t, "schemas", "view_image_result.v1.schema.json")
	for _, path := range fixtureFiles(t, "testdata", "view-image-result", "v1", "valid") {
		t.Run("valid/"+filepath.Base(path), func(t *testing.T) {
			document := readFile(t, path)
			if err := ValidateAgainstSchema(schema, document); err != nil {
				t.Fatalf("schema: %v", err)
			}
			if err := ValidateViewImageResult(document); err != nil {
				t.Fatalf("semantics: %v", err)
			}
		})
	}
	for _, path := range fixtureFiles(t, "testdata", "view-image-result", "v1", "invalid") {
		t.Run("invalid/"+filepath.Base(path), func(t *testing.T) {
			document := readFile(t, path)
			schemaErr := ValidateAgainstSchema(schema, document)
			semanticErr := ValidateViewImageResult(document)
			if schemaErr == nil && semanticErr == nil {
				t.Fatal("invalid view_image result passed both schema and semantic validation")
			}
		})
	}
}

func fixtureFiles(t *testing.T, parts ...string) []string {
	t.Helper()
	pattern := filepath.Join(append([]string{"..", ".."}, append(parts, "*.json")...)...)
	paths, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no fixtures matched %s", pattern)
	}
	return paths
}

func readRepoFile(t *testing.T, parts ...string) []byte {
	t.Helper()
	path := filepath.Join(append([]string{"..", ".."}, parts...)...)
	return readFile(t, path)
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
