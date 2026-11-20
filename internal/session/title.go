package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"code-agent/internal/model"
)

var ErrSessionNotFound = errors.New("session not found")

// NameStore protects an automatic title from a concurrent manual rename.
type NameStore interface {
	UpdateConversationName(ctx context.Context, id, name string, onlyIfEmpty bool, event EventRecord) (seq int64, changed bool, err error)
}

const MaxManualNameBytes = 200

// TitleInput extracts readable text without passing a whole log to the model.
func TitleInput(text string) string {
	var first, last, diagnostic string
	fence := ""
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			if fence == "" {
				fence = line[:3]
			} else if strings.HasPrefix(line, fence) {
				fence = ""
			}
			continue
		}
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		logLine := fence != "" || strings.HasPrefix(lower, "at ") ||
			strings.HasPrefix(lower, "traceback") ||
			(len(line) > 10 && line[4] == '-' && line[7] == '-') ||
			strings.HasPrefix(lower, "[info]") || strings.HasPrefix(lower, "[error]") ||
			strings.HasPrefix(lower, "[warn]") || strings.HasPrefix(lower, "[debug]")
		if logLine {
			if diagnostic == "" && (strings.Contains(lower, "error") ||
				strings.Contains(lower, "failed") || strings.Contains(lower, "exception")) {
				diagnostic = compactTitleText(line, 160)
			}
			continue
		}
		if first == "" {
			first = compactTitleText(line, 360)
		}
		last = compactTitleText(line, 360)
	}
	lines := []string{first}
	if last != first {
		lines = append(lines, last)
	}
	if diagnostic != "" {
		lines = append(lines, diagnostic)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func compactTitleText(text string, limit int) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	text = strings.Join(strings.Fields(text), " ")
	for i := range text {
		if limit == 0 {
			return text[:i]
		}
		limit--
	}
	return text
}

func MessagePreview(text string, assets []model.GatewayAssetRef, localAssets []model.LocalAssetRef) string {
	input := TitleInput(text)
	if input != "" {
		first, _, _ := strings.Cut(input, "\n")
		// The first prose line normally contains the request. When it begins
		// with a log, TitleInput puts the trailing prose ahead of diagnostics.
		return compactTitleText(first, 80)
	}
	if strings.TrimSpace(text) != "" {
		return "日志排查"
	}
	attachments := len(assets) + len(localAssets)
	if attachments == 0 {
		return ""
	}
	images := 0
	for _, asset := range assets {
		if asset.Kind == "image" || strings.HasPrefix(asset.MIMEType, "image/") {
			images++
		}
	}
	for _, asset := range localAssets {
		if asset.Kind == "image" || strings.HasPrefix(asset.MIMEType, "image/") {
			images++
		}
	}
	if attachments == images {
		if images == 1 {
			return "图片附件"
		}
		return fmt.Sprintf("%d 张图片", images)
	}
	if attachments == 1 {
		return "附件"
	}
	return fmt.Sprintf("%d 个附件", attachments)
}

func (m Meta) ConversationPreview() string {
	if m.Preview != "" {
		return m.Preview
	}
	return MessagePreview(m.Title, nil, nil)
}

func (s *Session) ConversationPreview() string {
	if s.Preview != "" {
		return s.Preview
	}
	for _, msg := range s.Messages {
		if msg.Role == model.RoleUser && !msg.RuntimeInput {
			return MessagePreview(msg.Content, msg.Assets, msg.LocalAssets)
		}
	}
	return ""
}
