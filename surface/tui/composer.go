package tui

import (
	"fmt"
	"strings"
	"unicode"

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
	undo            []composerSnapshot
	killRing        []string
	lastEditAction  string
	jumpDirection   int
}

type composerSnapshot struct {
	value  string
	cursor int
	images []llm.ImageBlock
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

	applyComposerStyles(&input, theme)
	input.SetWidth(76)
	input.SetHeight(1)
	return composerModel{input: input, theme: theme, width: 80, maxInputHeight: 8, historyIndex: -1}
}

func (m *composerModel) SetTheme(theme Theme) {
	if m == nil {
		return
	}
	m.theme = theme
	applyComposerStyles(&m.input, theme)
}

func (m *composerModel) SetKeybindings(bindings appKeybindings) {
	if m == nil {
		return
	}
	set := func(binding *key.Binding, action string) { binding.SetKeys(bindings.WidgetKeys(action)...) }
	set(&m.input.KeyMap.CharacterBackward, keyEditorCursorLeft)
	set(&m.input.KeyMap.CharacterForward, keyEditorCursorRight)
	set(&m.input.KeyMap.WordBackward, keyEditorWordLeft)
	set(&m.input.KeyMap.WordForward, keyEditorWordRight)
	set(&m.input.KeyMap.LinePrevious, keyEditorCursorUp)
	set(&m.input.KeyMap.LineNext, keyEditorCursorDown)
	set(&m.input.KeyMap.LineStart, keyEditorLineStart)
	set(&m.input.KeyMap.LineEnd, keyEditorLineEnd)
	set(&m.input.KeyMap.PageUp, keyEditorPageUp)
	set(&m.input.KeyMap.PageDown, keyEditorPageDown)
	set(&m.input.KeyMap.DeleteCharacterBackward, keyEditorDeleteBackward)
	set(&m.input.KeyMap.DeleteCharacterForward, keyEditorDeleteForward)
	set(&m.input.KeyMap.DeleteWordBackward, keyEditorDeleteWordBack)
	set(&m.input.KeyMap.DeleteWordForward, keyEditorDeleteWordFront)
	set(&m.input.KeyMap.DeleteBeforeCursor, keyEditorDeleteLineStart)
	set(&m.input.KeyMap.DeleteAfterCursor, keyEditorDeleteLineEnd)
	set(&m.input.KeyMap.InsertNewline, keyInputNewLine)
}

func applyComposerStyles(input *textarea.Model, theme Theme) {
	styles := textarea.DefaultDarkStyles()
	if theme.IsLight {
		styles = textarea.DefaultLightStyles()
	}
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
	before := m.snapshot()
	updated, command := m.input.Update(message)
	m.input = updated
	if m.input.Value() != before.value {
		press, isPress := message.(tea.KeyPressMsg)
		typed := ""
		if isPress {
			typed = press.Key().Text
		}
		if typed != "" {
			if strings.IndexFunc(typed, unicode.IsSpace) >= 0 || m.lastEditAction != "type-word" {
				m.pushUndo(before)
			}
			m.lastEditAction = "type-word"
		} else {
			m.pushUndo(before)
			m.lastEditAction = ""
		}
	} else if _, isPress := message.(tea.KeyPressMsg); isPress {
		m.lastEditAction = ""
	}
	if m.historyIndex >= 0 && m.input.Value() != before.value {
		m.exitHistory()
	}
	return command
}

// HandleEditingKey implements the editor operations that Bubble's textarea
// does not provide: undo, kill/yank, yank-pop, and one-character jumps.
func (m *composerModel) HandleEditingKey(message tea.KeyPressMsg, bindings appKeybindings) bool {
	if m == nil {
		return false
	}
	if m.jumpDirection != 0 {
		if bindings.MatchesPress(keyEditorJumpForward, message) || bindings.MatchesPress(keyEditorJumpBackward, message) {
			m.jumpDirection = 0
			return true
		}
		if text := message.Key().Text; text != "" {
			target, _ := utf8FirstRune(text)
			m.jumpToRune(target, m.jumpDirection)
			m.jumpDirection = 0
			return true
		}
		m.jumpDirection = 0
	}
	switch {
	case bindings.MatchesPress(keyEditorUndo, message):
		m.undoEdit()
		return true
	case bindings.MatchesPress(keyEditorYank, message):
		m.yank()
		return true
	case bindings.MatchesPress(keyEditorYankPop, message):
		m.yankPop()
		return true
	case bindings.MatchesPress(keyEditorDeleteLineStart, message):
		m.killToLineStart()
		return true
	case bindings.MatchesPress(keyEditorDeleteLineEnd, message):
		m.killToLineEnd()
		return true
	case bindings.MatchesPress(keyEditorDeleteWordBack, message):
		m.killWordBackward()
		return true
	case bindings.MatchesPress(keyEditorDeleteWordFront, message):
		m.killWordForward()
		return true
	case bindings.MatchesPress(keyEditorJumpForward, message):
		m.jumpDirection = 1
		return true
	case bindings.MatchesPress(keyEditorJumpBackward, message):
		m.jumpDirection = -1
		return true
	default:
		return false
	}
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
	m.pushUndo(m.snapshot())
	m.setValueAtOffset(string(next), cursorOffset)
	m.lastEditAction = ""
	m.exitHistory()
}

