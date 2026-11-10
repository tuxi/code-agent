// Package r4input loads and validates file-backed Preview inputs for the R4
// acceptance commands. It is not part of the production server or agent loop.
package r4input

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"code-agent/internal/preview"
	"code-agent/internal/workspace"
)

type Options struct {
	ViewerHTMLFile       string
	FallbackFile         string
	Title                string
	Summary              string
	ArtifactKey          string
	PreferredAspectRatio float64
	MinHeight            int
}

type Defaults struct {
	ViewerHTML           string
	FallbackPath         string
	Title                string
	Summary              string
	ArtifactKey          string
	FallbackText         string
	Alt                  string
	PreferredAspectRatio float64
	MinHeight            int
}

type Prepared struct {
	ViewerHTML           string
	ViewerSHA256         string
	FallbackPath         string
	FallbackSHA256       string
	Title                string
	Summary              string
	ArtifactKey          string
	FallbackText         string
	Alt                  string
	PreferredAspectRatio float64
	MinHeight            int
}

func Prepare(opts Options, defaults Defaults, workspaceRoot string) (Prepared, error) {
	if strings.TrimSpace(workspaceRoot) == "" {
		return Prepared{}, fmt.Errorf("workspace root is required")
	}
	viewer, err := loadViewerHTML(opts.ViewerHTMLFile, defaults.ViewerHTML)
	if err != nil {
		return Prepared{}, err
	}
	fallbackPath, fallbackDigest, err := prepareFallback(workspaceRoot, opts.FallbackFile, defaults.FallbackPath)
	if err != nil {
		return Prepared{}, err
	}
	prepared := Prepared{
		ViewerHTML:           viewer,
		ViewerSHA256:         digestBytes([]byte(viewer)),
		FallbackPath:         fallbackPath,
		FallbackSHA256:       fallbackDigest,
		Title:                defaultString(opts.Title, defaults.Title),
		Summary:              defaultString(opts.Summary, defaults.Summary),
		ArtifactKey:          defaultString(opts.ArtifactKey, defaults.ArtifactKey),
		FallbackText:         defaults.FallbackText,
		Alt:                  defaults.Alt,
		PreferredAspectRatio: defaults.PreferredAspectRatio,
		MinHeight:            defaults.MinHeight,
	}
	if opts.PreferredAspectRatio > 0 {
		prepared.PreferredAspectRatio = opts.PreferredAspectRatio
	}
	if opts.MinHeight > 0 {
		prepared.MinHeight = opts.MinHeight
	}
	return prepared, nil
}

func loadViewerHTML(path, fallback string) (string, error) {
	data := []byte(fallback)
	if strings.TrimSpace(path) != "" {
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read viewer HTML: %w", err)
		}
	}
	if len(data) == 0 {
		return "", fmt.Errorf("viewer HTML is empty")
	}
	if len(data) > preview.MaxViewerBytes {
		return "", fmt.Errorf("viewer HTML exceeds the 2 MiB limit")
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("viewer HTML is not valid UTF-8")
	}
	return string(data), nil
}

func prepareFallback(workspaceRoot, source, defaultPath string) (string, string, error) {
	if strings.TrimSpace(source) == "" {
		source = defaultPath
	}
	if strings.TrimSpace(source) == "" {
		return "", "", fmt.Errorf("fallback image is required")
	}
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "", "", err
	}
	sourceAbs := source
	if !filepath.IsAbs(sourceAbs) {
		sourceAbs = filepath.Join(root, sourceAbs)
	}
	sourceAbs, err = filepath.Abs(sourceAbs)
	if err != nil {
		return "", "", err
	}
	class := workspace.ClassifyPath(root, sourceAbs)
	if class == workspace.PathManagedWorktree {
		return "", "", fmt.Errorf("fallback image is inside the managed worktree boundary")
	}
	if class == workspace.PathInsideWorkspace {
		data, err := os.ReadFile(sourceAbs)
		if err != nil {
			return "", "", fmt.Errorf("read fallback image: %w", err)
		}
		if _, err := validateImageBytes(data); err != nil {
			return "", "", err
		}
		rel, err := filepath.Rel(root, sourceAbs)
		if err != nil {
			return "", "", err
		}
		return filepath.ToSlash(rel), digestBytes(data), nil
	}

	data, err := os.ReadFile(sourceAbs)
	if err != nil {
		return "", "", fmt.Errorf("read fallback image: %w", err)
	}
	mimeType, err := validateImageBytes(data)
	if err != nil {
		return "", "", err
	}
	digest := digestBytes(data)
	extension := imageExtension(mimeType)
	targetDir := filepath.Join(root, ".codeagent", "assets", "client", "r4-input")
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		return "", "", fmt.Errorf("create controlled fallback directory: %w", err)
	}
	target := filepath.Join(targetDir, strings.TrimPrefix(digest, "sha256:")+extension)
	if err := writeImmutableCopy(target, data); err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return "", "", err
	}
	return filepath.ToSlash(rel), digest, nil
}

func validateImageBytes(data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("fallback image is empty")
	}
	mimeType := strings.Split(http.DetectContentType(data), ";")[0]
	switch mimeType {
	case "image/png", "image/jpeg":
		if _, _, err := image.DecodeConfig(bytes.NewReader(data)); err != nil {
			return "", fmt.Errorf("fallback image cannot be decoded: %w", err)
		}
	case "image/webp":
		if err := validateWebP(data); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("fallback image must be PNG, JPEG, or WebP")
	}
	return mimeType, nil
}

// validateWebP verifies the RIFF container and the dimensions/signature of a
// lossy, lossless, or extended WebP image chunk without adding a new dependency.
func validateWebP(data []byte) error {
	if len(data) < 20 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return fmt.Errorf("fallback WebP container is invalid")
	}
	riffSize := int(binary.LittleEndian.Uint32(data[4:8])) + 8
	if riffSize > len(data) {
		return fmt.Errorf("fallback WebP is truncated")
	}
	foundImage := false
	for offset := 12; offset+8 <= riffSize; {
		chunk := string(data[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		payloadStart := offset + 8
		payloadEnd := payloadStart + size
		if payloadEnd > riffSize {
			return fmt.Errorf("fallback WebP chunk is truncated")
		}
		payload := data[payloadStart:payloadEnd]
		switch chunk {
		case "VP8 ":
			if len(payload) >= 10 && payload[3] == 0x9d && payload[4] == 0x01 && payload[5] == 0x2a {
				foundImage = true
			}
		case "VP8L":
			if len(payload) >= 5 && payload[0] == 0x2f {
				foundImage = true
			}
		case "VP8X":
			if len(payload) >= 10 {
				foundImage = true
			}
		}
		offset = payloadEnd
		if size%2 == 1 {
			offset++
		}
	}
	if !foundImage {
		return fmt.Errorf("fallback WebP has no decodable image chunk")
	}
	return nil
}

func writeImmutableCopy(path string, data []byte) error {
	if existing, err := os.ReadFile(path); err == nil {
		if bytes.Equal(existing, data) {
			return nil
		}
		return fmt.Errorf("controlled fallback path already contains different bytes")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".r4-input-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func imageExtension(mimeType string) string {
	switch mimeType {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	default:
		return ".img"
	}
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}
