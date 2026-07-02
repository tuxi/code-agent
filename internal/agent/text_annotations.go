package agent

import (
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"code-agent/internal/assetref"
)

type textAnnotationCandidate struct {
	text        string
	ref         assets.Ref
	lineMention bool
	priority    int
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
	if len(candidates) == 0 && len(lineMentionRefs(refs)) == 0 {
		return nil
	}
	matches := findAnnotationMatches(text, candidates)
	matches = append(matches, tableLineMentionMatches(text, refs)...)
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].candidate.priority != matches[j].candidate.priority {
			return matches[i].candidate.priority > matches[j].candidate.priority
		}
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
	for _, m := range lineMentionMatches(text, refs, selected) {
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

func findAnnotationMatches(text string, candidates []textAnnotationCandidate) []textAnnotationMatch {
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
			if candidateBoundary(c, text, start, end) {
				matches = append(matches, textAnnotationMatch{candidate: c, start: start, end: end})
			}
			offset = end
		}
	}
	return matches
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
		case "file", "file_location", "directory":
			rel := filepath.ToSlash(ref.WorkspaceRelativePath)
			if rel != "" {
				add(ref, rel)
			}
			if ref.Kind == "directory" && rel != "" {
				add(ref, strings.TrimRight(rel, "/")+"/")
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

func lineMentionMatches(text string, refs []assets.Ref, anchors []textAnnotationMatch) []textAnnotationMatch {
	lineRefs := lineMentionRefs(refs)
	if len(lineRefs) == 0 {
		return nil
	}
	uniqueFiles := uniqueLineMentionFiles(lineRefs)
	allowGlobal := len(uniqueFiles) == 1
	candidates := make([]textAnnotationCandidate, 0, len(lineRefs)*6)
	for _, ref := range lineRefs {
		line := ref.Range.StartLine
		for _, mention := range lineMentionTexts(line) {
			candidates = append(candidates, textAnnotationCandidate{text: mention, ref: ref, lineMention: true})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if len(candidates[i].text) != len(candidates[j].text) {
			return len(candidates[i].text) > len(candidates[j].text)
		}
		return candidates[i].text < candidates[j].text
	})
	matches := findAnnotationMatches(text, candidates)
	out := make([]textAnnotationMatch, 0, len(matches))
	for _, m := range matches {
		if allowGlobal || nearbySameFileAnchor(m, anchors) {
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		li := out[i].end - out[i].start
		lj := out[j].end - out[j].start
		if li != lj {
			return li > lj
		}
		if out[i].start != out[j].start {
			return out[i].start < out[j].start
		}
		return out[i].candidate.ref.ID < out[j].candidate.ref.ID
	})
	return out
}

func lineMentionRefs(refs []assets.Ref) []assets.Ref {
	seen := make(map[string]bool)
	var out []assets.Ref
	for _, ref := range refs {
		if ref.Kind != "file_location" || ref.ID == "" || ref.Range == nil || ref.Range.StartLine <= 0 {
			continue
		}
		file := canonicalAnnotationFile(ref)
		if file == "" {
			continue
		}
		key := file + "\x00" + itoa(ref.Range.StartLine)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ref)
	}
	return out
}

func uniqueLineMentionFiles(refs []assets.Ref) map[string]bool {
	out := make(map[string]bool)
	for _, ref := range refs {
		if file := canonicalAnnotationFile(ref); file != "" {
			out[file] = true
		}
	}
	return out
}

func canonicalAnnotationFile(ref assets.Ref) string {
	if ref.WorkspaceRelativePath != "" {
		return filepath.ToSlash(ref.WorkspaceRelativePath)
	}
	if ref.AbsolutePath != "" {
		return filepath.ToSlash(ref.AbsolutePath)
	}
	if ref.URI != "" && strings.HasPrefix(ref.URI, "workspace://") {
		return ref.URI
	}
	return ""
}

func lineMentionTexts(line int) []string {
	n := itoa(line)
	return []string{
		"第 " + n + " 行",
		"第" + n + "行",
		"line " + n,
		"Line " + n,
		"L" + n,
		n + " 行",
	}
}

func nearbySameFileAnchor(m textAnnotationMatch, anchors []textAnnotationMatch) bool {
	file := canonicalAnnotationFile(m.candidate.ref)
	if file == "" {
		return false
	}
	for _, anchor := range anchors {
		if canonicalAnnotationFile(anchor.candidate.ref) != file {
			continue
		}
		if byteDistance(m, anchor) <= 500 {
			return true
		}
	}
	return false
}

func byteDistance(a, b textAnnotationMatch) int {
	if a.end <= b.start {
		return b.start - a.end
	}
	if b.end <= a.start {
		return a.start - b.end
	}
	return 0
}

type annotationLine struct {
	text  string
	start int
}

type annotationTableCell struct {
	text  string
	start int
	end   int
}

type annotationTableLineNumber struct {
	line  int
	start int
	end   int
}

