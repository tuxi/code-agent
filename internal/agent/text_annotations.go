package agent

import (
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"code-agent/internal/assetref"
)

type textAnnotationCandidate struct {
	text string
	ref  assets.Ref
}

type textAnnotationMatch struct {
	candidate textAnnotationCandidate
	start     int
	end       int
}

func annotateTextWithAssets(text string, refs []assets.Ref) []assets.TextAnnotation {
	if text == "" || len(refs) == 0 {
		return nil
	}
	candidates := annotationCandidates(refs)
	if len(candidates) == 0 {
		return nil
	}
	var matches []textAnnotationMatch
	for _, c := range candidates {
		offset := 0
		for {
			idx := strings.Index(text[offset:], c.text)
			if idx < 0 {
				break
			}
			start := offset + idx
			end := start + len(c.text)
			if annotationBoundary(text, start, end) {
				matches = append(matches, textAnnotationMatch{candidate: c, start: start, end: end})
			}
			offset = end
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		li := matches[i].end - matches[i].start
		lj := matches[j].end - matches[j].start
		if li != lj {
			return li > lj
		}
		if matches[i].start != matches[j].start {
			return matches[i].start < matches[j].start
		}
		return matches[i].candidate.ref.ID < matches[j].candidate.ref.ID
	})

	selected := make([]textAnnotationMatch, 0, len(matches))
	for _, m := range matches {
		if overlapsAny(m, selected) {
			continue
		}
		selected = append(selected, m)
	}
	sort.SliceStable(selected, func(i, j int) bool {
		if selected[i].start != selected[j].start {
			return selected[i].start < selected[j].start
		}
		return selected[i].end < selected[j].end
	})

	annotations := make([]assets.TextAnnotation, 0, len(selected))
	for _, m := range selected {
		ref := m.candidate.ref
		annotations = append(annotations, assets.TextAnnotation{
			AssetID:      ref.ID,
			Kind:         ref.Kind,
			Text:         text[m.start:m.end],
			StartByte:    m.start,
			EndByte:      m.end,
			StartUTF16:   utf16Offset(text, m.start),
			EndUTF16:     utf16Offset(text, m.end),
			SourceTurnID: ref.SourceTurnID,
			SourceCallID: ref.SourceCallID,
		})
	}
	return annotations
}

func annotationCandidates(refs []assets.Ref) []textAnnotationCandidate {
	seen := make(map[string]bool)
	var out []textAnnotationCandidate
	add := func(ref assets.Ref, s string) {
		s = strings.TrimSpace(s)
		if len(s) < 4 || ref.ID == "" {
			return
		}
		key := ref.ID + "\x00" + s
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, textAnnotationCandidate{text: s, ref: ref})
	}
	for _, ref := range refs {
		switch ref.Kind {
		case "file", "file_location":
			rel := filepath.ToSlash(ref.WorkspaceRelativePath)
			if rel != "" {
				add(ref, rel)
			}
			line := 0
			if ref.Range != nil {
				line = ref.Range.StartLine
			}
			if rel != "" && line > 0 {
				add(ref, rel+":"+itoa(line))
				add(ref, filepath.Base(rel)+":"+itoa(line))
			}
			add(ref, ref.DisplayName)
		case "url":
			add(ref, ref.URI)
			add(ref, ref.DisplayName)
		default:
			// Avoid broad symbol-name annotation in prose. Symbols become clickable
			// through their tool cards today; assistant-text annotation can grow a
			// symbol-aware pass once the client wants that surface explicitly.
			if strings.ContainsAny(ref.DisplayName, "/:") {
				add(ref, ref.DisplayName)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if len(out[i].text) != len(out[j].text) {
			return len(out[i].text) > len(out[j].text)
		}
		return out[i].text < out[j].text
	})
	return out
}

func annotationBoundary(text string, start, end int) bool {
	return boundaryBefore(text, start) && boundaryAfter(text, end)
}

func boundaryBefore(text string, idx int) bool {
	if idx <= 0 {
		return true
	}
	r, _ := lastRuneBefore(text[:idx])
	return !isAssetTokenRune(r)
}

func boundaryAfter(text string, idx int) bool {
	if idx >= len(text) {
		return true
	}
	r, _ := firstRuneAfter(text[idx:])
	if r == '.' {
		prev, ok := lastRuneBefore(text[:idx])
		if ok && unicode.IsDigit(prev) {
			return true
		}
	}
	return !isAssetTokenRune(r)
}

func isAssetTokenRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' || r == '/' || r == '\\'
}

func firstRuneAfter(s string) (rune, bool) {
	for _, r := range s {
		return r, true
	}
	return 0, false
}

func lastRuneBefore(s string) (rune, bool) {
	var last rune
	ok := false
	for _, r := range s {
		last = r
		ok = true
	}
	return last, ok
}

func overlapsAny(m textAnnotationMatch, selected []textAnnotationMatch) bool {
	for _, s := range selected {
		if m.start < s.end && s.start < m.end {
			return true
		}
	}
	return false
}

func utf16Offset(s string, byteOffset int) int {
	if byteOffset <= 0 {
		return 0
	}
	if byteOffset > len(s) {
		byteOffset = len(s)
	}
	n := 0
	for _, r := range s[:byteOffset] {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
