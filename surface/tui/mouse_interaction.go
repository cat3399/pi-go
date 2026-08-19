package tui

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
)

type transcriptSelection struct {
	anchor       transcriptCell
	focus        transcriptCell
	screen       bool
	screenAnchor screenCell
	screenFocus  screenCell
	active       bool
	dragged      bool
	pressedURL   string
}

type screenCell struct {
	row    int
	column int
}

type selectionAutoScrollMsg struct{ generation uint64 }

type urlOpenedMsg struct {
	url string
	err error
}

func (m *Model) handleMouseWheel(mouse tea.Mouse) tea.Cmd {
	if m.mode != ScreenFull {
		return nil
	}
	delta := 0
	switch mouse.Button {
	case tea.MouseWheelUp:
		delta = -1
	case tea.MouseWheelDown:
		delta = 1
	default:
		return nil
	}
	if m.selector != nil && mouse.Y >= m.transcript.lastHeight && mouse.Y < max(0, m.height-1) {
		m.selector.Move(delta)
		return nil
	}
	if delta < 0 {
		m.transcript.ScrollUp(-delta)
	} else {
		m.transcript.ScrollDown(delta)
	}
	return nil
}

func (m *Model) handleMouseClick(mouse tea.Mouse) tea.Cmd {
	if m.mode != ScreenFull || mouse.Button != tea.MouseLeft {
		return nil
	}
	if m.selector != nil || m.helpVisible {
		return m.startScreenSelection(mouse)
	}
	cell, row, ok := m.transcript.CellAt(mouse.Y, mouse.X, false)
	if !ok {
		return m.startScreenSelection(mouse)
	}
	m.stopSelectionAutoScroll()
	m.mouseSelection = &transcriptSelection{
		anchor: cell, focus: cell, active: true,
		pressedURL: osc8LinkAtColumn(row.text, cell.column),
	}
	return nil
}

func (m *Model) handleMouseMotion(mouse tea.Mouse) tea.Cmd {
	selection := m.mouseSelection
	if m.mode != ScreenFull || selection == nil || !selection.active || mouse.Button != tea.MouseLeft {
		return nil
	}
	if selection.screen {
		cell, _, ok := m.screenCellAt(mouse.Y, mouse.X, true)
		if !ok {
			return nil
		}
		if compareScreenCell(cell, selection.screenFocus) != 0 {
			selection.screenFocus = cell
			selection.dragged = true
			selection.pressedURL = ""
		}
		return nil
	}
	cell, _, ok := m.transcript.CellAt(mouse.Y, mouse.X, true)
	if !ok {
		m.stopSelectionAutoScroll()
		return nil
	}
	if compareTranscriptCell(cell, selection.focus) != 0 {
		selection.focus = cell
		selection.dragged = true
		selection.pressedURL = ""
	}
	first, last, ok := m.transcript.VisibleRowBounds()
	if !ok {
		m.stopSelectionAutoScroll()
		return nil
	}
	direction := 0
	if mouse.Y <= first {
		direction = -1
	} else if mouse.Y >= last {
		direction = 1
	}
	return m.startSelectionAutoScroll(direction, mouse)
}

func (m *Model) handleMouseRelease(mouse tea.Mouse) tea.Cmd {
	selection := m.mouseSelection
	if m.mode != ScreenFull || selection == nil || !selection.active {
		return nil
	}
	selection.active = false
	m.stopSelectionAutoScroll()
	if selection.screen {
		return m.releaseScreenSelection(mouse, selection)
	}
	cell, row, ok := m.transcript.CellAt(mouse.Y, mouse.X, true)
	if ok {
		if compareTranscriptCell(cell, selection.focus) != 0 {
			selection.dragged = true
		}
		selection.focus = cell
	}
	if !selection.dragged && ok && compareTranscriptCell(selection.anchor, selection.focus) == 0 &&
		selection.pressedURL != "" && osc8LinkAtColumn(row.text, cell.column) == selection.pressedURL {
		url := selection.pressedURL
		m.mouseSelection = nil
		return openURLCmd(m.openURL, url)
	}
	selection.pressedURL = ""
	text := m.transcript.SelectedText(selection.anchor, selection.focus)
	if text == "" {
		m.mouseSelection = nil
		return nil
	}
	m.setStatus("Copied!", statusSuccess)
	if m.setClipboard == nil {
		return nil
	}
	return m.setClipboard(text)
}

