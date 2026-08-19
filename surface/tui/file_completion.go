package tui

import (
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/cat3399/pi-go/internal/application"
)

type fileCompletionTarget struct {
	start  int
	end    int
	query  string
	quoted bool
}

func (t fileCompletionTarget) equal(other fileCompletionTarget) bool {
	return t.start == other.start && t.end == other.end && t.query == other.query && t.quoted == other.quoted
}

type fileCompletionModel struct {
	target    fileCompletionTarget
	items     []application.FileIndexEntry
	selected  int
	loading   bool
	err       string
	dismissed *fileCompletionTarget
}

func (m *fileCompletionModel) SetLoading(target fileCompletionTarget) {
	if m == nil {
		return
	}
	m.target = target
	m.items = nil
	m.selected = 0
	m.loading = true
	m.err = ""
	if m.dismissed != nil && !m.dismissed.equal(target) {
		m.dismissed = nil
	}
}

func (m *fileCompletionModel) SetResult(target fileCompletionTarget, items []application.FileIndexEntry, err error) {
	if m == nil || !m.target.equal(target) {
		return
	}
	m.loading = false
	m.items = append([]application.FileIndexEntry(nil), items...)
	m.selected = min(m.selected, max(0, len(m.items)-1))
	m.err = ""
	if err != nil {
		m.err = strings.TrimSpace(err.Error())
	}
}

func (m *fileCompletionModel) Hide() {
	if m == nil {
		return
	}
	m.items = nil
	m.loading = false
	m.err = ""
	m.selected = 0
	m.target = fileCompletionTarget{}
}

func (m *fileCompletionModel) Dismiss() {
	if m == nil {
		return
	}
	target := m.target
	m.dismissed = &target
	m.Hide()
}

func (m *fileCompletionModel) Active() bool {
	if m == nil || m.dismissed != nil && m.dismissed.equal(m.target) {
		return false
	}
	return m.loading || m.err != "" || len(m.items) != 0
}

func (m *fileCompletionModel) Move(delta int) {
	if m == nil || len(m.items) == 0 || delta == 0 {
		return
	}
	m.selected = (m.selected + delta) % len(m.items)
	if m.selected < 0 {
		m.selected += len(m.items)
	}
}

func (m *fileCompletionModel) Selected() (application.FileIndexEntry, bool) {
	if m == nil || m.selected < 0 || m.selected >= len(m.items) {
		return application.FileIndexEntry{}, false
	}
	return m.items[m.selected], true
}

func currentFileCompletionTarget(value string, cursor int) (fileCompletionTarget, bool) {
	runes := []rune(value)
	cursor = max(0, min(cursor, len(runes)))
	if cursor == 0 {
		return fileCompletionTarget{}, false
	}
	quotedStart := -1
	for index := cursor - 1; index >= 1; index-- {
		if runes[index-1] == '@' && runes[index] == '"' {
			quotedStart = index - 1
			break
		}
		if runes[index] == '"' || runes[index] == '\n' {
			break
		}
	}
	if quotedStart >= 0 {
		end := cursor
		for end < len(runes) && runes[end] != '"' && runes[end] != '\n' {
			end++
		}
		if end < len(runes) && runes[end] == '"' {
			end++
		}
		return fileCompletionTarget{
			start: quotedStart, end: end, query: string(runes[quotedStart+2 : cursor]), quoted: true,
		}, true
	}
	start := cursor
	for start > 0 && !unicode.IsSpace(runes[start-1]) {
		start--
	}
	if start >= cursor || runes[start] != '@' {
		return fileCompletionTarget{}, false
	}
	end := cursor
	for end < len(runes) && !unicode.IsSpace(runes[end]) {
		end++
	}
	return fileCompletionTarget{start: start, end: end, query: string(runes[start+1 : cursor])}, true
}

func fileCompletionEntries(result application.FileIndexResult) []application.FileIndexEntry {
	if result.HasQuery {
		return append([]application.FileIndexEntry(nil), result.Matches...)
	}
	limit := min(20, len(result.Files))
	entries := make([]application.FileIndexEntry, 0, limit)
	for _, path := range result.Files[:limit] {
		if strings.TrimSpace(path) != "" {
			entries = append(entries, application.FileIndexEntry{Path: path})
		}
	}
	return entries
}

