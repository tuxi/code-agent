package agent

import (
	"path/filepath"
	"strings"
)

func ExtractPatchPaths(patch string) []string {
	seen := make(map[string]bool)
	var paths []string

	lines := strings.Split(patch, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)

		var path string

		switch {
		case strings.HasPrefix(line, "diff --git "):
			// Format: diff --git a/path b/path
			parts := strings.Fields(line)
			if len(parts) >= 4 {
				path = cleanPatchPath(parts[3])
			}

		case strings.HasPrefix(line, "+++ "):
			// Format: +++ b/path
			value := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
			if value != "/dev/null" {
				path = cleanPatchPath(value)
			}
		}

		if path == "" {
			continue
		}

		if !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}

	return paths
}

func cleanPatchPath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.TrimPrefix(path, "a/")
	path = strings.TrimPrefix(path, "b/")
	path = filepath.ToSlash(filepath.Clean(path))

	if path == "." || strings.HasPrefix(path, "../") {
		return ""
	}

	return path
}
