package tui

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"code-agent/internal/session"
)

type renamedMsg struct {
	dialog   *renameDialog
	id, name string
	seq      int64
	err      error
}

type renameDialog struct {
	id     string
	input  textinput.Model
	rename func(id, name string) (int64, error)
	saving bool
	err    string
}

func (m *model) openRename(value string) tea.Cmd {
	if m.src.rename == nil {
		return m.system("Renaming is unavailable")
	}
	if m.renameDialog != nil && m.renameDialog.saving {
		return nil
	}
	if value == "" {
		value = m.sessionName
	}
	input := textinput.New()
	input.SetWidth(min(48, max(10, m.width-12)))
	input.SetValue(value)
	d := &renameDialog{id: m.sessionID, input: input, rename: m.src.rename}
	m.renameDialog, m.dialog = d, d
	m.blocking = false
	return d.Init()
}

func (d *renameDialog) Init() tea.Cmd { return d.input.Focus() }

func (d *renameDialog) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if d.saving {
		return d, nil
	}
	if key, ok := msg.(tea.KeyPressMsg); ok && key.String() == "enter" {
		name := strings.TrimSpace(d.input.Value())
		switch {
		case name == "":
			d.err = "Enter a conversation name"
		case len(name) > session.MaxManualNameBytes:
			d.err = "Name must be at most 200 bytes"
		default:
			d.saving, d.err = true, ""
			return d, func() tea.Msg {
				seq, err := d.rename(d.id, name)
				return renamedMsg{dialog: d, id: d.id, name: name, seq: seq, err: err}
			}
		}
		return d, nil
	}
	input, cmd := d.input.Update(msg)
	d.input = input
	return d, cmd
}

func (d *renameDialog) View() tea.View {
	footer := "Enter save · Esc cancel"
	if d.saving {
		footer = "Saving…"
	} else if d.err != "" {
		footer = d.err + "\n" + footer
	}
	return tea.NewView(lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(1, 2).
		Render("Rename conversation\n\n" + d.input.View() + "\n\n" + footer))
}

func (m *model) applyName(id, name string, seq int64) {
	if id != m.sessionID || seq > 0 && seq < m.nameSeq {
		return
	}
	m.sessionName = name
	m.nameSeq = max(m.nameSeq, seq)
	m.refreshTitle()
}

func (m *model) refreshTitle() {
	title := m.sessionName
	if title == "" {
		title = m.sessionPreview
	}
	if title == "" {
		title = "新对话"
	}
	m.status.SetTitle(title)
}
