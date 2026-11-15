package agent

import (
	"path/filepath"
	"strings"

	"code-agent/internal/assetref"
	"code-agent/internal/tools"
)

// Written files reuse the existing session-scoped asset reader. No path is
// opened here and out-of-workspace reports never become new asset authority.
func outputWriteAssets(refs []assets.Ref, writes []tools.FileWrite, root, turnID, callID string) []assets.Ref {
	out := append([]assets.Ref(nil), refs...)
	if root == "" {
		return out
	}
	for _, write := range writes {
		path := filepath.ToSlash(filepath.Clean(write.Path))
		if path == "." || filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, "../") || write.Status == tools.WriteDeleted {
			continue
		}
		found := false
		for _, ref := range out {
			if ref.WorkspaceRelativePath == path {
				found = true
				break
			}
		}
		if found {
			continue
		}
		mime := assets.MIMEType(path)
		out = append(out, assets.Ref{ID: assets.StableID(turnID, callID, len(out), path, "written_output"), Kind: assets.KindForMIME(mime), WorkspaceID: assets.WorkspaceID(root), WorkspaceRelativePath: path, DisplayName: filepath.Base(path), MIMEType: mime, SourceTurnID: turnID, SourceCallID: callID})
	}
	return out
}
