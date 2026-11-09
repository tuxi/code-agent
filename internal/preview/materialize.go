package preview

import (
	"fmt"
	"html"
	"strings"
	"unicode/utf8"
)

const ViewerContentSecurityPolicy = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; connect-src 'none'; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'"

const viewerDocument = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <meta http-equiv="Content-Security-Policy" content="%s">
  <title>%s</title>
  <style>
    :root { color-scheme: light dark; font: 14px -apple-system, sans-serif; }
    body { margin: 0; padding: 16px; background: Canvas; color: CanvasText; }
  </style>
</head>
<body>
  <main data-preview-root>%s</main>
</body>
</html>
`

// MaterializeViewer wraps a model-authored fragment in the runtime-owned HTML
// shell. The fragment is never served as a top-level document.
func MaterializeViewer(title, fragment string) ([]byte, *Error) {
	if !utf8.ValidString(fragment) || !utf8.ValidString(title) {
		return nil, NewError(CodeInvalidArtifact, "Preview viewer is not valid UTF-8.", false)
	}
	if err := rejectUnsafeViewerFragment(fragment); err != nil {
		return nil, err
	}
	document := fmt.Sprintf(viewerDocument, ViewerContentSecurityPolicy, html.EscapeString(title), fragment)
	data := []byte(document)
	if len(data) > MaxViewerBytes {
		return nil, NewError(CodeViewerTooLarge, "Viewer exceeds the 2 MiB limit.", false)
	}
	return data, nil
}

func ValidateViewerBytes(data []byte, wantSize int64, wantDigest string) *Error {
	if int64(len(data)) > MaxViewerBytes || wantSize > MaxViewerBytes {
		return NewError(CodeViewerTooLarge, "Viewer exceeds the 2 MiB limit.", false)
	}
	if wantSize != 0 && int64(len(data)) != wantSize {
		return NewError(CodeDigestMismatch, "Viewer size did not match.", false)
	}
	if !utf8.Valid(data) {
		return NewError(CodeInvalidArtifact, "Viewer is not valid UTF-8.", false)
	}
	if wantDigest != "" && !DigestMatches(wantDigest, data) {
		return NewError(CodeDigestMismatch, "Viewer digest did not match.", false)
	}
	return nil
}

func rejectUnsafeViewerFragment(fragment string) *Error {
	lower := strings.ToLower(fragment)
	blocked := []string{
		"<!doctype", "<html", "<head", "<body", "<iframe", "<object", "<embed", "<form", "<base",
		"href=", "http://", "https://", "file://", "ftp://", "ws://", "wss://",
		"window.location", "document.location", "location.href", "location.assign", "location.replace",
		"window.open", "top.location", "parent.location", "window.parent", "window.top",
		"fetch(", "xmlhttprequest", "websocket(", "eventsource(", "sendbeacon",
		"window.webkit", "messagehandlers", "postmessage",
	}
	for _, token := range blocked {
		if strings.Contains(lower, token) {
			return NewError(CodeSecurityViolation, "Preview viewer contains a forbidden navigation, network, or bridge primitive.", false)
		}
	}
	for offset := 0; ; {
		index := strings.Index(lower[offset:], "src=")
		if index < 0 {
			break
		}
		start := offset + index + len("src=")
		rest := strings.TrimSpace(lower[start:])
		if !strings.HasPrefix(rest, `"data:`) && !strings.HasPrefix(rest, `'data:`) && !strings.HasPrefix(rest, "data:") {
			return NewError(CodeSecurityViolation, "Preview viewer may only load self-contained data resources.", false)
		}
		offset = start
	}
	return nil
}
