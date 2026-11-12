// Package systemskills owns the manifest and installation contract for skills
// shipped by CodeAgent. System bundles are immutable inputs: callers verify the
// manifest and every file before the installer mutates the host skill root.
package systemskills

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"code-agent/internal/skills"
)

const (
	SchemaV1         = "codeagent.system-skills.v1"
	ManifestFilename = "system-skills.json"
	MarkerFilename   = ".codeagent-system-skills.json"
	MarkerSalt       = "codeagent-system-skills-v1"
	ScopeSystem      = "system"
)

// BundleManifest is the single lock shared by the codeagentd embedded bundle,
// AgentKit resources, and release validation.
type BundleManifest struct {
	Schema        string       `json:"schema"`
	BundleVersion string       `json:"bundle_version"`
	Skills        []SkillEntry `json:"skills"`
}

type SkillEntry struct {
	Name         string      `json:"name"`
	Source       string      `json:"source"`
	Destination  string      `json:"destination"`
	SHA256       string      `json:"sha256"`
	Scope        string      `json:"scope"`
	Version      string      `json:"version,omitempty"`
	Dependencies []string    `json:"dependencies,omitempty"`
	References   []string    `json:"references,omitempty"`
	Files        []FileEntry `json:"files"`
}

type FileEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Marker records the exact bundle that owns .system. It is intentionally data,
// not a version-only stamp: a deleted/corrupted installed file triggers repair.
type Marker struct {
	Schema        string      `json:"schema"`
	BundleVersion string      `json:"bundle_version"`
	Fingerprint   string      `json:"fingerprint"`
	Source        string      `json:"source"`
	Files         []FileEntry `json:"files"`
}

func LoadManifest(data []byte) (*BundleManifest, error) {
	var manifest BundleManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse system skills manifest: %w", err)
	}
	if err := ValidateManifest(&manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func LoadManifestFile(filename string) (*BundleManifest, []byte, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, nil, fmt.Errorf("read system skills manifest: %w", err)
	}
	manifest, err := LoadManifest(data)
	if err != nil {
		return nil, nil, err
	}
	return manifest, data, nil
}

func ReadManifestFS(fsys fs.FS) (*BundleManifest, []byte, error) {
	data, err := fs.ReadFile(fsys, ManifestFilename)
	if err != nil {
		return nil, nil, fmt.Errorf("read embedded system skills manifest: %w", err)
	}
	manifest, err := LoadManifest(data)
	if err != nil {
		return nil, nil, err
	}
	return manifest, data, nil
}

func ValidateManifest(manifest *BundleManifest) error {
	if manifest.Schema != SchemaV1 {
		return fmt.Errorf("unsupported system skills schema %q", manifest.Schema)
	}
	if strings.TrimSpace(manifest.BundleVersion) == "" {
		return fmt.Errorf("system skills bundle_version is required")
	}
	if len(manifest.Skills) == 0 {
		return fmt.Errorf("system skills manifest has no skills")
	}
	names := make(map[string]struct{}, len(manifest.Skills))
	destinations := make(map[string]struct{}, len(manifest.Skills))
	for i := range manifest.Skills {
		entry := &manifest.Skills[i]
		if !validName(entry.Name) {
			return fmt.Errorf("skill[%d]: invalid name %q", i, entry.Name)
		}
		if _, exists := names[entry.Name]; exists {
			return fmt.Errorf("duplicate system skill name %q", entry.Name)
		}
		names[entry.Name] = struct{}{}
		if entry.Scope != ScopeSystem {
			return fmt.Errorf("skill %q: scope must be %q", entry.Name, ScopeSystem)
		}
		if entry.Destination != entry.Name || !validName(entry.Destination) {
			return fmt.Errorf("skill %q: destination must match the skill name", entry.Name)
		}
		if _, exists := destinations[entry.Destination]; exists {
			return fmt.Errorf("duplicate system skill destination %q", entry.Destination)
		}
		destinations[entry.Destination] = struct{}{}
		if !validSourcePath(entry.Source) {
			return fmt.Errorf("skill %q: unsafe source path %q", entry.Name, entry.Source)
		}
		if !validSHA256(entry.SHA256) {
			return fmt.Errorf("skill %q: sha256 is missing or invalid", entry.Name)
		}
		if len(entry.Files) == 0 {
			return fmt.Errorf("skill %q: files are missing", entry.Name)
		}
		files := make(map[string]struct{}, len(entry.Files))
		for _, file := range entry.Files {
			if !validRelativePath(file.Path) {
				return fmt.Errorf("skill %q: unsafe file path %q", entry.Name, file.Path)
			}
			if _, exists := files[file.Path]; exists {
				return fmt.Errorf("skill %q: duplicate file %q", entry.Name, file.Path)
			}
			files[file.Path] = struct{}{}
			if !validSHA256(file.SHA256) {
				return fmt.Errorf("skill %q file %q: invalid sha256", entry.Name, file.Path)
			}
		}
		if _, ok := files["SKILL.md"]; !ok {
			return fmt.Errorf("skill %q: SKILL.md must be declared", entry.Name)
		}
	}
	for _, entry := range manifest.Skills {
		for _, dependency := range entry.Dependencies {
			if _, ok := names[dependency]; !ok {
				return fmt.Errorf("skill %q: unknown dependency %q", entry.Name, dependency)
			}
		}
	}
	return nil
}