func (m *Model) startSelectionAutoScroll(direction int, pointer tea.Mouse) tea.Cmd {
	if direction == 0 {
		m.stopSelectionAutoScroll()
		return nil
	}
	m.mouseAutoPointer = pointer
	if m.mouseAutoDirection == direction {
		return nil
	}
	m.mouseAutoGeneration++
	m.mouseAutoDirection = direction
	return selectionAutoScrollCmd(m.mouseAutoGeneration)
}

func (m *Model) stopSelectionAutoScroll() {
	m.mouseAutoGeneration++
	m.mouseAutoDirection = 0
}

func (m *Model) handleSelectionAutoScroll(message selectionAutoScrollMsg) tea.Cmd {
	selection := m.mouseSelection
	if message.generation != m.mouseAutoGeneration || m.mouseAutoDirection == 0 ||
		selection == nil || !selection.active {
		return nil
	}
	if cell, _, ok := m.transcript.CellAt(m.mouseAutoPointer.Y, m.mouseAutoPointer.X, true); ok &&
		compareTranscriptCell(cell, selection.focus) != 0 {
		selection.focus = cell
		selection.dragged = true
		selection.pressedURL = ""
	}
	beforeFollow, beforeAnchor := m.transcript.follow, m.transcript.anchor
	if m.mouseAutoDirection < 0 {
		m.transcript.ScrollUp(1)
	} else {
		m.transcript.ScrollDown(1)
	}
	if beforeFollow == m.transcript.follow && beforeAnchor == m.transcript.anchor {
		m.stopSelectionAutoScroll()
		return nil
	}
	return selectionAutoScrollCmd(message.generation)
}

func selectionAutoScrollCmd(generation uint64) tea.Cmd {
	return tea.Tick(50*time.Millisecond, func(time.Time) tea.Msg {
		return selectionAutoScrollMsg{generation: generation}
	})
}

func (m *Model) renderTranscript(width, height int) string {
	view := m.transcript.View(width, height, m.renderer)
	selection := m.mouseSelection
	if selection == nil || selection.screen || compareTranscriptCell(selection.anchor, selection.focus) == 0 {
		return view
	}
	lines := strings.Split(view, "\n")
	for index := range lines {
		if index >= len(m.transcript.lastRows) || !m.transcript.lastRows[index].valid ||
			isTerminalImageLine(lines[index]) {
			continue
		}
		from, to, ok := selectionColumns(
			m.transcript.lastRows[index].text,
			m.transcript.lastRows[index].position,
			selection.anchor,
			selection.focus,
		)
		if !ok || to <= from {
			continue
		}
		lineWidth := VisibleWidth(lines[index])
		before, _ := SliceColumns(lines[index], 0, from, true)
		selected, _ := SliceColumns(lines[index], from, to-from, true)
		after, _ := SliceColumns(lines[index], to, max(0, lineWidth-to), true)
		lines[index] = before + reverseSelection(selected) + after
	}
	return strings.Join(lines, "\n")
}

func (m *Model) startScreenSelection(mouse tea.Mouse) tea.Cmd {
	cell, row, ok := m.screenCellAt(mouse.Y, mouse.X, false)
	if !ok {
		return nil
	}
	m.stopSelectionAutoScroll()
	m.mouseSelection = &transcriptSelection{
		screen: true, screenAnchor: cell, screenFocus: cell, active: true,
		pressedURL: osc8LinkAtColumn(row, cell.column),
	}
	return nil
}

