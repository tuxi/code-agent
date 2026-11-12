package systemskills

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"
)

func testBundle(t *testing.T, version, body string, extraFiles ...FileEntry) fstest.MapFS {
	t.Helper()
	files := []struct {
		path string
		body string
	}{{path: "SKILL.md", body: body}}
	for _, extra := range extraFiles {
		data, err := os.ReadFile(extra.Path)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, struct{ path, body string }{path: filepath.Base(extra.Path), body: string(data)})
	}
	entries := make([]FileEntry, 0, len(files))
	bundle := fstest.MapFS{ManifestFilename: &fstest.MapFile{Mode: 0o644}}
	for _, file := range files {
		sum := sha256.Sum256([]byte(file.body))
		entries = append(entries, FileEntry{Path: file.path, SHA256: hex.EncodeToString(sum[:])})
		bundle["ui-adaptation/"+file.path] = &fstest.MapFile{Data: []byte(file.body), Mode: 0o644}
	}
	manifest := BundleManifest{
		Schema:        SchemaV1,
		BundleVersion: version,
		Skills: []SkillEntry{{
			Name:        "ui-adaptation",
			Source:      "skills/ui-adaptation",
			Destination: "ui-adaptation",
			SHA256:      DigestFiles(entries),
			Scope:       ScopeSystem,
			Files:       entries,
		}},
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	bundle[ManifestFilename].Data = append(data, '\n')
	return bundle
}

func TestInstallFreshHomeAndIdempotent(t *testing.T) {
	home := t.TempDir()
	bundle := testBundle(t, "v1", "---\nname: ui-adaptation\ndescription: layout\n---\nbody v1")
	if err := InstallForHome(home, bundle, "test"); err != nil {
		t.Fatalf("InstallForHome: %v", err)
	}
	dest := filepath.Join(home, ".codeagent", "skills", ".system")
	if _, err := os.Stat(filepath.Join(dest, "ui-adaptation", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(dest, MarkerFilename)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(markerPath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := InstallForHome(home, bundle, "test"); err != nil {
		t.Fatalf("second install: %v", err)
	}
	info, err := os.Stat(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Fatal("matching marker did not skip reinstall")
	}
}

func TestInstallUpdatePreservesUserSibling(t *testing.T) {
	home := t.TempDir()
	skillsRoot := filepath.Join(home, ".codeagent", "skills")
	userSkill := filepath.Join(skillsRoot, "mine", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(userSkill), 0o755); err != nil {
		t.Fatal(err)
	}
	userBody := []byte("---\nname: mine\ndescription: user\n---\nmine")
	if err := os.WriteFile(userSkill, userBody, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InstallForHome(home, testBundle(t, "v1", "---\nname: ui-adaptation\ndescription: layout\n---\nbody v1"), "test"); err != nil {
		t.Fatal(err)
	}
	if err := InstallForHome(home, testBundle(t, "v2", "---\nname: ui-adaptation\ndescription: layout\n---\nbody v2"), "test"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(skillsRoot, ".system", "ui-adaptation", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == "body v1" || string(got) != "---\nname: ui-adaptation\ndescription: layout\n---\nbody v2" {
		t.Fatalf("system skill not updated: %q", got)
	}
	preserved, err := os.ReadFile(userSkill)
	if err != nil {
		t.Fatal(err)
	}
	if string(preserved) != string(userBody) {
		t.Fatal("user sibling skill was modified")
	}
}

func TestMatchingMarkerRepairsUndeclaredFiles(t *testing.T) {
	home := t.TempDir()
	bundle := testBundle(t, "v1", "---\nname: ui-adaptation\ndescription: layout\n---\nbody")
	if err := InstallForHome(home, bundle, "test"); err != nil {
		t.Fatal(err)
	}
	rogue := filepath.Join(home, ".codeagent", "skills", ".system", "rogue", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(rogue), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rogue, []byte("---\nname: rogue\ndescription: rogue\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InstallForHome(home, bundle, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rogue); !os.IsNotExist(err) {
		t.Fatal("undeclared file inside .system was not removed")
	}
}

func TestInstallRepairsBrokenMarkerAndContent(t *testing.T) {
	home := t.TempDir()
	bundle := testBundle(t, "v1", "---\nname: ui-adaptation\ndescription: layout\n---\nbody")
	if err := InstallForHome(home, bundle, "test"); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(home, ".codeagent", "skills", ".system")
	if err := os.WriteFile(filepath.Join(dest, MarkerFilename), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "ui-adaptation", "SKILL.md"), []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InstallForHome(home, bundle, "test"); err != nil {
		t.Fatalf("repair install: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dest, "ui-adaptation", "SKILL.md"))
	if len(got) == 0 || string(got) == "corrupt" {
		t.Fatal("corrupted system skill was not repaired")
	}
}

func TestInstallRejectsMissingAndUnsafeBundles(t *testing.T) {
	valid := testBundle(t, "v1", "---\nname: ui-adaptation\ndescription: layout\n---\nbody")
	cases := map[string]fstest.MapFS{
		"missing-manifest": {},
		"missing-skill": func() fstest.MapFS {
			b := testBundle(t, "v1", "---\nname: ui-adaptation\ndescription: layout\n---\nbody")
			delete(b, "ui-adaptation/SKILL.md")
			return b
		}(),
		"hash-mismatch": func() fstest.MapFS {
			b := testBundle(t, "v1", "---\nname: ui-adaptation\ndescription: layout\n---\nbody")
			b["ui-adaptation/SKILL.md"].Data = []byte("tampered")
			return b
		}(),
		"missing-hash": func() fstest.MapFS {
			b := testBundle(t, "v1", "---\nname: ui-adaptation\ndescription: layout\n---\nbody")
			var m BundleManifest
			_ = json.Unmarshal(b[ManifestFilename].Data, &m)
			m.Skills[0].SHA256 = ""
			b[ManifestFilename].Data, _ = json.Marshal(m)
			return b
		}(),
		"path-traversal": func() fstest.MapFS {
			b := testBundle(t, "v1", "---\nname: ui-adaptation\ndescription: layout\n---\nbody")
			var m BundleManifest
			_ = json.Unmarshal(b[ManifestFilename].Data, &m)
			m.Skills[0].Destination = "../escape"
			b[ManifestFilename].Data, _ = json.Marshal(m)
			return b
		}(),
		"symlink": func() fstest.MapFS {
			b := testBundle(t, "v1", "---\nname: ui-adaptation\ndescription: layout\n---\nbody")
			b["ui-adaptation/link"] = &fstest.MapFile{Mode: fs.ModeSymlink | 0o777, Data: []byte("../outside")}
			return b
		}(),
	}
	for name, bundle := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if name == "missing-manifest" {
				if err := InstallForHome(root, valid, "test"); err != nil {
					t.Fatal(err)
				}
			}
			if err := InstallForHome(root, bundle, "test"); err == nil {
				t.Fatal("expected installation failure")
			}
			if _, err := os.Stat(filepath.Join(root, "escape")); !os.IsNotExist(err) {
				t.Fatal("unsafe destination escaped the skills root")
			}
		})
	}
}
