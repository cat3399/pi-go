package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/cat3399/pi-go/internal/llm"
)

type composerModel struct {
	input           textarea.Model
	theme           Theme
	width           int
	maxInputHeight  int
	busy            bool
	images          []llm.ImageBlock
	history         []string
	historyIndex    int
	historyDraft    string
	historyDraftRow int
	historyDraftCol int
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
	return composerModel{input: input, theme: theme, width: 80, maxInputHeight: 8, historyIndex: -1}
}

func (m *composerModel) Init() tea.Cmd {
	if m == nil {
		return nil
	}
	return m.input.Focus()
}

func (m *composerModel) Focus() tea.Cmd {
	if m == nil {
		return nil
	}
	return m.input.Focus()
}

func (m *composerModel) Blur() {
	if m != nil {
		m.input.Blur()
	}
}

func (m *composerModel) Update(message tea.Msg) tea.Cmd {
	if m == nil {
		return nil
	}
	before := m.input.Value()
	updated, command := m.input.Update(message)
	m.input = updated
	if m.historyIndex >= 0 && m.input.Value() != before {
		m.exitHistory()
	}
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
	m.maxInputHeight = max(1, min(8, height))
	m.applyMaxHeight()
}

func (m *composerModel) applyMaxHeight() {
	height := m.maxInputHeight
	if len(m.images) != 0 {
		height = max(1, height-1)
	}
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

func (m *composerModel) CursorOffset() int {
	if m == nil {
		return 0
	}
	lines := strings.Split(m.input.Value(), "\n")
	row := max(0, min(m.input.Line(), len(lines)-1))
	offset := 0
	for index := 0; index < row; index++ {
		offset += len([]rune(lines[index])) + 1
	}
	offset += min(max(0, m.input.Column()), len([]rune(lines[row])))
	return offset
}

func (m *composerModel) ReplaceRuneRange(start, end int, replacement string, cursorOffset int) {
	if m == nil {
		return
	}
	value := []rune(m.input.Value())
	start = max(0, min(start, len(value)))
	end = max(start, min(end, len(value)))
	replacementRunes := []rune(replacement)
	next := make([]rune, 0, len(value)-(end-start)+len(replacementRunes))
	next = append(next, value[:start]...)
	next = append(next, replacementRunes...)
	next = append(next, value[end:]...)
	cursorOffset = max(0, min(start+cursorOffset, len(next)))
	m.input.SetValue(string(next))
	m.input.MoveToBegin()
	prefix := next[:cursorOffset]
	row, column := 0, 0
	for _, char := range prefix {
		if char == '\n' {
			row++
			column = 0
		} else {
			column++
		}
	}
	for range row {
		m.input.CursorDown()
	}
	m.input.SetCursorColumn(column)
	m.exitHistory()
}

func (m *composerModel) Reset() {
	if m != nil {
		m.input.Reset()
		m.images = nil
		m.exitHistory()
		m.applyMaxHeight()
	}
}

func (m *composerModel) SetDraft(value string, images []llm.ImageBlock) {
	if m == nil {
		return
	}
	m.input.SetValue(value)
	m.images = append([]llm.ImageBlock(nil), images...)
	m.exitHistory()
	m.applyMaxHeight()
}

func (m *composerModel) Images() []llm.ImageBlock {
	if m == nil {
		return nil
	}
	return append([]llm.ImageBlock(nil), m.images...)
}

func (m *composerModel) Empty() bool {
	return m == nil || (strings.TrimSpace(m.input.Value()) == "" && len(m.images) == 0)
}

func (m *composerModel) HasImages() bool { return m != nil && len(m.images) != 0 }

func (m *composerModel) RestoreDraftIfEmpty(value string, images []llm.ImageBlock) {
	if m == nil || (strings.TrimSpace(value) == "" && len(images) == 0) || !m.Empty() {
		return
	}
	m.SetDraft(value, images)
}

func (m *composerModel) AddToHistory(value string) {
	if m == nil {
		return
	}
	value = strings.TrimSpace(value)
	if value == "" || (len(m.history) != 0 && m.history[0] == value) {
		return
	}
	m.history = append([]string{value}, m.history...)
	if len(m.history) > 100 {
		m.history = m.history[:100]
	}
}

func (m *composerModel) NavigateHistory(keyName string) bool {
	if m == nil || len(m.history) == 0 || len(m.images) != 0 {
		return false
	}
	lineInfo := m.input.LineInfo()
	switch keyName {
	case "up":
		firstVisualLine := m.input.Line() == 0 && lineInfo.RowOffset == 0
		if !firstVisualLine {
			return false
		}
		if strings.TrimSpace(m.input.Value()) != "" && m.historyIndex < 0 && m.input.Column() != 0 {
			m.input.CursorStart()
			return true
		}
		if m.historyIndex+1 >= len(m.history) {
			return true
		}
		if m.historyIndex < 0 {
			m.historyDraft = m.input.Value()
			m.historyDraftRow = m.input.Line()
			m.historyDraftCol = m.input.Column()
		}
		m.historyIndex++
		m.setHistoryValue(m.history[m.historyIndex], true)
		return true
	case "down":
		lastVisualLine := m.input.Line() == m.input.LineCount()-1 && lineInfo.RowOffset == lineInfo.Height-1
		if !lastVisualLine {
			return false
		}
		if m.historyIndex < 0 {
			m.input.CursorEnd()
			return true
		}
		m.historyIndex--
		if m.historyIndex >= 0 {
			m.setHistoryValue(m.history[m.historyIndex], false)
			return true
		}
		draft, row, column := m.historyDraft, m.historyDraftRow, m.historyDraftCol
		m.exitHistory()
		m.input.SetValue(draft)
		m.input.MoveToBegin()
		for range row {
			m.input.CursorDown()
		}
		m.input.SetCursorColumn(column)
		return true
	default:
		return false
	}
}

func (m *composerModel) setHistoryValue(value string, cursorAtStart bool) {
	m.input.SetValue(value)
	if cursorAtStart {
		m.input.MoveToBegin()
	} else {
		m.input.MoveToEnd()
	}
}

func (m *composerModel) exitHistory() {
	m.historyIndex = -1
	m.historyDraft = ""
	m.historyDraftRow = 0
	m.historyDraftCol = 0
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
	content := m.input.View()
	if len(m.images) != 0 {
		label := fmt.Sprintf("▧ %d image", len(m.images))
		if len(m.images) != 1 {
			label += "s"
		}
		content = m.theme.mutedStyle().Render(label) + "\n" + content
	}
	return m.frameStyle().Render(content)
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
	if len(m.images) != 0 {
		cursor.Position.Y++
	}
	return cursor
}
