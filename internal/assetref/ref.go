package assets

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"mime"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

type Range struct {
	StartLine   int `json:"start_line,omitempty"`
	StartColumn int `json:"start_column,omitempty"`
	EndLine     int `json:"end_line,omitempty"`
	EndColumn   int `json:"end_column,omitempty"`
}

type Ref struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`

	URI         string `json:"uri,omitempty"`
	DisplayName string `json:"display_name,omitempty"`

	WorkspaceID           string `json:"workspace_id,omitempty"`
	WorkspaceRelativePath string `json:"workspace_relative_path,omitempty"`
	AbsolutePath          string `json:"absolute_path,omitempty"`

	Range    *Range            `json:"range,omitempty"`
	Preview  string            `json:"preview,omitempty"`
	MIMEType string            `json:"mime_type,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`

	SourceTurnID string `json:"source_turn_id,omitempty"`
	SourceCallID string `json:"source_call_id,omitempty"`
}

type TextAnnotation struct {
	AssetID string `json:"asset_id"`
	Kind    string `json:"kind,omitempty"`
	Text    string `json:"text"`

	StartByte  int `json:"start_byte"`
	EndByte    int `json:"end_byte"`
	StartUTF16 int `json:"start_utf16"`
	EndUTF16   int `json:"end_utf16"`

	SourceTurnID string `json:"source_turn_id,omitempty"`
	SourceCallID string `json:"source_call_id,omitempty"`
}

func StableID(turnID, callID string, ordinal int, parts ...string) string {
	h := sha1.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	sum := hex.EncodeToString(h.Sum(nil))[:8]
	return fmt.Sprintf("asset_%s_%s_%03d_%s", safeID(turnID), safeID(callID), ordinal, sum)
}

func WorkspaceID(root string) string {
	name := filepath.Base(filepath.Clean(root))
	if name == "." || name == string(filepath.Separator) || name == "" {
		name = "workspace"
	}
	return safeID(strings.ToLower(name)) + "-local"
}

func WorkspaceURI(workspaceID, rel string, r *Range) string {
	u := "workspace://" + workspaceID + "/" + encodePath(filepath.ToSlash(rel))
	if r != nil && r.StartLine > 0 {
		u += fmt.Sprintf("#L%d", r.StartLine)
		if r.StartColumn > 0 {
			u += fmt.Sprintf("C%d", r.StartColumn)
		}
	}
	return u
}

func DisplayName(path string, line int) string {
	name := filepath.Base(filepath.ToSlash(path))
	if line > 0 {
		return fmt.Sprintf("%s:%d", name, line)
	}
	return name
}

func MIMEType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "text/x-go"
	case ".swift":
		return "text/x-swift"
	case ".rs":
		return "text/rust"
	case ".py":
		return "text/x-python"
	case ".ts":
		return "text/typescript"
	case ".tsx":
		return "text/tsx"
	case ".js":
		return "text/javascript"
	case ".jsx":
		return "text/jsx"
	case ".md":
		return "text/markdown"
	}
	if mt := mime.TypeByExtension(filepath.Ext(path)); mt != "" {
		return mt
	}
	return "text/plain"
}

var unsafeID = regexp.MustCompile(`[^a-zA-Z0-9_=-]+`)

func safeID(s string) string {
	s = unsafeID.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return "unknown"
	}
	return s
}

func encodePath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
