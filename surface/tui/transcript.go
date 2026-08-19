package tui

import (
	"strings"
	"unicode"
)

type transcriptRenderKey struct {
	width    int
	revision uint64
	renderer string
}

type transcriptItem struct {
	content contentItem
	cache   map[transcriptRenderKey][]string
}

type transcriptPosition struct {
	item int
	line int
}

type transcriptRow struct {
	position transcriptPosition
	text     string
	valid    bool
}

type transcriptCell struct {
	position transcriptPosition
	column   int
}

// transcriptModel is a message-level virtual list. It renders only enough
// cached items to fill the viewport and keeps a semantic item anchor while the
// user is scrolled away from the live tail.
type transcriptModel struct {
	items []transcriptItem
	index map[string]int

	follow bool
	anchor contentAnchor

	lastTop      transcriptPosition
	lastWidth    int
	lastHeight   int
	lastRenderer itemRenderer
	lastRows     []transcriptRow
	hasLayout    bool
}

type contentAnchor struct {
	id   string
	line int
}

func newTranscriptModel() transcriptModel {
	return transcriptModel{index: make(map[string]int), follow: true}
}

func (m *transcriptModel) Len() int {
	if m == nil {
		return 0
	}
	return len(m.items)
}

func (m *transcriptModel) Following() bool {
	return m == nil || m.follow
}

func (m *transcriptModel) SetItems(values []contentItem) {
	if m == nil {
		return
	}
	previous := make(map[string]transcriptItem, len(m.items))
	for _, item := range m.items {
		previous[item.content.ID] = item
	}
	next := make([]transcriptItem, 0, len(values))
	for _, value := range values {
		if value.ID == "" {
			continue
		}
		entry := transcriptItem{content: value, cache: make(map[transcriptRenderKey][]string)}
		if existing, ok := previous[value.ID]; ok && existing.content.Revision == value.Revision {
			entry.cache = existing.cache
		}
		next = append(next, entry)
	}
	m.items = next
	m.rebuildIndex()
	if !m.follow {
		if _, ok := m.index[m.anchor.id]; !ok {
			m.follow = true
			m.anchor = contentAnchor{}
		}
	}
	m.hasLayout = false
}

func (m *transcriptModel) Upsert(value contentItem) {
	if m == nil || value.ID == "" {
		return
	}
	if index, ok := m.index[value.ID]; ok {
		entry := &m.items[index]
		if entry.content.Revision != value.Revision {
			entry.cache = make(map[transcriptRenderKey][]string)
		}
		entry.content = value
		m.hasLayout = false
		return
	}
	m.items = append(m.items, transcriptItem{content: value, cache: make(map[transcriptRenderKey][]string)})
	m.index[value.ID] = len(m.items) - 1
	m.hasLayout = false
}

func (m *transcriptModel) UpdateToolExecution(
	callID, name, arguments string,
	result []contentBlock,
	live, failed bool,
) (contentItem, bool) {
	if m == nil || callID == "" {
		return contentItem{}, false
	}
	for index := len(m.items) - 1; index >= 0; index-- {
		entry := &m.items[index]
		callIndex := -1
		blocks := make([]contentBlock, 0, len(entry.content.Blocks)+len(result))
		for _, block := range entry.content.Blocks {
			if block.Kind == contentBlockToolCall && block.ToolCallID == callID {
				callIndex = len(blocks)
				block.ToolName = name
				block.Text = arguments
				block.Live = live
				block.IsError = failed
				blocks = append(blocks, block)
				continue
			}
			if block.ToolCallID == callID {
				continue
			}
			blocks = append(blocks, block)
		}
		if callIndex < 0 {
			continue
		}
		owned := append([]contentBlock(nil), result...)
		blocks = append(blocks, make([]contentBlock, len(owned))...)
		copy(blocks[callIndex+1+len(owned):], blocks[callIndex+1:len(blocks)-len(owned)])
		copy(blocks[callIndex+1:], owned)
		entry.content.Blocks = blocks
		entry.content.Revision++
		entry.content.Live = false
		for _, block := range blocks {
			entry.content.Live = entry.content.Live || block.Live
		}
		entry.content.Failed = entry.content.Failed || failed
		entry.cache = make(map[transcriptRenderKey][]string)
		m.hasLayout = false
		return entry.content, true
	}
	return contentItem{}, false
}