func (m *composerModel) Reset() {
	if m != nil {
		if m.input.Value() != "" || len(m.images) != 0 {
			m.pushUndo(m.snapshot())
		}
		m.input.Reset()
		m.images = nil
		m.lastEditAction = ""
		m.jumpDirection = 0
		m.exitHistory()
		m.applyMaxHeight()
	}
}

func (m *composerModel) SetDraft(value string, images []llm.ImageBlock) {
	if m == nil {
		return
	}
	if m.input.Value() != value || len(m.images) != len(images) {
		m.pushUndo(m.snapshot())
	}
	m.input.SetValue(value)
	m.images = append([]llm.ImageBlock(nil), images...)
	m.lastEditAction = ""
	m.jumpDirection = 0
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
	m.lastEditAction = ""
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

func (m *composerModel) snapshot() composerSnapshot {
	if m == nil {
		return composerSnapshot{}
	}
	return composerSnapshot{
		value: m.input.Value(), cursor: m.CursorOffset(), images: append([]llm.ImageBlock(nil), m.images...),
	}
}

func (m *composerModel) pushUndo(snapshot composerSnapshot) {
	if m == nil {
		return
	}
	if len(m.undo) >= 100 {
		copy(m.undo, m.undo[len(m.undo)-99:])
		m.undo = m.undo[:99]
	}
	m.undo = append(m.undo, snapshot)
}

func (m *composerModel) undoEdit() {
	if m == nil || len(m.undo) == 0 {
		return
	}
	snapshot := m.undo[len(m.undo)-1]
	m.undo = m.undo[:len(m.undo)-1]
	m.images = append([]llm.ImageBlock(nil), snapshot.images...)
	m.setValueAtOffset(snapshot.value, snapshot.cursor)
	m.lastEditAction = ""
	m.jumpDirection = 0
	m.exitHistory()
	m.applyMaxHeight()
}

func (m *composerModel) setValueAtOffset(value string, offset int) {
	if m == nil {
		return
	}
	runes := []rune(value)
	offset = max(0, min(offset, len(runes)))
	m.input.SetValue(value)
	m.input.MoveToBegin()
	row, column := 0, 0
	for _, char := range runes[:offset] {
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
}

func (m *composerModel) killToLineStart() {
	runes, cursor := []rune(m.Value()), m.CursorOffset()
	start := cursor
	for start > 0 && runes[start-1] != '\n' {
		start--
	}
	if start < cursor {
		m.killRange(start, cursor, true)
	} else if cursor > 0 {
		m.killRange(cursor-1, cursor, true)
	}
}

func (m *composerModel) killToLineEnd() {
	runes, cursor := []rune(m.Value()), m.CursorOffset()
	end := cursor
	for end < len(runes) && runes[end] != '\n' {
		end++
	}
	if cursor < end {
		m.killRange(cursor, end, false)
	} else if end < len(runes) {
		m.killRange(end, end+1, false)
	}
}

func (m *composerModel) killWordBackward() {
	runes, cursor := []rune(m.Value()), m.CursorOffset()
	lineStart := cursor
	for lineStart > 0 && runes[lineStart-1] != '\n' {
		lineStart--
	}
	if cursor == lineStart {
		if cursor > 0 {
			m.killRange(cursor-1, cursor, true)
		}
		return
	}
	start := cursor
	for start > lineStart && unicode.IsSpace(runes[start-1]) {
		start--
	}
	if start > lineStart {
		class := editorRuneClass(runes[start-1])
		for start > lineStart && editorRuneClass(runes[start-1]) == class {
			start--
		}
	}
	m.killRange(start, cursor, true)
}

func (m *composerModel) killWordForward() {
	runes, cursor := []rune(m.Value()), m.CursorOffset()
	lineEnd := cursor
	for lineEnd < len(runes) && runes[lineEnd] != '\n' {
		lineEnd++
	}
	if cursor == lineEnd {
		if cursor < len(runes) {
			m.killRange(cursor, cursor+1, false)
		}
		return
	}
	end := cursor
	for end < lineEnd && unicode.IsSpace(runes[end]) {
		end++
	}
	if end < lineEnd {
		class := editorRuneClass(runes[end])
		for end < lineEnd && editorRuneClass(runes[end]) == class {
			end++
		}
	}
	m.killRange(cursor, end, false)
}

func editorRuneClass(value rune) int {
	switch {
	case unicode.IsSpace(value):
		return 0
	case unicode.IsLetter(value) || unicode.IsDigit(value) || value == '_':
		return 1
	default:
		return 2
	}
}

func (m *composerModel) killRange(start, end int, prepend bool) {
	if m == nil {
		return
	}
	runes := []rune(m.Value())
	start = max(0, min(start, len(runes)))
	end = max(start, min(end, len(runes)))
	if start == end {
		return
	}
	deleted := string(runes[start:end])
	m.pushUndo(m.snapshot())
	if m.lastEditAction == "kill" && len(m.killRing) != 0 {
		index := len(m.killRing) - 1
		if prepend {
			m.killRing[index] = deleted + m.killRing[index]
		} else {
			m.killRing[index] += deleted
		}
	} else {
		m.killRing = append(m.killRing, deleted)
		if len(m.killRing) > 60 {
			m.killRing = append([]string(nil), m.killRing[len(m.killRing)-60:]...)
		}
	}
	next := append(append([]rune(nil), runes[:start]...), runes[end:]...)
	m.setValueAtOffset(string(next), start)
	m.lastEditAction = "kill"
	m.exitHistory()
}

func (m *composerModel) yank() {
	if m == nil || len(m.killRing) == 0 {
		return
	}
	m.insertYank(m.killRing[len(m.killRing)-1], true)
}

func (m *composerModel) insertYank(value string, recordUndo bool) {
	if value == "" {
		return
	}
	runes, cursor := []rune(m.Value()), m.CursorOffset()
	if recordUndo {
		m.pushUndo(m.snapshot())
	}
	insert := []rune(value)
	next := make([]rune, 0, len(runes)+len(insert))
	next = append(next, runes[:cursor]...)
	next = append(next, insert...)
	next = append(next, runes[cursor:]...)
	m.setValueAtOffset(string(next), cursor+len(insert))
	m.lastEditAction = "yank"
	m.exitHistory()
}

func (m *composerModel) yankPop() {
	if m == nil || m.lastEditAction != "yank" || len(m.killRing) < 2 {
		return
	}
	runes, cursor := []rune(m.Value()), m.CursorOffset()
	previous := []rune(m.killRing[len(m.killRing)-1])
	start := cursor - len(previous)
	if start < 0 || string(runes[start:cursor]) != string(previous) {
		m.lastEditAction = ""
		return
	}
	m.pushUndo(m.snapshot())
	latest := m.killRing[len(m.killRing)-1]
	copy(m.killRing[1:], m.killRing[:len(m.killRing)-1])
	m.killRing[0] = latest
	nextYank := []rune(m.killRing[len(m.killRing)-1])
	next := make([]rune, 0, len(runes)-len(previous)+len(nextYank))
	next = append(next, runes[:start]...)
	next = append(next, nextYank...)
	next = append(next, runes[cursor:]...)
	m.setValueAtOffset(string(next), start+len(nextYank))
	m.lastEditAction = "yank"
}

func (m *composerModel) jumpToRune(target rune, direction int) {
	if m == nil || target == 0 {
		return
	}
	runes, cursor := []rune(m.Value()), m.CursorOffset()
	if direction > 0 {
		for index := cursor + 1; index < len(runes); index++ {
			if runes[index] == target {
				m.setValueAtOffset(m.Value(), index)
				break
			}
		}
	} else {
		for index := cursor - 1; index >= 0; index-- {
			if runes[index] == target {
				m.setValueAtOffset(m.Value(), index)
				break
			}
		}
	}
	m.lastEditAction = ""
}

func utf8FirstRune(value string) (rune, bool) {
	for _, char := range value {
		return char, true
	}
	return 0, false
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
