package collab

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// WorkspaceAnchor excludes HEAD and directory timestamps, which change during
// ordinary coding. File identities detect replacement of a checkout at a path.
type WorkspaceAnchor struct {
	Root         string `json:"root"`
	RootIdentity string `json:"root_identity"`
	GitDir       string `json:"git_dir"`
	GitIdentity  string `json:"git_identity"`
}

func AnchorWorkspace(path string) (WorkspaceAnchor, error) {
	var anchor WorkspaceAnchor
	if !filepath.IsAbs(path) {
		return anchor, ErrInvalid
	}
	root, err := filepath.EvalSymlinks(path)
	if err != nil {
		return anchor, err
	}
	root = filepath.Clean(root)
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return anchor, ErrInvalid
	}
	rootID, err := fileIdentity(info)
	if err != nil {
		return anchor, err
	}
	gitDir := filepath.Join(root, ".git")
	gitInfo, err := os.Stat(gitDir)
	if err != nil {
		return anchor, err
	}
	if !gitInfo.IsDir() {
		if gitInfo.Size() > 4096 {
			return anchor, ErrInvalid
		}
		b, err := os.ReadFile(gitDir)
		if err != nil {
			return anchor, err
		}
		line := strings.TrimSpace(string(b))
		if !strings.HasPrefix(line, "gitdir: ") || strings.ContainsAny(line, "\x00\r\n") {
			return anchor, ErrInvalid
		}
		gitDir = strings.TrimPrefix(line, "gitdir: ")
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(root, gitDir)
		}
	}
	gitDir, err = filepath.EvalSymlinks(gitDir)
	if err != nil {
		return anchor, err
	}
	gitInfo, err = os.Stat(gitDir)
	if err != nil || !gitInfo.IsDir() {
		return anchor, ErrInvalid
	}
	head, err := os.Stat(filepath.Join(gitDir, "HEAD"))
	if err != nil || !head.Mode().IsRegular() || head.Size() == 0 || head.Size() > 4096 {
		return anchor, ErrInvalid
	}
	gitID, err := fileIdentity(gitInfo)
	if err != nil {
		return anchor, err
	}
	return WorkspaceAnchor{Root: root, RootIdentity: rootID, GitDir: filepath.Clean(gitDir), GitIdentity: gitID}, nil
}

func (a WorkspaceAnchor) Check(path string) error {
	b, err := AnchorWorkspace(path)
	if err != nil || a != b {
		return errors.Join(ErrManagedWorkspace, err)
	}
	return nil
}
