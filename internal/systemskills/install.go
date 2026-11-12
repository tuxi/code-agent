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
)

// Install verifies the bundle, updates .system when needed, then removes known
// bundle-owned flattened legacy skills. Modified legacy skills are preserved and
// reported because they shadow the system copy.
func Install(bundle fs.FS, skillsRoot, source string) error {
	report, err := InstallWithReport(bundle, skillsRoot, source)
	if err != nil {
		return err
	}
	for _, migration := range report.Legacy {
		if migration.Warning != "" {
			fmt.Fprintf(os.Stderr, "skills: warning: %s\n", migration.Warning)
		}
	}
	return nil
}

type InstallReport struct {
	Installed bool
	Legacy    []LegacyMigration
}

type LegacyMigration struct {
	Name    string
	State   string
	Warning string
}

const (
	LegacyMigrated  = "migrated"
	LegacyPreserved = "preserved"
)

// InstallWithReport exposes migration outcomes for tests and host diagnostics.
func InstallWithReport(bundle fs.FS, skillsRoot, source string) (InstallReport, error) {
	var report InstallReport
	if strings.TrimSpace(skillsRoot) == "" {
		return report, fmt.Errorf("skills root is required")
	}
	manifest, manifestData, err := ReadManifestFS(bundle)
	if err != nil {
		return report, err
	}
	legacyManifest, legacyData, err := ReadLegacyManifestFS(bundle)
	if err != nil {
		return report, err
	}
	if err := ValidateBundle(bundle, manifest); err != nil {
		return report, err
	}
	if source == "" {
		source = "embedded"
	}
	fingerprint := bundleFingerprint(manifestData, legacyData)
	destination := filepath.Join(skillsRoot, ".system")

	if err := os.MkdirAll(skillsRoot, 0o755); err != nil {
		return report, fmt.Errorf("create skills root: %w", err)
	}
	if err := recoverInterruptedInstall(skillsRoot); err != nil {
		return report, err
	}
	if info, err := os.Lstat(destination); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return report, fmt.Errorf("system skills destination is a symlink: %s", destination)
	} else if err != nil && !os.IsNotExist(err) {
		return report, fmt.Errorf("inspect system skills destination: %w", err)
	}

	marker := Marker{
		Schema:        manifest.Schema,
		BundleVersion: manifest.BundleVersion,
		Fingerprint:   fingerprint,
		Source:        source,
		Files:         markerFiles(manifest, manifestData, legacyData),
	}
	if !markerMatches(filepath.Join(destination, MarkerFilename), marker, destination, manifest) {
		stage, err := os.MkdirTemp(skillsRoot, ".system.stage-")
		if err != nil {
			return report, fmt.Errorf("create system skill staging directory: %w", err)
		}
		defer os.RemoveAll(stage)
		if err := copyFS(bundle, stage); err != nil {
			return report, fmt.Errorf("stage system skills: %w", err)
		}
		markerData, err := json.MarshalIndent(marker, "", "  ")
		if err != nil {
			return report, err
		}
		markerData = append(markerData, '\n')
		if err := os.WriteFile(filepath.Join(stage, MarkerFilename), markerData, 0o644); err != nil {
			return report, fmt.Errorf("write system skills marker: %w", err)
		}
		if err := ValidateBundle(os.DirFS(stage), manifest); err != nil {
			return report, fmt.Errorf("validate staged system skills: %w", err)
		}

		backup := destination + ".backup"
		_ = os.RemoveAll(backup)
		if err := os.Rename(destination, backup); err != nil && !os.IsNotExist(err) {
			return report, fmt.Errorf("backup existing system skills: %w", err)
		}
		if err := os.Rename(stage, destination); err != nil {
			_ = os.Rename(backup, destination)
			return report, fmt.Errorf("activate system skills: %w", err)
		}
		if err := os.RemoveAll(backup); err != nil {
			return report, fmt.Errorf("remove system skills backup: %w", err)
		}
		report.Installed = true
	}

	report.Legacy, err = migrateLegacySkills(skillsRoot, legacyManifest)
	if err != nil {
		return report, err
	}
	return report, nil
}

// InstallForHome is the codeagentd/CLI entry point for ~/.codeagent/skills.
func InstallForHome(home string, bundle fs.FS, source string) error {
	if strings.TrimSpace(home) == "" {
		return fmt.Errorf("home directory is required")
	}
	return Install(bundle, filepath.Join(home, ".codeagent", "skills"), source)
}

// InstallForDataDir is the embedded Runtime/AgentKit entry point for
// <data-directory>/skills.
func InstallForDataDir(dataDir string, bundle fs.FS, source string) error {
	if strings.TrimSpace(dataDir) == "" {
		return fmt.Errorf("embedded data directory is required")
	}
	return Install(bundle, filepath.Join(dataDir, "skills"), source)
}

