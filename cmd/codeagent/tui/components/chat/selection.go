package chat

import (
	"strings"

	chAnsi "github.com/charmbracelet/x/ansi"
)

// cellPos addresses one cell of the rendered transcript in content space:
// row is the line index (0 = first line, independent of viewport scrolling),
// col is the cell column. Columns are cell-based, so a wide (CJK) rune
// occupies two columns. Rendered lines are padded to the list width, so a
// valid col always lies in [0, width).
type cellPos struct {
	row, col int
}

// screenToContent maps a terminal coordinate onto a content-space cell.
// It reports false when the point is outside the transcript's rectangle or
// beyond the rendered content.
func (m *List) screenToContent(x, y int) (cellPos, bool) {
	lx := x - m.screenX
	ly := y - m.screenY
	if lx < 0 || ly < 0 || lx >= m.width {
		return cellPos{}, false
	}
	row := ly + m.viewport.YOffset()
	if row < 0 || row >= len(m.lineTable) {
		return cellPos{}, false
	}
	col := lx
	if w := cellWidth(m.lineTable[row]); col >= w {
		col = w - 1
	}
	return cellPos{row: row, col: max(col, 0)}, true
}

// selRange returns the normalized (start, end) of the active selection, so the
// range always runs top-to-bottom, left-to-right.
func (m *List) selRange() (cellPos, cellPos) {
	if m.selStart.row < m.selCur.row ||
		(m.selStart.row == m.selCur.row && m.selStart.col <= m.selCur.col) {
		return m.selStart, m.selCur
	}
	return m.selCur, m.selStart
}

// setContent stores the freshly rendered content, builds the plain-text line
// table used for selection and copy, installs the content into the viewport,
// and re-applies the selection highlight when a drag is in progress.
func (m *List) setContent(content string) {
	m.contentLines = strings.Split(content, "\n")
	m.lineTable = make([]string, len(m.contentLines))
	for i, ln := range m.contentLines {
		m.lineTable[i] = chAnsi.Strip(ln)
	}
	m.viewport.SetContent(content)
	if m.selecting {
		m.applySelection()
	}
}

// applySelection overlays reverse-video on the cells covered by the active
// selection. Rendered lines are exactly one cell row each (the list pads every
// line to its width), so a selection range maps one-to-one onto content rows.
func (m *List) applySelection() {
	if !m.selecting || len(m.contentLines) == 0 {
		return
	}
	start, end := m.selRange()
	lines := make([]string, len(m.contentLines))
	for r, ln := range m.contentLines {
		if r < start.row || r > end.row {
			lines[r] = ln
			continue
		}
		plain := m.lineTable[r]
		c1, c2 := 0, cellWidth(plain)
		if r == start.row {
			c1 = start.col
		}
		if r == end.row {
			c2 = end.col + 1
		}
		if c1 > c2 {
			c1, c2 = c2, c1
		}
		lines[r] = sliceCells(plain, 0, c1) + "\x1b[7m" +
			sliceCells(plain, c1, c2) + "\x1b[27m" +
			sliceCells(plain, c2, cellWidth(plain))
	}
	m.viewport.SetContent(strings.Join(lines, "\n"))
}

// copySelection writes the selected text to the clipboard through copyText
// (tests override it to avoid touching the system clipboard).
func (m *List) copySelection() {
	start, end := m.selRange()
	var sb strings.Builder
	for r := start.row; r <= end.row; r++ {
		if r < 0 || r >= len(m.lineTable) {
			continue
		}
		line := m.lineTable[r]
		c1, c2 := 0, cellWidth(line)
		if r == start.row {
			c1 = start.col
		}
		if r == end.row {
			c2 = end.col + 1
		}
		if c1 > c2 {
			c1, c2 = c2, c1
		}
		seg := strings.TrimRight(sliceCells(line, c1, c2), " ")
		seg = stripBorder(seg)
		sb.WriteString(seg)
		if r != end.row {
			sb.WriteString("\n")
		}
	}
	text := sb.String()
	if text == "" || m.copyText == nil {
		return
	}
	_ = m.copyText(text)
}

// stripBorder removes the left border glyph (and its padding space) that
// bordered blocks — messages, thinking, tool cards — render at the start of
// every line, so copied text is the content, not the chrome.
func stripBorder(s string) string {
	rs := []rune(s)
	if len(rs) == 0 || !borderGlyphs[rs[0]] {
		return s
	}
	if len(rs) > 1 && rs[1] == ' ' {
		return string(rs[2:])
	}
	return string(rs[1:])
}

var borderGlyphs = map[rune]bool{
	'┃': true, '│': true, '╎': true, '╏': true,
	'┊': true, '┋': true, '┆': true, '┇': true,
	'▏': true, '▎': true, '▍': true, '▌': true, '▋': true, '▊': true, '▉': true,
}

// cellWidth returns the display width of a plain line in cells.
func cellWidth(s string) int {
	return chAnsi.StringWidth(s)
}

// runeCellWidth returns the display width of one rune in cells.
func runeCellWidth(r rune) int {
	return chAnsi.StringWidth(string(r))
}

// cellToRune returns the rune index whose start cell column is >= cell — the
// slice position that covers the given cell. A boundary inside a wide rune
// snaps to the rune's start.
func cellToRune(s string, cell int) int {
	col, ri := 0, 0
	for _, r := range s {
		w := runeCellWidth(r)
		if col+w > cell {
			return ri
		}
		col += w
		if col >= cell {
			return ri + 1
		}
		ri++
	}
	return len([]rune(s))
}

// sliceCells returns the substring of s covering the cell range [from, to).
func sliceCells(s string, from, to int) string {
	if from > to {
		from, to = to, from
	}
	rs := []rune(s)
	a, b := cellToRune(s, from), cellToRune(s, to)
	if a < 0 {
		a = 0
	}
	if b > len(rs) {
		b = len(rs)
	}
	if a > b {
		a = b
	}
	return string(rs[a:b])
}