func (m *transcriptModel) MergeToolResult(callID string, result []contentBlock) (contentItem, bool) {
	if m == nil || callID == "" {
		return contentItem{}, false
	}
	for index := len(m.items) - 1; index >= 0; index-- {
		entry := &m.items[index]
		if !mergeToolResultBlocks(&entry.content, callID, result) {
			continue
		}
		entry.cache = make(map[transcriptRenderKey][]string)
		m.hasLayout = false
		return entry.content, true
	}
	return contentItem{}, false
}

func (m *transcriptModel) Remove(id string) {
	if m == nil || id == "" {
		return
	}
	index, ok := m.index[id]
	if !ok {
		return
	}
	m.items = append(m.items[:index], m.items[index+1:]...)
	m.rebuildIndex()
	if !m.follow && m.anchor.id == id {
		if index < len(m.items) {
			m.anchor.id = m.items[index].content.ID
			m.anchor.line = 0
		} else {
			m.follow = true
			m.anchor = contentAnchor{}
		}
	}
	m.hasLayout = false
}

func (m *transcriptModel) rebuildIndex() {
	m.index = make(map[string]int, len(m.items))
	for index := range m.items {
		m.index[m.items[index].content.ID] = index
	}
}

func (m *transcriptModel) View(width, height int, renderer itemRenderer) string {
	if m == nil || height <= 0 {
		return ""
	}
	if width <= 0 {
		width = 1
	}
	m.lastWidth, m.lastHeight, m.lastRenderer = width, height, renderer
	if len(m.items) == 0 {
		m.lastTop = transcriptPosition{}
		m.lastRows = make([]transcriptRow, height)
		m.hasLayout = true
		return strings.Repeat("\n", max(0, height-1))
	}
	var lines []string
	if m.follow {
		lines, m.lastTop = m.viewTail(width, height, renderer)
	} else {
		lines, m.lastTop = m.viewAnchor(width, height, renderer)
	}
	rows := m.rowsFrom(m.lastTop, lines, width, renderer)
	if len(rows) < height {
		padding := make([]transcriptRow, height-len(rows))
		if m.follow {
			rows = append(padding, rows...)
		} else {
			rows = append(rows, padding...)
		}
	} else if len(rows) > height {
		rows = rows[:height]
	}
	m.lastRows = rows
	m.hasLayout = true
	visible := make([]string, len(rows))
	for index := range rows {
		visible[index] = rows[index].text
	}
	return strings.Join(visible, "\n")
}

func (m *transcriptModel) viewTail(width, height int, renderer itemRenderer) ([]string, transcriptPosition) {
	lines := make([]string, 0, height)
	top := transcriptPosition{item: len(m.items) - 1}
	for index := len(m.items) - 1; index >= 0 && len(lines) < height; index-- {
		itemLines := m.lines(index, width, renderer)
		needed := height - len(lines)
		start := 0
		if len(itemLines) > needed {
			start = len(itemLines) - needed
		}
		prefix := append([]string(nil), itemLines[start:]...)
		lines = append(prefix, lines...)
		top = transcriptPosition{item: index, line: start}
	}
	return lines, top
}