func fileCompletionReplacement(entry application.FileIndexEntry, quoted bool) (string, int) {
	path := strings.TrimSpace(entry.Path)
	if entry.IsDir && !strings.HasSuffix(path, "/") {
		path += "/"
	}
	quoted = quoted || strings.IndexFunc(path, unicode.IsSpace) >= 0
	if quoted {
		path = strings.ReplaceAll(path, `"`, `\"`)
		replacement := `@"` + path + `"`
		if entry.IsDir {
			return replacement, len([]rune(replacement)) - 1
		}
		return replacement + " ", len([]rune(replacement)) + 1
	}
	replacement := "@" + path
	if entry.IsDir {
		return replacement, len([]rune(replacement))
	}
	return replacement + " ", len([]rune(replacement)) + 1
}

func (m *Model) refreshFileCompletion() tea.Cmd {
	target, ok := currentFileCompletionTarget(m.composer.Value(), m.composer.CursorOffset())
	if !ok || m.composer.HasImages() || m.selector != nil {
		m.fileCompletionGeneration++
		m.fileCompletion.Hide()
		return nil
	}
	if m.fileCompletion.dismissed != nil && m.fileCompletion.dismissed.equal(target) {
		return nil
	}
	if m.fileCompletion.target.equal(target) && m.fileCompletion.Active() {
		return nil
	}
	m.fileCompletionGeneration++
	generation := m.fileCompletionGeneration
	m.fileCompletion.SetLoading(target)
	cwd := strings.TrimSpace(m.state.CWD)
	if cwd == "" {
		cwd = m.api.DefaultCWD()
	}
	return loadFileCompletionsCmd(m.ctx, m.api, cwd, target, generation)
}

func (m *Model) handleFileCompletionLoaded(message fileCompletionsLoadedMsg) {
	if message.generation != m.fileCompletionGeneration {
		return
	}
	target, ok := currentFileCompletionTarget(m.composer.Value(), m.composer.CursorOffset())
	if !ok || !target.equal(message.target) {
		return
	}
	m.fileCompletion.SetResult(message.target, fileCompletionEntries(message.result), message.err)
}

func (m *Model) acceptFileCompletion() tea.Cmd {
	entry, ok := m.fileCompletion.Selected()
	if !ok {
		return nil
	}
	target := m.fileCompletion.target
	replacement, cursor := fileCompletionReplacement(entry, target.quoted)
	m.composer.ReplaceRuneRange(target.start, target.end, replacement, cursor)
	m.fileCompletion.Hide()
	m.updateSlashPalette()
	if entry.IsDir {
		return m.refreshFileCompletion()
	}
	return nil
}

func (m *Model) renderFileCompletion(width, maxLines int) []string {
	if maxLines <= 0 || !m.fileCompletion.Active() {
		return nil
	}
	if m.fileCompletion.loading {
		return []string{Truncate(m.theme.subtleStyle().Render("  Searching project files…"), width, "…", false)}
	}
	if m.fileCompletion.err != "" {
		return []string{Truncate(
			lipgloss.NewStyle().Foreground(m.theme.color(m.theme.Danger)).Render("  "+m.fileCompletion.err),
			width, "…", false,
		)}
	}
	count := min(maxLines, len(m.fileCompletion.items))
	start := max(0, min(m.fileCompletion.selected-count/2, len(m.fileCompletion.items)-count))
	lines := make([]string, 0, count)
	for index := start; index < start+count; index++ {
		entry := m.fileCompletion.items[index]
		label := "@" + entry.Path
		if entry.IsDir && !strings.HasSuffix(label, "/") {
			label += "/"
		}
		prefix := "  "
		style := m.theme.subtleStyle()
		if index == m.fileCompletion.selected {
			prefix = "› "
			style = lipgloss.NewStyle().Bold(true).Foreground(m.theme.color(m.theme.Primary))
		}
		lines = append(lines, Truncate(style.Render(prefix+label), width, "…", false))
	}
	return lines
}
