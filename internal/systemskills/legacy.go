package systemskills

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
)

const (
	LegacySchemaV1         = "codeagent.legacy-system-skills.v1"
	LegacyManifestFilename = "legacy-system-skills.json"
)

// LegacySkillManifest describes flattened bundles shipped before .system.
// Every variant is an exact directory snapshot; extra files make the directory
// user-owned and therefore ineligible for automatic removal.
type LegacySkillManifest struct {
	Schema        string             `json:"schema"`
	BundleVersion string             `json:"bundle_version"`
	Skills        []LegacySkillEntry `json:"skills"`
}

type LegacySkillEntry struct {
	Name     string          `json:"name"`
	Variants []LegacyVariant `json:"variants"`
}

type LegacyVariant struct {
	ID     string      `json:"id"`
	SHA256 string      `json:"sha256"`
	Files  []FileEntry `json:"files"`
}

func LoadLegacyManifest(data []byte) (*LegacySkillManifest, error) {
	var manifest LegacySkillManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse legacy system skills manifest: %w", err)
	}
	if err := ValidateLegacyManifest(&manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func ReadLegacyManifestFS(fsys fs.FS) (*LegacySkillManifest, []byte, error) {
	data, err := fs.ReadFile(fsys, LegacyManifestFilename)
	if err != nil {
		return nil, nil, fmt.Errorf("read legacy system skills manifest: %w", err)
	}
	manifest, err := LoadLegacyManifest(data)
	if err != nil {
		return nil, nil, err
	}
	return manifest, data, nil
}

func ValidateLegacyManifest(manifest *LegacySkillManifest) error {
	if manifest.Schema != LegacySchemaV1 {
		return fmt.Errorf("unsupported legacy system skills schema %q", manifest.Schema)
	}
	if strings.TrimSpace(manifest.BundleVersion) == "" {
		return fmt.Errorf("legacy system skills bundle_version is required")
	}
	if len(manifest.Skills) == 0 {
		return fmt.Errorf("legacy system skills manifest has no skills")
	}
	names := make(map[string]struct{}, len(manifest.Skills))
	for _, skill := range manifest.Skills {
		if !validName(skill.Name) {
			return fmt.Errorf("legacy skill name %q is invalid", skill.Name)
		}
		if _, exists := names[skill.Name]; exists {
			return fmt.Errorf("duplicate legacy skill %q", skill.Name)
		}
		names[skill.Name] = struct{}{}
		if len(skill.Variants) == 0 {
			return fmt.Errorf("legacy skill %q has no variants", skill.Name)
		}
		for _, variant := range skill.Variants {
			if strings.TrimSpace(variant.ID) == "" || !validSHA256(variant.SHA256) {
				return fmt.Errorf("legacy skill %q has an invalid variant", skill.Name)
			}
			if len(variant.Files) == 0 || DigestFiles(variant.Files) != variant.SHA256 {
				return fmt.Errorf("legacy skill %q variant %q hash is invalid", skill.Name, variant.ID)
			}
			paths := make(map[string]struct{}, len(variant.Files))
			for _, file := range variant.Files {
				if !validRelativePath(file.Path) || !validSHA256(file.SHA256) {
					return fmt.Errorf("legacy skill %q variant %q has an invalid file", skill.Name, variant.ID)
				}
				if _, exists := paths[file.Path]; exists {
					return fmt.Errorf("legacy skill %q variant %q has duplicate file %q", skill.Name, variant.ID, file.Path)
				}
				paths[file.Path] = struct{}{}
			}
			if _, ok := paths["SKILL.md"]; !ok {
				return fmt.Errorf("legacy skill %q variant %q is missing SKILL.md", skill.Name, variant.ID)
			}
		}
	}
	return nil
}
