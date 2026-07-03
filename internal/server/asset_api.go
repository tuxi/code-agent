package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"code-agent/internal/assetref"
	"code-agent/internal/conversation"
	"code-agent/internal/session"
)

const (
	assetPreviewMaxBytes = 64 * 1024
	assetContentMaxBytes = 1024 * 1024
)

type AssetPreviewResponse struct {
	Asset     assets.Ref     `json:"asset"`
	Kind      string         `json:"kind"`
	Content   string         `json:"content,omitempty"`
	MIMEType  string         `json:"mime_type,omitempty"`
	SizeBytes int64          `json:"size_bytes,omitempty"`
	Truncated bool           `json:"truncated,omitempty"`
	Source    string         `json:"source,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type AssetContentResponse struct {
	Asset     assets.Ref `json:"asset"`
	Content   string     `json:"content"`
	MIMEType  string     `json:"mime_type,omitempty"`
	SizeBytes int64      `json:"size_bytes,omitempty"`
	Truncated bool       `json:"truncated,omitempty"`
}

type assetHTTPError struct {
	status  int
	message string
}

func (e assetHTTPError) Error() string { return e.message }

func findConversationAsset(ctx context.Context, eventStore conversation.ConversationEventStore, repo conversation.ConversationRepository, conversationID, assetID string) (assets.Ref, *session.Session, error) {
	sess, err := repo.Load(ctx, conversationID)
	if err != nil {
		return assets.Ref{}, nil, assetHTTPError{status: http.StatusNotFound, message: "conversation not found"}
	}
	recs, err := eventStore.Replay(ctx, conversationID)
	if err != nil {
		return assets.Ref{}, nil, assetHTTPError{status: http.StatusInternalServerError, message: err.Error()}
	}
	for _, rec := range recs {
		ev, ok := decodeStoredEvent(rec)
		if !ok {
			continue
		}
		for _, ref := range ev.Assets {
			if ref.ID == assetID {
				return ref, sess, nil
			}
		}
	}
	return assets.Ref{}, nil, assetHTTPError{status: http.StatusNotFound, message: "asset not found"}
}

func writeAssetError(w http.ResponseWriter, err error) {
	var ae assetHTTPError
	if errors.As(err, &ae) {
		http.Error(w, ae.message, ae.status)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func previewConversationAsset(ctx context.Context, eventStore conversation.ConversationEventStore, repo conversation.ConversationRepository, conversationID, assetID string) (AssetPreviewResponse, error) {
	ref, sess, err := findConversationAsset(ctx, eventStore, repo, conversationID, assetID)
	if err != nil {
		return AssetPreviewResponse{}, err
	}
	resp := AssetPreviewResponse{
		Asset:    ref,
		Kind:     "asset_preview",
		MIMEType: ref.MIMEType,
	}
	path, pathErr := resolveAssetPath(ref, sess.WorkspacePath)
	if pathErr != nil {
		if ref.Preview != "" {
			resp.Content = ref.Preview
			resp.Source = "asset_preview"
			return resp, nil
		}
		resp.Source = "metadata"
		return resp, nil
	}
	stat, err := os.Stat(path)
	if err != nil {
		if ref.Preview != "" {
			resp.Content = ref.Preview
			resp.Source = "asset_preview"
			return resp, nil
		}
		return AssetPreviewResponse{}, assetHTTPError{status: http.StatusNotFound, message: "asset file not found"}
	}
	resp.SizeBytes = stat.Size()
	if stat.IsDir() {
		content, truncated, err := previewDirectory(path, assetPreviewMaxBytes)
		if err != nil {
			return AssetPreviewResponse{}, assetHTTPError{status: http.StatusInternalServerError, message: err.Error()}
		}
		resp.Content = content
		resp.Truncated = truncated
		resp.Source = "directory"
		return resp, nil
	}
	if resp.MIMEType == "" {
		resp.MIMEType = assets.MIMEType(path)
	}
	if !isTextAsset(resp.MIMEType) {
		resp.Metadata = assetMediaMetadata(conversationID, assetID)
		if ref.Preview != "" {
			resp.Content = ref.Preview
			resp.Source = "asset_preview"
		} else {
			resp.Source = "metadata"
		}
		return resp, nil
	}
	if ref.Range != nil && ref.Range.StartLine > 0 {
		content, truncated, err := previewLineWindow(path, ref.Range.StartLine, assetPreviewMaxBytes)
		if err == nil && content != "" {
			resp.Content = content
			resp.Truncated = truncated
			resp.Source = "file_window"
			return resp, nil
		}
	}
	content, truncated, err := readTextPrefix(path, assetPreviewMaxBytes)
	if err != nil {
		return AssetPreviewResponse{}, assetHTTPError{status: http.StatusInternalServerError, message: err.Error()}
	}
	resp.Content = content
	resp.Truncated = truncated
	resp.Source = "file"
	return resp, nil
}

func contentConversationAsset(ctx context.Context, eventStore conversation.ConversationEventStore, repo conversation.ConversationRepository, conversationID, assetID string) (AssetContentResponse, error) {
	ref, sess, err := findConversationAsset(ctx, eventStore, repo, conversationID, assetID)
	if err != nil {
		return AssetContentResponse{}, err
	}
	path, err := resolveAssetPath(ref, sess.WorkspacePath)
	if err != nil {
		return AssetContentResponse{}, err
	}
	stat, err := os.Stat(path)
	if err != nil {
		return AssetContentResponse{}, assetHTTPError{status: http.StatusNotFound, message: "asset file not found"}
	}
	if stat.IsDir() {
		return AssetContentResponse{}, assetHTTPError{status: http.StatusBadRequest, message: "asset is a directory"}
	}
	mimeType := ref.MIMEType
	if mimeType == "" {
		mimeType = assets.MIMEType(path)
	}
	if !isTextAsset(mimeType) {
		return AssetContentResponse{}, assetHTTPError{status: http.StatusUnsupportedMediaType, message: "asset content is not text"}
	}
	content, truncated, err := readTextPrefix(path, assetContentMaxBytes)
	if err != nil {
		return AssetContentResponse{}, assetHTTPError{status: http.StatusInternalServerError, message: err.Error()}
	}
	return AssetContentResponse{
		Asset:     ref,
		Content:   content,
		MIMEType:  mimeType,
		SizeBytes: stat.Size(),
		Truncated: truncated,
	}, nil
}

func serveConversationAssetBlob(ctx context.Context, w http.ResponseWriter, r *http.Request, eventStore conversation.ConversationEventStore, repo conversation.ConversationRepository, conversationID, assetID string) error {
	ref, sess, err := findConversationAsset(ctx, eventStore, repo, conversationID, assetID)
	if err != nil {
		return err
	}
	path, err := resolveAssetPath(ref, sess.WorkspacePath)
	if err != nil {
		if ae, ok := err.(assetHTTPError); ok && ae.message == "asset has no file path" {
			return assetHTTPError{status: http.StatusUnsupportedMediaType, message: "asset has no local blob"}
		}
		return err
	}
	stat, err := os.Stat(path)
	if err != nil {
		return assetHTTPError{status: http.StatusGone, message: "asset file not found"}
	}
	if stat.IsDir() {
		return assetHTTPError{status: http.StatusBadRequest, message: "asset is a directory"}
	}
	f, err := os.Open(path)
	if err != nil {
		return assetHTTPError{status: http.StatusInternalServerError, message: err.Error()}
	}
	defer f.Close()

	mimeType := ref.MIMEType
	if mimeType == "" {
		mimeType = assets.MIMEType(path)
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("ETag", assetBlobETag(stat))
	http.ServeContent(w, r, filepath.Base(path), stat.ModTime(), f)
	return nil
}

func assetMediaMetadata(conversationID, assetID string) map[string]any {
	return map[string]any{
		"media_url":     "/v1/conversations/" + conversationID + "/assets/" + assetID + "/blob",
		"thumbnail_url": "/v1/conversations/" + conversationID + "/assets/" + assetID + "/thumbnail?max_px=512",
	}
}

func assetBlobETag(stat os.FileInfo) string {
	return fmt.Sprintf(`"%x-%x"`, stat.Size(), stat.ModTime().UnixNano())
}

func resolveAssetPath(ref assets.Ref, workspaceRoot string) (string, error) {
	if workspaceRoot == "" {
		return "", assetHTTPError{status: http.StatusBadRequest, message: "conversation has no workspace root"}
	}
	root := filepath.Clean(workspaceRoot)
	var candidate string
	switch {
	case ref.WorkspaceRelativePath != "":
		if filepath.IsAbs(ref.WorkspaceRelativePath) {
			return "", assetHTTPError{status: http.StatusBadRequest, message: "workspace_relative_path must be relative"}
		}
		rel := filepath.Clean(ref.WorkspaceRelativePath)
		if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", assetHTTPError{status: http.StatusForbidden, message: "asset path escapes workspace"}
		}
		candidate = filepath.Join(root, rel)
	case ref.AbsolutePath != "":
		candidate = filepath.Clean(ref.AbsolutePath)
	default:
		return "", assetHTTPError{status: http.StatusBadRequest, message: "asset has no file path"}
	}
	if !pathInsideRoot(candidate, root) {
		return "", assetHTTPError{status: http.StatusForbidden, message: "asset path escapes workspace"}
	}
	return candidate, nil
}

func pathInsideRoot(path, root string) bool {
	cleanPath := filepath.Clean(path)
	cleanRoot := filepath.Clean(root)
	if evaluatedRoot, err := filepath.EvalSymlinks(cleanRoot); err == nil {
		cleanRoot = evaluatedRoot
	}
	if evaluatedPath, err := filepath.EvalSymlinks(cleanPath); err == nil {
		cleanPath = evaluatedPath
	}
	rel, err := filepath.Rel(cleanRoot, cleanPath)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func isTextAsset(mimeType string) bool {
	m := strings.ToLower(mimeType)
	return strings.HasPrefix(m, "text/") ||
		strings.Contains(m, "json") ||
		strings.Contains(m, "xml") ||
		strings.Contains(m, "javascript") ||
		strings.Contains(m, "typescript") ||
		strings.Contains(m, "swift") ||
		strings.Contains(m, "rust")
}

func readTextPrefix(path string, maxBytes int64) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return "", false, err
	}
	truncated := int64(len(data)) > maxBytes
	if truncated {
		data = data[:maxBytes]
	}
	return string(data), truncated, nil
}

func previewLineWindow(path string, line, maxBytes int) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	start := line - 3
	if start < 1 {
		start = 1
	}
	end := line + 3
	var b strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxBytes)
	n := 0
	truncated := false
	for sc.Scan() {
		n++
		if n < start {
			continue
		}
		if n > end {
			break
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%d: %s", n, sc.Text())
		if b.Len() >= maxBytes {
			truncated = true
			break
		}
	}
	if err := sc.Err(); err != nil {
		return "", false, err
	}
	return b.String(), truncated, nil
}

func previewDirectory(path string, maxBytes int) (string, bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", false, err
	}
	var b strings.Builder
	truncated := false
	for i, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		if b.Len()+len(name) > maxBytes {
			truncated = true
			break
		}
		b.WriteString(name)
	}
	return b.String(), truncated, nil
}