func tableLineMentionMatches(text string, refs []assets.Ref) []textAnnotationMatch {
	lineRefs := lineMentionRefs(refs)
	if len(lineRefs) == 0 {
		return nil
	}
	byFileLine := lineRefsByFileAndLine(lineRefs)
	uniqueFiles := uniqueLineMentionFiles(lineRefs)
	lines := annotationLines(text)
	var out []textAnnotationMatch
	for i := 0; i < len(lines); i++ {
		headerCells := parseMarkdownTableCells(lines[i])
		if len(headerCells) == 0 {
			continue
		}
		lineCol, fileCol := tableLineAndFileColumns(headerCells)
		if lineCol < 0 {
			continue
		}
		rowStart := i + 1
		if rowStart < len(lines) && isMarkdownTableSeparator(lines[rowStart]) {
			rowStart++
		}
		currentFile := ""
		for j := rowStart; j < len(lines); j++ {
			rowCells := parseMarkdownTableCells(lines[j])
			if len(rowCells) == 0 {
				break
			}
			if lineCol >= len(rowCells) {
				continue
			}
			file := ""
			if fileCol >= 0 && fileCol < len(rowCells) {
				cellFile := markdownCellPlainText(rowCells[fileCol].text)
				if sameFileMarker(cellFile) {
					file = currentFile
				} else {
					file = resolveAnnotationFile(cellFile, lineRefs)
					if file != "" {
						currentFile = file
					}
				}
			}
			if file == "" && len(uniqueFiles) == 1 {
				for f := range uniqueFiles {
					file = f
				}
			}
			if file == "" {
				continue
			}
			lineNumbers := tableLineCellNumbers(rowCells[lineCol])
			if len(lineNumbers) == 0 {
				continue
			}
			fileAnnotated := false
			for _, lineNumber := range lineNumbers {
				if ref, ok := byFileLine[file][lineNumber.line]; ok {
					if !fileAnnotated && fileCol >= 0 && fileCol < len(rowCells) && !sameFileMarker(markdownCellPlainText(rowCells[fileCol].text)) {
						if fileText, fileStart, fileEnd, ok := markdownCellContentRange(rowCells[fileCol]); ok {
							if resolveAnnotationFile(fileText, []assets.Ref{ref}) != "" {
								out = append(out, textAnnotationMatch{
									candidate: textAnnotationCandidate{text: text[fileStart:fileEnd], ref: ref, priority: 10},
									start:     fileStart,
									end:       fileEnd,
								})
								fileAnnotated = true
							}
						}
					}
					out = append(out, textAnnotationMatch{
						candidate: textAnnotationCandidate{text: text[lineNumber.start:lineNumber.end], ref: ref, lineMention: true, priority: 10},
						start:     lineNumber.start,
						end:       lineNumber.end,
					})
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].start != out[j].start {
			return out[i].start < out[j].start
		}
		return out[i].end < out[j].end
	})
	return out
}

func annotationLines(text string) []annotationLine {
	var out []annotationLine
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			out = append(out, annotationLine{text: text[start:i], start: start})
			start = i + 1
		}
	}
	out = append(out, annotationLine{text: text[start:], start: start})
	return out
}

func parseMarkdownTableCells(line annotationLine) []annotationTableCell {
	if !strings.Contains(line.text, "|") {
		return nil
	}
	var pipes []int
	for i := 0; i < len(line.text); i++ {
		if line.text[i] == '|' {
			pipes = append(pipes, i)
		}
	}
	if len(pipes) == 0 {
		return nil
	}
	if pipes[0] != 0 {
		pipes = append([]int{-1}, pipes...)
	}
	if pipes[len(pipes)-1] != len(line.text)-1 {
		pipes = append(pipes, len(line.text))
	}
	if len(pipes) < 2 {
		return nil
	}
	cells := make([]annotationTableCell, 0, len(pipes)-1)
	for i := 0; i+1 < len(pipes); i++ {
		start := pipes[i] + 1
		end := pipes[i+1]
		cells = append(cells, annotationTableCell{
			text:  line.text[start:end],
			start: line.start + start,
			end:   line.start + end,
		})
	}
	return cells
}

func isMarkdownTableSeparator(line annotationLine) bool {
	cells := parseMarkdownTableCells(line)
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		s := strings.TrimSpace(c.text)
		if s == "" {
			return false
		}
		for _, r := range s {
			if r != '-' && r != ':' {
				return false
			}
		}
	}
	return true
}

func tableLineAndFileColumns(headerCells []annotationTableCell) (int, int) {
	lineCol := -1
	fileCol := -1
	for i, c := range headerCells {
		h := strings.ToLower(strings.ReplaceAll(markdownCellPlainText(c.text), " ", ""))
		switch {
		case lineCol < 0 && (h == "行" || strings.Contains(h, "行号") || h == "line" || h == "lines" || strings.Contains(h, "lineno") || strings.Contains(h, "linenumber")):
			lineCol = i
		case fileCol < 0 && (strings.Contains(h, "文件") || strings.Contains(h, "file") || strings.Contains(h, "path")):
			fileCol = i
		}
	}
	return lineCol, fileCol
}