// ValidateBundle verifies every declared file and rejects undeclared skill
// directories. It is used for both generated and committed bundles.
func ValidateBundle(fsys fs.FS, manifest *BundleManifest) error {
	if err := ValidateManifest(manifest); err != nil {
		return err
	}
	declared := make(map[string]map[string]struct{}, len(manifest.Skills))
	for _, entry := range manifest.Skills {
		files := make(map[string]struct{}, len(entry.Files))
		for _, file := range entry.Files {
			rel := path.Join(entry.Destination, file.Path)
			data, err := fs.ReadFile(fsys, rel)
			if err != nil {
				return fmt.Errorf("skill %q: read %s: %w", entry.Name, rel, err)
			}
			sum := sha256.Sum256(data)
			if got := hex.EncodeToString(sum[:]); got != file.SHA256 {
				return fmt.Errorf("skill %q: hash mismatch for %s", entry.Name, rel)
			}
			files[file.Path] = struct{}{}
		}
		if got := DigestFiles(entry.Files); got != entry.SHA256 {
			return fmt.Errorf("skill %q: aggregate hash mismatch", entry.Name)
		}
		declared[entry.Destination] = files
	}

	err := fs.WalkDir(fsys, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." || name == ManifestFilename || name == MarkerFilename {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("bundle contains symlink %q", name)
		}
		if entry.IsDir() {
			parts := strings.Split(name, "/")
			if len(parts) == 1 {
				if _, ok := declared[name]; !ok {
					return fmt.Errorf("bundle contains undeclared skill directory %q", name)
				}
			}
			return nil
		}
		parts := strings.Split(name, "/")
		if len(parts) < 2 {
			return fmt.Errorf("bundle contains undeclared top-level file %q", name)
		}
		files, ok := declared[parts[0]]
		if !ok {
			return fmt.Errorf("bundle contains undeclared skill %q", parts[0])
		}
		rel := strings.Join(parts[1:], "/")
		if _, ok := files[rel]; !ok {
			return fmt.Errorf("bundle contains undeclared file %q", name)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

func UpdateManifestHashes(repoRoot string, manifest *BundleManifest) error {
	for i := range manifest.Skills {
		entry := &manifest.Skills[i]
		if !validSourcePath(entry.Source) {
			return fmt.Errorf("skill %q: unsafe source path %q", entry.Name, entry.Source)
		}
		sourceDir := filepath.Join(repoRoot, filepath.FromSlash(entry.Source))
		files, err := collectDiskFiles(sourceDir)
		if err != nil {
			return fmt.Errorf("skill %q: %w", entry.Name, err)
		}
		if len(files) == 0 {
			return fmt.Errorf("skill %q: source has no files", entry.Name)
		}
		hasSkill := false
		for _, file := range files {
			if file.Path == "SKILL.md" {
				hasSkill = true
			}
		}
		if !hasSkill {
			return fmt.Errorf("skill %q: source is missing SKILL.md", entry.Name)
		}
		skillData, err := os.ReadFile(filepath.Join(sourceDir, "SKILL.md"))
		if err != nil {
			return fmt.Errorf("skill %q: read SKILL.md: %w", entry.Name, err)
		}
		meta, err := skills.ParseMeta(skillData)
		if err != nil {
			return fmt.Errorf("skill %q: parse SKILL.md: %w", entry.Name, err)
		}
		if meta.Name != entry.Name {
			return fmt.Errorf("skill %q: frontmatter name is %q", entry.Name, meta.Name)
		}
		entry.Version = meta.Version
		entry.Files = files
		entry.SHA256 = DigestFiles(files)
	}
	for _, entry := range manifest.Skills {
		for _, reference := range entry.References {
			if !validRelativePath(reference) {
				return fmt.Errorf("skill %q: unsafe reference path %q", entry.Name, reference)
			}
			if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(reference))); err != nil {
				return fmt.Errorf("skill %q: reference %q: %w", entry.Name, reference, err)
			}
		}
	}
	return ValidateManifest(manifest)
}

func ValidateAgainstSources(repoRoot string, manifest *BundleManifest) error {
	expected := *manifest
	expected.Skills = append([]SkillEntry(nil), manifest.Skills...)
	for i := range expected.Skills {
		expected.Skills[i].Files = append([]FileEntry(nil), manifest.Skills[i].Files...)
	}
	if err := UpdateManifestHashes(repoRoot, &expected); err != nil {
		return err
	}
	if !manifestEqual(manifest, &expected) {
		return fmt.Errorf("system skill manifest hashes are stale; run go run ./cmd/system-skills --write")
	}
	return nil
}

