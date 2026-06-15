package workspace

import (
	"path/filepath"
	"strings"
)

func IsSubPath(rootAbs, targetAbs string) bool {
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && rel != "..")
}

func ShouldSkipName(name string) bool {

	switch name {
	case ".git", "node_modules", "vendor", ".idea", ".vscode", ".DS_Store", "dist", "build", ".next":
		return true
	default:
		return false
	}

}

func PathDepth(rel string) int {

	if rel == "." || rel == "" {
		return 0
	}
	return strings.Count(filepath.ToSlash(rel), "/") + 1

}