func markerMatches(filename string, expected Marker, destination string, manifest *BundleManifest) bool {
	data, err := os.ReadFile(filename)
	if err != nil {
		return false
	}
	var actual Marker
	if err := json.Unmarshal(data, &actual); err != nil {
		return false
	}
	if actual.Schema != expected.Schema || actual.BundleVersion != expected.BundleVersion ||
		actual.Fingerprint != expected.Fingerprint ||
		!sameFileEntries(actual.Files, expected.Files) {
		return false
	}
	for _, file := range expected.Files {
		disk, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(file.Path)))
		if err != nil {
			return false
		}
		sum := sha256.Sum256(disk)
		if hex.EncodeToString(sum[:]) != file.SHA256 {
			return false
		}
	}
	return ValidateBundle(os.DirFS(destination), manifest) == nil
}

func migrateLegacySkills(skillsRoot string, manifest *LegacySkillManifest) ([]LegacyMigration, error) {
	var report []LegacyMigration
	for _, skill := range manifest.Skills {
		legacyPath := filepath.Join(skillsRoot, skill.Name)
		info, err := os.Lstat(legacyPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return report, fmt.Errorf("inspect legacy skill %q: %w", skill.Name, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			report = append(report, preservedLegacy(skill.Name, "not a regular directory"))
			continue
		}
		files, err := readLegacyDirectory(legacyPath)
		if err != nil {
			report = append(report, preservedLegacy(skill.Name, err.Error()))
			continue
		}
		if !matchesLegacyVariant(files, skill.Variants) {
			report = append(report, preservedLegacy(skill.Name, "content differs from known bundle-owned versions"))
			continue
		}
		if err := os.RemoveAll(legacyPath); err != nil {
			return report, fmt.Errorf("remove bundle-owned legacy skill %q: %w", skill.Name, err)
		}
		report = append(report, LegacyMigration{Name: skill.Name, State: LegacyMigrated})
	}
	return report, nil
}

func preservedLegacy(name, reason string) LegacyMigration {
	return LegacyMigration{
		Name:    name,
		State:   LegacyPreserved,
		Warning: fmt.Sprintf("legacy skill %q preserved: %s; it shadows system skill %q", name, reason, name),
	}
}

func readLegacyDirectory(root string) ([]FileEntry, error) {
	var files []FileEntry
	err := filepath.WalkDir(root, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("contains a symlink")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("contains a non-regular file")
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		files = append(files, FileEntry{Path: filepath.ToSlash(rel), SHA256: hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func matchesLegacyVariant(files []FileEntry, variants []LegacyVariant) bool {
	for _, variant := range variants {
		if DigestFiles(files) != variant.SHA256 || len(files) != len(variant.Files) {
			continue
		}
		expected := make(map[string]string, len(variant.Files))
		for _, file := range variant.Files {
			expected[file.Path] = file.SHA256
		}
		matched := true
		for _, file := range files {
			if expected[file.Path] != file.SHA256 {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func recoverInterruptedInstall(skillsRoot string) error {
	destination := filepath.Join(skillsRoot, ".system")
	backup := destination + ".backup"
	if _, err := os.Lstat(destination); os.IsNotExist(err) {
		if info, backupErr := os.Lstat(backup); backupErr == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			if err := os.Rename(backup, destination); err != nil {
				return fmt.Errorf("restore interrupted system skills install: %w", err)
			}
		}
	}
	entries, err := os.ReadDir(skillsRoot)
	if err != nil {
		return fmt.Errorf("scan skills root for interrupted install: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), ".system.stage-") {
			_ = os.RemoveAll(filepath.Join(skillsRoot, entry.Name()))
		}
	}
	return nil
}

func copyFS(source fs.FS, destination string) error {
	return fs.WalkDir(source, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("symlink rejected: %s", name)
		}
		target := filepath.Join(destination, filepath.FromSlash(name))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(source, name)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

func installedFiles(manifest *BundleManifest) []FileEntry {
	var files []FileEntry
	for _, entry := range manifest.Skills {
		for _, file := range entry.Files {
			files = append(files, FileEntry{
				Path:   path.Join(entry.Destination, file.Path),
				SHA256: file.SHA256,
			})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

func markerFiles(manifest *BundleManifest, manifestData, legacyData []byte) []FileEntry {
	files := installedFiles(manifest)
	for _, item := range []struct {
		name string
		data []byte
	}{{ManifestFilename, manifestData}, {LegacyManifestFilename, legacyData}} {
		sum := sha256.Sum256(item.data)
		files = append(files, FileEntry{Path: item.name, SHA256: hex.EncodeToString(sum[:])})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

func sameFileEntries(a, b []FileEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func bundleFingerprint(manifestData, legacyManifestData []byte) string {
	h := sha256.New()
	h.Write([]byte(MarkerSalt))
	h.Write([]byte{0})
	h.Write(manifestData)
	h.Write([]byte{0})
	h.Write(legacyManifestData)
	return hex.EncodeToString(h.Sum(nil))
}
