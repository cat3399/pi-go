package tui

import "strings"

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
		m.hasLayout = true
		return strings.Repeat("\n", max(0, height-1))
	}
	var lines []string
	if m.follow {
		lines, m.lastTop = m.viewTail(width, height, renderer)
	} else {
		lines, m.lastTop = m.viewAnchor(width, height, renderer)
	}
	m.hasLayout = true
	if len(lines) < height {
		lines = append(lines, make([]string, height-len(lines))...)
	} else if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
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
	if len(lines) < height {
		padding := make([]string, height-len(lines))
		lines = append(padding, lines...)
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
