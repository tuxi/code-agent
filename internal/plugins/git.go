package plugins

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// cloneRepo clones a Git repository into pluginsDir and returns the path to the
// cloned directory. If the repo is already cloned, it runs `git pull` to update it.
func cloneRepo(repoURL, pluginsDir string) (string, error) {
	// Derive a directory name from the repo URL: the last component without .git.
	name := repoName(repoURL)
	dst := filepath.Join(pluginsDir, name)

	if _, err := os.Stat(filepath.Join(dst, ".git")); err == nil {
		// Already cloned — pull to update.
		cmd := exec.Command("git", "-C", dst, "pull", "--ff-only")
		cmd.Stderr = os.Stderr
		if out, err := cmd.Output(); err != nil {
			return "", fmt.Errorf("git pull in %s: %w\n%s", dst, err, string(out))
		}
		return dst, nil
	}

	// Fresh clone.
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		return "", fmt.Errorf("create plugins dir: %w", err)
	}
	cmd := exec.Command("git", "clone", repoURL, dst)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git clone %s: %w", repoURL, err)
	}
	return dst, nil
}

// repoName derives a short directory name from a Git URL.
//
//	https://github.com/anthropics/skills.git → skills
//	git@github.com:anthropics/skills.git      → skills
//	https://github.com/anthropics/skills       → skills
func repoName(url string) string {
	// Strip trailing .git.
	name := strings.TrimSuffix(url, ".git")
	// Take the last path component.
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	// Handle git@github.com:user/repo format.
	if i := strings.LastIndexByte(name, ':'); i >= 0 {
		name = name[i+1:]
	}
	return name
}