// WriteBundle validates canonical sources, updates the checked-in lock, and
// materializes a deterministic bundle at outputDir.
func WriteBundle(repoRoot, manifestPath, outputDir string) error {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var manifest BundleManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parse system skills manifest: %w", err)
	}
	if err := UpdateManifestHashes(repoRoot, &manifest); err != nil {
		return err
	}
	manifestData, err := json.MarshalIndent(&manifest, "", "  ")
	if err != nil {
		return err
	}
	manifestData = append(manifestData, '\n')
	if err := atomicWriteFile(manifestPath, manifestData, 0o644); err != nil {
		return err
	}

	parent := filepath.Dir(outputDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".system-skills-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	for _, entry := range manifest.Skills {
		src := filepath.Join(repoRoot, filepath.FromSlash(entry.Source))
		dst := filepath.Join(stage, entry.Destination)
		if err := copyTree(src, dst); err != nil {
			return fmt.Errorf("copy skill %q: %w", entry.Name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(stage, ManifestFilename), manifestData, 0o644); err != nil {
		return err
	}
	if err := ValidateBundle(os.DirFS(stage), &manifest); err != nil {
		return err
	}
	return replaceDir(stage, outputDir)
}

func CheckBundle(repoRoot, manifestPath, bundleDir string) error {
	manifest, _, err := LoadManifestFile(manifestPath)
	if err != nil {
		return err
	}
	if err := ValidateAgainstSources(repoRoot, manifest); err != nil {
		return err
	}
	bundleManifest, bundleData, err := ReadManifestFS(os.DirFS(bundleDir))
	if err != nil {
		return err
	}
	if err := ValidateBundle(os.DirFS(bundleDir), bundleManifest); err != nil {
		return err
	}
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	if string(bundleData) != string(manifestData) {
		return fmt.Errorf("generated system manifest differs from %s; run go run ./cmd/system-skills --write", manifestPath)
	}
	return nil
}

func DigestFiles(files []FileEntry) string {
	ordered := append([]FileEntry(nil), files...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	h := sha256.New()
	h.Write([]byte(MarkerSalt))
	h.Write([]byte{0})
	for _, file := range ordered {
		h.Write([]byte(file.Path))
		h.Write([]byte{0})
		h.Write([]byte(file.SHA256))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func collectDiskFiles(root string) ([]FileEntry, error) {
	var files []FileEntry
	err := filepath.WalkDir(root, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink rejected: %s", name)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular file rejected: %s", name)
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !validRelativePath(rel) {
			return fmt.Errorf("unsafe skill file path %q", rel)
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		files = append(files, FileEntry{Path: rel, SHA256: hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink rejected: %s", name)
		}
		rel, err := filepath.Rel(src, name)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular file rejected: %s", name)
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

func replaceDir(stage, dst string) error {
	backup := dst + ".previous"
	_ = os.RemoveAll(backup)
	if err := os.Rename(dst, backup); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(stage, dst); err != nil {
		_ = os.Rename(backup, dst)
		return err
	}
	_ = os.RemoveAll(backup)
	return nil
}

func atomicWriteFile(filename string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(filename)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	stage, err := os.CreateTemp(dir, "."+filepath.Base(filename)+".tmp-*")
	if err != nil {
		return err
	}
	name := stage.Name()
	defer os.Remove(name)
	if err := stage.Chmod(mode); err != nil {
		stage.Close()
		return err
	}
	if _, err := stage.Write(data); err != nil {
		stage.Close()
		return err
	}
	if err := stage.Sync(); err != nil {
		stage.Close()
		return err
	}
	if err := stage.Close(); err != nil {
		return err
	}
	return os.Rename(name, filename)
}

func validName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (r == '-' && i > 0) {
			continue
		}
		return false
	}
	return true
}

func validSourcePath(name string) bool {
	return validRelativePath(name) && (name == "skills" || strings.HasPrefix(name, "skills/"))
}

func validRelativePath(name string) bool {
	if name == "" || strings.Contains(name, "\\") || path.IsAbs(name) {
		return false
	}
	clean := path.Clean(name)
	return clean == name && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func manifestEqual(a, b *BundleManifest) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

// SyncBundle copies a generated bundle into a directory owned by another host
// (for example AgentKit's SwiftPM resources). The destination is replaced as a
// unit so stale skills cannot survive a bundle downgrade.
func SyncBundle(bundleDir, destination string) error {
	manifest, _, err := ReadManifestFS(os.DirFS(bundleDir))
	if err != nil {
		return err
	}
	if err := ValidateBundle(os.DirFS(bundleDir), manifest); err != nil {
		return err
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".system-skills-sync-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := copyTree(bundleDir, stage); err != nil {
		return err
	}
	if err := ValidateBundle(os.DirFS(stage), manifest); err != nil {
		return err
	}
	return replaceDir(stage, destination)
}
