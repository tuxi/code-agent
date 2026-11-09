package preview

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFixtureArtifactHashes(t *testing.T) {
	root := filepath.Join("..", "..", "docs", "capability-runtime-upgrade", "fixtures", "preview", "v1")
	for _, name := range []string{
		"ready-interactive.json", "static-image.json", "pending.json", "unavailable.json",
		"failed-with-fallback.json", "failed-without-fallback.json", "unavailable-with-fallback.json",
		"expired-replay.json",
	} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, name))
			if err != nil {
				t.Fatal(err)
			}
			var artifact Artifact
			if err := json.Unmarshal(data, &artifact); err != nil {
				t.Fatal(err)
			}
			got, err := RevisionSHA256(artifact)
			if err != nil {
				t.Fatal(err)
			}
			if got != artifact.RevisionSHA256 {
				artifact.RevisionSHA256 = ""
				raw, _ := json.Marshal(artifact)
				canonical, _ := CanonicalizeJSON(raw)
				t.Fatalf("revision hash = %s, want %s\ncanonical: %s", got, artifact.RevisionSHA256, canonical)
			}
			if err := artifact.Validate(); err != nil {
				t.Fatalf("fixture artifact did not validate: %+v", err)
			}
		})
	}
}

func TestJCSSharedVectors(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "rfc8785-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Version string `json:"version"`
		Vectors []struct {
			Name      string `json:"name"`
			Input     string `json:"input"`
			Canonical string `json:"canonical"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Version != "rfc8785-jcs-v1" || len(fixture.Vectors) == 0 {
		t.Fatalf("invalid shared JCS vector file: %+v", fixture)
	}
	for _, vector := range fixture.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			got, err := CanonicalizeJSON([]byte(vector.Input))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != vector.Canonical {
				t.Fatalf("canonical JSON mismatch\n got: %s\nwant: %s", got, vector.Canonical)
			}
		})
	}
}
