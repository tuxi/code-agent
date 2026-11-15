package matrix

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func persistFixture(t *testing.T) (testWorkspace, Matrix) {
	t.Helper()
	ws := newWorkspace(t, workspaceOptions{})
	store, builder := newTestStore(t)
	const matrixID = "matrix_p1g_persist"
	document := mustBuild(t, builder, buildRequest(ws, matrixID, fullMatrix(t, ws, store, matrixID)))
	return ws, document
}

func storedRevisions(t *testing.T, root, matrixID string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(Directory), matrixID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names
}

func TestPersistIsIdempotent(t *testing.T) {
	ws, document := persistFixture(t)
	first, err := Persist(context.Background(), ws.Root, document)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.Revision != 1 {
		t.Fatalf("first persist = %+v", first)
	}
	wantPath := Directory + "/" + document.MatrixID + "/revision-000001/matrix.json"
	if first.MatrixPath != wantPath {
		t.Fatalf("matrix path = %s, want %s", first.MatrixPath, wantPath)
	}
	if !strings.HasSuffix(first.ManifestPath, "/manifest.json") {
		t.Fatalf("manifest path = %s", first.ManifestPath)
	}
	second, err := Persist(context.Background(), ws.Root, document)
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.Revision != first.Revision || second.MatrixPath != first.MatrixPath || second.ManifestPath != first.ManifestPath {
		t.Fatalf("second persist = %+v, want reuse of %+v", second, first)
	}
	revisions := storedRevisions(t, ws.Root, document.MatrixID)
	if len(revisions) != 1 || revisions[0] != "revision-000001" {
		t.Fatalf("stored revisions = %v", revisions)
	}
	storedMatrix, err := os.ReadFile(filepath.Join(ws.Root, filepath.FromSlash(first.MatrixPath)))
	if err != nil {
		t.Fatal(err)
	}
	wantMatrix, err := Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if string(storedMatrix) != string(wantMatrix) {
		t.Fatalf("stored matrix bytes differ from the built document")
	}
	storedManifest, err := os.ReadFile(filepath.Join(ws.Root, filepath.FromSlash(first.ManifestPath)))
	if err != nil {
		t.Fatal(err)
	}
	wantManifest, err := BuildManifest(document).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(storedManifest) != string(wantManifest) {
		t.Fatalf("stored manifest bytes differ from the derived manifest")
	}
}

func TestPersistAllocatesRevisionForChangedMatrix(t *testing.T) {
	ws := newWorkspace(t, workspaceOptions{})
	store, builder := newTestStore(t)
	const matrixID = "matrix_p1g_persist"
	cells := fullMatrix(t, ws, store, matrixID)
	full := mustBuild(t, builder, buildRequest(ws, matrixID, cells))
	partial := mustBuild(t, builder, buildRequest(ws, matrixID, cells[1:]))
	if full.MatrixSHA256 == partial.MatrixSHA256 {
		t.Fatal("changed matrix kept the same digest")
	}
	first, err := Persist(context.Background(), ws.Root, full)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Persist(context.Background(), ws.Root, partial)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Created || second.Revision != first.Revision+1 {
		t.Fatalf("second persist = %+v, first = %+v", second, first)
	}
	if revisions := storedRevisions(t, ws.Root, matrixID); len(revisions) != 2 {
		t.Fatalf("stored revisions = %v", revisions)
	}
}

func TestPersistRepairsCorruptStoredRevision(t *testing.T) {
	ws, document := persistFixture(t)
	first, err := Persist(context.Background(), ws.Root, document)
	if err != nil {
		t.Fatal(err)
	}
	corruptPath := filepath.Join(ws.Root, filepath.FromSlash(first.MatrixPath))
	if err := os.WriteFile(corruptPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := Persist(context.Background(), ws.Root, document)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Created || second.Revision != first.Revision+1 {
		t.Fatalf("persist after corruption = %+v, first = %+v", second, first)
	}
}

func TestPersistRejectsSymlinkedMatrixDirectory(t *testing.T) {
	ws, document := persistFixture(t)
	base := filepath.Join(ws.Root, filepath.FromSlash(Directory))
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(base, document.MatrixID)); err != nil {
		t.Fatal(err)
	}
	_, err := Persist(context.Background(), ws.Root, document)
	if err == nil || ErrorCode(err) != CodeInvalid {
		t.Fatalf("symlinked matrix directory error = %v", err)
	}
}

func TestPersistRequiresValidMatrix(t *testing.T) {
	ws := newWorkspace(t, workspaceOptions{})
	_, err := Persist(context.Background(), ws.Root, Matrix{Schema: Schema, MatrixID: "matrix_bad"})
	if err == nil || ErrorCode(err) != CodeInvalid {
		t.Fatalf("invalid matrix persist error = %v", err)
	}
}

func TestManifestIsPathFreeAndStable(t *testing.T) {
	_, document := persistFixture(t)
	manifest := BuildManifest(document)
	if manifest.Schema != ManifestSchema || manifest.MatrixSHA256 != document.MatrixSHA256 {
		t.Fatalf("manifest = %+v", manifest)
	}
	encoded, err := manifest.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "path") {
		t.Fatalf("manifest leaks filesystem paths:\n%s", encoded)
	}
	again, err := BuildManifest(document).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != string(again) {
		t.Fatal("manifest rendering is not stable")
	}
}

func TestBuildAndPersistReusesRevision(t *testing.T) {
	ws := newWorkspace(t, workspaceOptions{})
	store, builder := newTestStore(t)
	const matrixID = "matrix_p1g_build_persist"
	request := buildRequest(ws, matrixID, fullMatrix(t, ws, store, matrixID))
	first, err := builder.BuildAndPersist(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Persisted.Created || first.Manifest.MatrixSHA256 != first.Matrix.MatrixSHA256 {
		t.Fatalf("first build+persist = %+v", first.Persisted)
	}
	second, err := builder.BuildAndPersist(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if second.Persisted.Created || second.Persisted.Revision != first.Persisted.Revision {
		t.Fatalf("second build+persist = %+v", second.Persisted)
	}
	if second.Matrix.MatrixSHA256 != first.Matrix.MatrixSHA256 {
		t.Fatalf("replayed digest = %s, want %s", second.Matrix.MatrixSHA256, first.Matrix.MatrixSHA256)
	}
}

func TestDecodeRejectsInvalidMatrix(t *testing.T) {
	if _, err := Decode([]byte(`{}`)); err == nil {
		t.Fatal("invalid matrix decoded")
	}
	_, document := persistFixture(t)
	encoded, err := Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.MatrixSHA256 != document.MatrixSHA256 {
		t.Fatalf("decoded digest = %s", decoded.MatrixSHA256)
	}
}