func (m *Model) releaseScreenSelection(mouse tea.Mouse, selection *transcriptSelection) tea.Cmd {
	cell, row, ok := m.screenCellAt(mouse.Y, mouse.X, true)
	if ok {
		if compareScreenCell(cell, selection.screenFocus) != 0 {
			selection.dragged = true
		}
		selection.screenFocus = cell
	}
	if !selection.dragged && ok && compareScreenCell(selection.screenAnchor, selection.screenFocus) == 0 &&
		selection.pressedURL != "" && osc8LinkAtColumn(row, cell.column) == selection.pressedURL {
		url := selection.pressedURL
		m.mouseSelection = nil
		return openURLCmd(m.openURL, url)
	}
	selection.pressedURL = ""
	text := m.selectedScreenText(selection.screenAnchor, selection.screenFocus)
	if text == "" {
		m.mouseSelection = nil
		return nil
	}
	m.setStatus("Copied!", statusSuccess)
	if m.setClipboard == nil {
		return nil
	}
	return m.setClipboard(text)
}

func (m *Model) screenCellAt(row, column int, clamp bool) (screenCell, string, bool) {
	if m == nil || len(m.lastScreen) == 0 {
		return screenCell{}, "", false
	}
	if clamp {
		row = max(0, min(len(m.lastScreen)-1, row))
	} else if row < 0 || row >= len(m.lastScreen) {
		return screenCell{}, "", false
	}
	column = max(0, min(max(0, m.width-1), column))
	return screenCell{row: row, column: column}, m.lastScreen[row], true
}

func compareScreenCell(left, right screenCell) int {
	if left.row < right.row {
		return -1
	}
	if left.row > right.row {
		return 1
	}
	if left.column < right.column {
		return -1
	}
	if left.column > right.column {
		return 1
	}
	return 0
}

func (m *Model) selectedScreenText(anchor, focus screenCell) string {
	if m == nil || compareScreenCell(anchor, focus) == 0 {
		return ""
	}
	start, end := anchor, focus
	if compareScreenCell(start, end) > 0 {
		start, end = end, start
	}
	if start.row < 0 || end.row >= len(m.lastScreen) {
		return ""
	}
	selected := make([]string, 0, end.row-start.row+1)
	for row := start.row; row <= end.row; row++ {
		line := m.lastScreen[row]
		from, to := screenSelectionColumns(line, row, start, end)
		part, _ := SliceColumns(line, from, max(0, to-from), true)
		selected = append(selected, strings.TrimRightFunc(StripTerminalSequences(part), unicode.IsSpace))
	}
	return strings.Join(selected, "\n")
}

func screenSelectionColumns(line string, row int, start, end screenCell) (int, int) {
	lineWidth := VisibleWidth(line)
	from, to := 0, lineWidth
	if row == start.row {
		if cell, ok := cellRangeAtColumn(line, start.column); ok {
			from = cell.start
		} else {
			from = min(max(0, start.column), lineWidth)
		}
	}
	if row == end.row {
		if cell, ok := cellRangeAtColumn(line, end.column); ok {
			to = cell.end
		} else {
			to = min(max(0, end.column+1), lineWidth)
		}
	}
	return from, to
}

func (m *Model) prepareScreen(content string) string {
	m.lastScreen = strings.Split(content, "\n")
	selection := m.mouseSelection
	if selection == nil || !selection.screen || compareScreenCell(selection.screenAnchor, selection.screenFocus) == 0 {
		return content
	}
	start, end := selection.screenAnchor, selection.screenFocus
	if compareScreenCell(start, end) > 0 {
		start, end = end, start
	}
	lines := append([]string(nil), m.lastScreen...)
	for row := start.row; row <= end.row && row < len(lines); row++ {
		if row < 0 || isTerminalImageLine(lines[row]) {
			continue
		}
		from, to := screenSelectionColumns(lines[row], row, start, end)
		if to <= from {
			continue
		}
		lineWidth := VisibleWidth(lines[row])
		before, _ := SliceColumns(lines[row], 0, from, true)
		selected, _ := SliceColumns(lines[row], from, to-from, true)
		after, _ := SliceColumns(lines[row], to, max(0, lineWidth-to), true)
		lines[row] = before + reverseSelection(selected) + after
	}
	return strings.Join(lines, "\n")
}