func markdownCellPlainText(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`")
	s = strings.Trim(s, "*_")
	return strings.TrimSpace(s)
}

func markdownCellContentRange(cell annotationTableCell) (string, int, int, bool) {
	raw := cell.text
	left := 0
	right := len(raw)
	for left < right && unicode.IsSpace(rune(raw[left])) {
		left++
	}
	for right > left && unicode.IsSpace(rune(raw[right-1])) {
		right--
	}
	for left < right && (raw[left] == '`' || raw[left] == '*' || raw[left] == '_') {
		left++
	}
	for right > left && (raw[right-1] == '`' || raw[right-1] == '*' || raw[right-1] == '_') {
		right--
	}
	for left < right && unicode.IsSpace(rune(raw[left])) {
		left++
	}
	for right > left && unicode.IsSpace(rune(raw[right-1])) {
		right--
	}
	if left >= right {
		return "", 0, 0, false
	}
	start := cell.start + left
	end := cell.start + right
	return raw[left:right], start, end, true
}

func tableLineCellNumbers(cell annotationTableCell) []annotationTableLineNumber {
	raw := cell.text
	var out []annotationTableLineNumber
	n := 0
	start := -1
	for i := 0; i <= len(raw); i++ {
		var b byte
		if i < len(raw) {
			b = raw[i]
		}
		if i < len(raw) && b >= '0' && b <= '9' {
			if start < 0 {
				start = i
			}
			n = n*10 + int(b-'0')
			continue
		}
		if start >= 0 {
			out = append(out, annotationTableLineNumber{
				line:  n,
				start: cell.start + start,
				end:   cell.start + i,
			})
			start = -1
			n = 0
		}
		if i < len(raw) && !isTableLineNumberSeparator(rune(b)) {
			return nil
		}
	}
	return out
}

func isTableLineNumberSeparator(r rune) bool {
	switch r {
	case 0, ',', '，', ';', '；', '/', '、', '&', '+':
		return true
	}
	return unicode.IsSpace(r)
}

func tableLineCellNumber(cell annotationTableCell) (int, int, int, bool) {
	numbers := tableLineCellNumbers(cell)
	if len(numbers) != 1 {
		return 0, 0, 0, false
	}
	n := numbers[0]
	return n.line, n.start, n.end, true
}

func lineRefsByFileAndLine(refs []assets.Ref) map[string]map[int]assets.Ref {
	out := make(map[string]map[int]assets.Ref)
	for _, ref := range refs {
		if ref.Range == nil || ref.Range.StartLine <= 0 {
			continue
		}
		file := canonicalAnnotationFile(ref)
		if file == "" {
			continue
		}
		if out[file] == nil {
			out[file] = make(map[int]assets.Ref)
		}
		if _, exists := out[file][ref.Range.StartLine]; !exists {
			out[file][ref.Range.StartLine] = ref
		}
	}
	return out
}

func resolveAnnotationFile(cellText string, refs []assets.Ref) string {
	cellText = filepath.ToSlash(markdownCellPlainText(cellText))
	if cellText == "" {
		return ""
	}
	var exact string
	var baseMatches []string
	for _, ref := range refs {
		file := canonicalAnnotationFile(ref)
		if file == "" {
			continue
		}
		if strings.Contains(cellText, file) {
			if exact == "" || len(file) > len(exact) {
				exact = file
			}
			continue
		}
		base := filepath.Base(file)
		if base != "" && strings.Contains(cellText, base) {
			baseMatches = append(baseMatches, file)
		}
	}
	if exact != "" {
		return exact
	}
	if len(baseMatches) == 1 {
		return baseMatches[0]
	}
	return ""
}

func sameFileMarker(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "同上" || s == "same" || s == "same file" || s == "same as above" || s == "ditto"
}

func annotationBoundary(text string, start, end int) bool {
	return boundaryBefore(text, start) && boundaryAfter(text, end)
}

func candidateBoundary(c textAnnotationCandidate, text string, start, end int) bool {
	if c.lineMention {
		return boundaryBefore(text, start) && lineMentionBoundaryAfter(text, end)
	}
	return annotationBoundary(text, start, end)
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
		if len(text[idx:]) == 1 {
			return true
		}
		next, ok := firstRuneAfter(text[idx+1:])
		if ok && unicode.IsSpace(next) {
			return true
		}
	}
	return !isAssetTokenRune(r)
}

func lineMentionBoundaryAfter(text string, idx int) bool {
	if idx >= len(text) {
		return true
	}
	r, _ := firstRuneAfter(text[idx:])
	if unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' || r == '/' || r == '\\' {
		return false
	}
	return true
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