func (m *transcriptModel) viewAnchor(width, height int, renderer itemRenderer) ([]string, transcriptPosition) {
	index, ok := m.index[m.anchor.id]
	if !ok {
		m.follow = true
		return m.viewTail(width, height, renderer)
	}
	itemLines := m.lines(index, width, renderer)
	line := min(max(0, m.anchor.line), max(0, len(itemLines)-1))
	top := transcriptPosition{item: index, line: line}
	lines := make([]string, 0, height)
	for index < len(m.items) && len(lines) < height {
		itemLines = m.lines(index, width, renderer)
		start := 0
		if index == top.item {
			start = top.line
		}
		lines = append(lines, itemLines[start:]...)
		index++
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	return lines, top
}

func (m *transcriptModel) lines(index, width int, renderer itemRenderer) []string {
	if index < 0 || index >= len(m.items) {
		return []string{""}
	}
	entry := &m.items[index]
	key := transcriptRenderKey{width: width, revision: entry.content.Revision}
	if renderer != nil {
		key.renderer = renderer.CacheKey()
	}
	lines, ok := entry.cache[key]
	if !ok {
		if renderer == nil {
			lines = Wrap(entry.content.Title, width)
		} else {
			lines = renderer.Render(entry.content, width)
		}
		if len(lines) == 0 {
			lines = []string{""}
		}
		entry.cache[key] = append([]string(nil), lines...)
	}
	result := append([]string(nil), lines...)
	if index > 0 {
		result = append([]string{""}, result...)
	}
	return result
}

func (m *transcriptModel) rowsFrom(
	top transcriptPosition,
	lines []string,
	width int,
	renderer itemRenderer,
) []transcriptRow {
	rows := make([]transcriptRow, 0, len(lines))
	position := top
	for _, line := range lines {
		if position.item < 0 || position.item >= len(m.items) {
			break
		}
		itemLines := m.lines(position.item, width, renderer)
		if position.line < 0 || position.line >= len(itemLines) {
			break
		}
		rows = append(rows, transcriptRow{position: position, text: line, valid: true})
		position.line++
		if position.line >= len(itemLines) {
			position.item++
			position.line = 0
		}
	}
	return rows
}

func (m *transcriptModel) CellAt(row, column int, clamp bool) (transcriptCell, transcriptRow, bool) {
	if m == nil || !m.hasLayout || len(m.lastRows) == 0 {
		return transcriptCell{}, transcriptRow{}, false
	}
	if clamp {
		row = max(0, min(len(m.lastRows)-1, row))
	} else if row < 0 || row >= len(m.lastRows) {
		return transcriptCell{}, transcriptRow{}, false
	}
	if !m.lastRows[row].valid && clamp {
		nearest := -1
		for distance := 1; distance < len(m.lastRows); distance++ {
			if row-distance >= 0 && m.lastRows[row-distance].valid {
				nearest = row - distance
				break
			}
			if row+distance < len(m.lastRows) && m.lastRows[row+distance].valid {
				nearest = row + distance
				break
			}
		}
		if nearest >= 0 {
			row = nearest
		}
	}
	visible := m.lastRows[row]
	if !visible.valid {
		return transcriptCell{}, transcriptRow{}, false
	}
	column = max(0, min(max(0, m.lastWidth-1), column))
	return transcriptCell{position: visible.position, column: column}, visible, true
}

func (m *transcriptModel) VisibleRowBounds() (int, int, bool) {
	if m == nil || !m.hasLayout {
		return 0, 0, false
	}
	first, last := -1, -1
	for index, row := range m.lastRows {
		if !row.valid {
			continue
		}
		if first < 0 {
			first = index
		}
		last = index
	}
	return first, last, first >= 0
}

func compareTranscriptPosition(left, right transcriptPosition) int {
	if left.item != right.item {
		if left.item < right.item {
			return -1
		}
		return 1
	}
	if left.line != right.line {
		if left.line < right.line {
			return -1
		}
		return 1
	}
	return 0
}

func compareTranscriptCell(left, right transcriptCell) int {
	if compared := compareTranscriptPosition(left.position, right.position); compared != 0 {
		return compared
	}
	if left.column < right.column {
		return -1
	}
	if left.column > right.column {
		return 1
	}
	return 0
}

func (m *transcriptModel) lineAt(position transcriptPosition) (string, bool) {
	if m == nil || position.item < 0 || position.item >= len(m.items) ||
		position.line < 0 || m.lastWidth <= 0 {
		return "", false
	}
	lines := m.lines(position.item, m.lastWidth, m.lastRenderer)
	if position.line >= len(lines) {
		return "", false
	}
	return lines[position.line], true
}

func (m *transcriptModel) nextPosition(position transcriptPosition) (transcriptPosition, bool) {
	if _, ok := m.lineAt(position); !ok {
		return transcriptPosition{}, false
	}
	position.line++
	if position.line < len(m.lines(position.item, m.lastWidth, m.lastRenderer)) {
		return position, true
	}
	position.item++
	position.line = 0
	if position.item >= len(m.items) {
		return transcriptPosition{}, false
	}
	return position, true
}

func (m *transcriptModel) SelectedText(anchor, focus transcriptCell) string {
	if m == nil || compareTranscriptCell(anchor, focus) == 0 {
		return ""
	}
	start, end := anchor, focus
	if compareTranscriptCell(start, end) > 0 {
		start, end = end, start
	}
	if _, ok := m.lineAt(start.position); !ok {
		return ""
	}
	if _, ok := m.lineAt(end.position); !ok {
		return ""
	}
	selected := make([]string, 0)
	for position := start.position; ; {
		line, ok := m.lineAt(position)
		if !ok {
			return ""
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
		part, _ := SliceColumns(line, from, max(0, to-from), true)
		selected = append(selected, strings.TrimRightFunc(StripTerminalSequences(part), unicode.IsSpace))
		if compareTranscriptPosition(position, end.position) == 0 {
			break
		}
		next, ok := m.nextPosition(position)
		if !ok || compareTranscriptPosition(next, end.position) > 0 {
			return ""
		}
		position = next
	}
	return strings.Join(selected, "\n")
}

func (m *transcriptModel) ScrollUp(lines int) {
	if m == nil || lines <= 0 || len(m.items) == 0 || !m.hasLayout {
		return
	}
	position := m.lastTop
	moved := false
	for range lines {
		if position.line > 0 {
			position.line--
			moved = true
			continue
		}
		if position.item == 0 {
			break
		}
		position.item--
		position.line = len(m.lines(position.item, m.lastWidth, m.lastRenderer)) - 1
		moved = true
	}
	if moved {
		m.follow = false
		m.anchor = contentAnchor{id: m.items[position.item].content.ID, line: position.line}
		m.hasLayout = false
	}
}

func (m *transcriptModel) ScrollDown(lines int) {
	if m == nil || lines <= 0 || len(m.items) == 0 || m.follow {
		return
	}
	index, ok := m.index[m.anchor.id]
	if !ok {
		m.ScrollToBottom()
		return
	}
	position := transcriptPosition{item: index, line: m.anchor.line}
	for range lines {
		itemLines := m.lines(position.item, m.lastWidth, m.lastRenderer)
		if position.line+1 < len(itemLines) {
			position.line++
			continue
		}
		if position.item+1 >= len(m.items) {
			m.ScrollToBottom()
			return
		}
		position.item++
		position.line = 0
	}
	m.anchor = contentAnchor{id: m.items[position.item].content.ID, line: position.line}
	if m.reachesEnd(position, m.lastHeight) {
		m.ScrollToBottom()
		return
	}
	m.hasLayout = false
}

func (m *transcriptModel) reachesEnd(position transcriptPosition, height int) bool {
	remaining := max(1, height)
	for position.item < len(m.items) {
		itemLines := m.lines(position.item, m.lastWidth, m.lastRenderer)
		remaining -= len(itemLines) - position.line
		if remaining <= 0 {
			return false
		}
		position.item++
		position.line = 0
	}
	return true
}

func (m *transcriptModel) ScrollToBottom() {
	if m == nil {
		return
	}
	m.follow = true
	m.anchor = contentAnchor{}
	m.hasLayout = false
}

func (m *transcriptModel) ScrollToTop() {
	if m == nil || len(m.items) == 0 {
		return
	}
	m.follow = false
	m.anchor = contentAnchor{id: m.items[0].content.ID}
	m.hasLayout = false
}

func (m *transcriptModel) ScrollToPreviousPrompt() {
	if m == nil || len(m.items) == 0 {
		return
	}
	start := len(m.items) - 1
	if !m.follow {
		if current, ok := m.index[m.anchor.id]; ok {
			start = current - 1
		}
	}
	for index := start; index >= 0; index-- {
		if m.items[index].content.Role != contentRoleUser {
			continue
		}
		m.follow = false
		m.anchor = contentAnchor{id: m.items[index].content.ID}
		m.hasLayout = false
		return
	}
	m.ScrollToTop()
}

func (m *transcriptModel) ScrollToNextPrompt() {
	if m == nil || len(m.items) == 0 || m.follow {
		return
	}
	start := 0
	if current, ok := m.index[m.anchor.id]; ok {
		start = current + 1
	}
	for index := start; index < len(m.items); index++ {
		if m.items[index].content.Role != contentRoleUser {
			continue
		}
		m.anchor = contentAnchor{id: m.items[index].content.ID}
		m.hasLayout = false
		return
	}
	m.ScrollToBottom()
}
