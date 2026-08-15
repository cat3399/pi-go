package tui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type composerModel struct {
	input textarea.Model
	theme Theme
	width int
	busy  bool
}

func newComposerModel(theme Theme) composerModel {
	input := textarea.New()
	input.Prompt = "› "
	input.Placeholder = "Ask pi-go anything"
	input.ShowLineNumbers = false
	input.DynamicHeight = true
	input.MinHeight = 1
	input.MaxHeight = 8
	input.MaxContentHeight = 10_000
	input.SetVirtualCursor(false)
	input.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("shift+enter", "ctrl+j"),
		key.WithHelp("shift+enter", "new line"),
	)

	styles := textarea.DefaultDarkStyles()
	styles.Focused.Base = lipgloss.NewStyle().Foreground(theme.color(theme.Foreground))
	styles.Focused.Text = lipgloss.NewStyle().Foreground(theme.color(theme.Foreground))
	styles.Focused.Prompt = lipgloss.NewStyle().Bold(true).Foreground(theme.color(theme.Primary))
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(theme.color(theme.Subtle))
	styles.Focused.CursorLine = lipgloss.NewStyle()
	styles.Blurred = styles.Focused
	styles.Cursor.Color = theme.color(theme.Primary)
	styles.Cursor.Shape = tea.CursorBar
	styles.Cursor.Blink = true
	input.SetStyles(styles)
	input.SetWidth(76)
	input.SetHeight(1)
	return composerModel{input: input, theme: theme, width: 80}
}

func (m *composerModel) Init() tea.Cmd {
	if m == nil {
		return nil
	}
	return m.input.Focus()
}

func (m *composerModel) Update(message tea.Msg) tea.Cmd {
	if m == nil {
		return nil
	}
	updated, command := m.input.Update(message)
	m.input = updated
	return command
}

func (m *composerModel) SetWidth(width int) {
	if m == nil {
		return
	}
	m.width = max(1, width)
	// Border (2) and horizontal padding (2) belong to the frame.
	m.input.SetWidth(max(1, m.width-4))
}

func (m *composerModel) SetMaxHeight(height int) {
	if m == nil {
		return
	}
	height = max(1, min(8, height))
	m.input.MaxHeight = height
	if m.input.Height() > height {
		m.input.SetHeight(height)
	}
}

func (m *composerModel) SetBusy(busy bool) {
	if m == nil {
		return
	}
	m.busy = busy
	if busy {
		m.input.Placeholder = "Steer the current run"
	} else {
		m.input.Placeholder = "Ask pi-go anything"
	}
}

func (m *composerModel) Value() string {
	if m == nil {
		return ""
	}
	return m.input.Value()
}

func (m *composerModel) Reset() {
	if m != nil {
		m.input.Reset()
	}
}

func (m *composerModel) RestoreIfEmpty(value string) {
	if m == nil || strings.TrimSpace(value) == "" || strings.TrimSpace(m.input.Value()) != "" {
		return
	}
	m.input.SetValue(value)
}

func (m *composerModel) frameStyle() lipgloss.Style {
	border := m.theme.Border
	if m.busy {
		border = m.theme.Warning
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.theme.color(border)).
		Padding(0, 1)
}

func (m *composerModel) View() string {
	if m == nil {
		return ""
	}
	return m.frameStyle().Render(m.input.View())
}

func (m *composerModel) Height() int {
	if m == nil {
		return 0
	}
	return lipgloss.Height(m.View())
}

func (m *composerModel) Cursor() *tea.Cursor {
	if m == nil {
		return nil
	}
	cursor := m.input.Cursor()
	if cursor == nil {
		return nil
	}
	style := m.frameStyle()
	cursor.Position.X += style.GetBorderLeftSize() + style.GetPaddingLeft()
	cursor.Position.Y += style.GetBorderTopSize() + style.GetPaddingTop()
	return cursor
}