func selectionColumns(
	line string,
	position transcriptPosition,
	anchor, focus transcriptCell,
) (int, int, bool) {
	start, end := anchor, focus
	if compareTranscriptCell(start, end) > 0 {
		start, end = end, start
	}
	if compareTranscriptPosition(position, start.position) < 0 ||
		compareTranscriptPosition(position, end.position) > 0 {
		return 0, 0, false
	}
	lineWidth := VisibleWidth(line)
	from, to := 0, lineWidth
	if compareTranscriptPosition(position, start.position) == 0 {
		if cell, ok := cellRangeAtColumn(line, start.column); ok {
			from = cell.start
		} else {
			from = min(max(0, start.column), lineWidth)
		}
	}
	if compareTranscriptPosition(position, end.position) == 0 {
		if cell, ok := cellRangeAtColumn(line, end.column); ok {
			to = cell.end
		} else {
			to = min(max(0, end.column+1), lineWidth)
		}
	}
	return from, to, to > from
}

func reverseSelection(text string) string {
	if text == "" {
		return ""
	}
	var result strings.Builder
	result.WriteString("\x1b[7m")
	for _, token := range tokenizeTerminal(text) {
		result.WriteString(token.text)
		if token.control && strings.HasPrefix(token.text, "\x1b[") && strings.HasSuffix(token.text, "m") {
			result.WriteString("\x1b[7m")
		}
	}
	result.WriteString("\x1b[27m")
	return result.String()
}

func isTerminalImageLine(line string) bool {
	return strings.Contains(line, "\x1b_G") || strings.Contains(line, "\x1b]1337;File=")
}

func osc8LinkAtColumn(line string, column int) string {
	if column < 0 {
		return ""
	}
	activeURL := ""
	current := 0
	for _, token := range tokenizeTerminal(line) {
		if token.control {
			if url, ok := parseOSC8Link(token.text); ok {
				activeURL = url
			}
			continue
		}
		width := clusterWidth(token.text)
		if width > 0 && column >= current && column < current+width {
			return activeURL
		}
		current += width
	}
	return ""
}

func parseOSC8Link(code string) (string, bool) {
	if !strings.HasPrefix(code, "\x1b]8;") {
		return "", false
	}
	body := strings.TrimPrefix(code, "\x1b]8;")
	switch {
	case strings.HasSuffix(body, "\a"):
		body = strings.TrimSuffix(body, "\a")
	case strings.HasSuffix(body, "\x1b\\"):
		body = strings.TrimSuffix(body, "\x1b\\")
	default:
		return "", false
	}
	parts := strings.SplitN(body, ";", 2)
	if len(parts) != 2 {
		return "", false
	}
	return parts[1], true
}

func openURLCmd(openURL func(string) error, url string) tea.Cmd {
	if openURL == nil || strings.TrimSpace(url) == "" {
		return nil
	}
	return func() tea.Msg {
		return urlOpenedMsg{url: url, err: openURL(url)}
	}
}

// openURLWithSystem mirrors upstream's no-shell launcher so OSC 8 targets are
// passed as one argument and cannot be reinterpreted as shell syntax.
func openURLWithSystem(target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return errors.New("empty URL")
	}
	var name string
	var arguments []string
	switch runtime.GOOS {
	case "darwin":
		name, arguments = "open", []string{target}
	case "windows":
		name, arguments = "rundll32", []string{"url.dll,FileProtocolHandler", target}
	default:
		name, arguments = "xdg-open", []string{target}
	}
	command := exec.Command(name, arguments...)
	if err := command.Start(); err != nil {
		return err
	}
	if command.Process == nil {
		return nil
	}
	return command.Process.Release()
}
