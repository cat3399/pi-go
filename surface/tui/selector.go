package tui

import (
	"sort"
	"strings"
	"unicode"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type selectorKind uint8

const (
	selectorModels selectorKind = iota + 1
	selectorSessions
	selectorThinking
	selectorTools
	selectorSettings
	selectorImportConfirm
	selectorSessionRename
	selectorSessionDelete
	selectorTrustConfirm
	selectorTree
	selectorFork
	selectorTreeSummary
	selectorTreeSummaryCustom
	selectorLoginProvider
	selectorLoginMethod
	selectorLoginAPIKey
	selectorLoginOAuth
	selectorLogoutProvider
	selectorLogoutConfirm
)

type selectorItem struct {
	Key         string
	Title       string
	Badge       string
	Description string
	Keywords    string
	Current     bool
	Checked     bool
	sortTime    int64
	sortCount   int
}

type selectorModel struct {
	kind            selectorKind
	title           string
	searchable      bool
	multi           bool
	theme           Theme
	input           textinput.Model
	items           []selectorItem
	filtered        []int
	selected        int
	loading         bool
	err             string
	notice          string
	autoSelectQuery string
	searchVisible   bool
}

func newSelectorModel(theme Theme, kind selectorKind, title, query string, searchable, multi bool) *selectorModel {
	input := textinput.New()
	input.Prompt = "Search: "
	input.Placeholder = "type to filter"
	input.SetVirtualCursor(false)
	input.SetValue(query)
	styles := textinput.DefaultDarkStyles()
	styles.Focused.Text = lipgloss.NewStyle().Foreground(theme.color(theme.Foreground))
	styles.Focused.Prompt = lipgloss.NewStyle().Bold(true).Foreground(theme.color(theme.Primary))
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(theme.color(theme.Subtle))
	styles.Blurred = styles.Focused
	styles.Cursor.Color = theme.color(theme.Primary)
	styles.Cursor.Shape = tea.CursorBar
	styles.Cursor.Blink = true
	input.SetStyles(styles)
	return &selectorModel{
		kind: kind, title: title, searchable: searchable, multi: multi, theme: theme,
		input: input, loading: true, selected: 0, autoSelectQuery: strings.TrimSpace(query),
	}
}

func (s *selectorModel) Focus() tea.Cmd {
	if s == nil || !s.searchable {
		return nil
	}
	return s.input.Focus()
}

func (s *selectorModel) Blur() {
	if s != nil {
		s.input.Blur()
	}
}

func (s *selectorModel) Query() string {
	if s == nil || !s.searchable {
		return ""
	}
	return s.input.Value()
}

func (s *selectorModel) TakeAutoSelectQuery() string {
	if s == nil {
		return ""
	}
	query := s.autoSelectQuery
	s.autoSelectQuery = ""
	if strings.TrimSpace(s.Query()) != strings.TrimSpace(query) {
		return ""
	}
	return query
}

func (s *selectorModel) SetLoading() {
	if s == nil {
		return
	}
	s.loading = true
	s.err = ""
}

func (s *selectorModel) SetError(err error) {
	if s == nil {
		return
	}
	s.loading = false
	if err == nil {
		s.err = ""
		return
	}
	s.err = strings.TrimSpace(err.Error())
}

func (s *selectorModel) SetItems(items []selectorItem, notice string) {
	if s == nil {
		return
	}
	previousKey := ""
	if selected, ok := s.Selected(); ok {
		previousKey = selected.Key
	}
	s.items = append([]selectorItem(nil), items...)
	s.loading = false
	s.err = ""
	s.notice = strings.TrimSpace(notice)
	s.refilterWithKey(false, previousKey)
}

func (s *selectorModel) Update(message tea.Msg) tea.Cmd {
	if s == nil || !s.searchable {
		return nil
	}
	before := s.input.Value()
	updated, command := s.input.Update(message)
	s.input = updated
	if s.input.Value() != before && !selectorUsesRawInput(s.kind) {
		s.refilter(true)
	}
	return command
}

func selectorUsesRawInput(kind selectorKind) bool {
	switch kind {
	case selectorTreeSummaryCustom, selectorSessionRename, selectorLoginAPIKey, selectorLoginOAuth:
		return true
	default:
		return false
	}
}

func (s *selectorModel) Move(delta int) {
	if s == nil || len(s.filtered) == 0 || delta == 0 {
		return
	}
	s.selected = (s.selected + delta) % len(s.filtered)
	if s.selected < 0 {
		s.selected += len(s.filtered)
	}
}

func (s *selectorModel) MovePage(delta, pageSize int) {
	if s == nil || len(s.filtered) == 0 || delta == 0 {
		return
	}
	pageSize = max(1, pageSize)
	s.selected = max(0, min(len(s.filtered)-1, s.selected+delta*pageSize))
}

func (s *selectorModel) Selected() (selectorItem, bool) {
	if s == nil || s.selected < 0 || s.selected >= len(s.filtered) {
		return selectorItem{}, false
	}
	index := s.filtered[s.selected]
	if index < 0 || index >= len(s.items) {
		return selectorItem{}, false
	}
	return s.items[index], true
}

func (s *selectorModel) ToggleSelected() bool {
	if s == nil || !s.multi || s.selected < 0 || s.selected >= len(s.filtered) {
		return false
	}
	index := s.filtered[s.selected]
	s.items[index].Checked = !s.items[index].Checked
	return true
}

func (s *selectorModel) CheckedKeys() []string {
	if s == nil {
		return nil
	}
	keys := make([]string, 0, len(s.items))
	for _, item := range s.items {
		if item.Checked {
			keys = append(keys, item.Key)
		}
	}
	return keys
}

func (s *selectorModel) refilter(queryChanged bool) {
	if s == nil {
		return
	}
	previousKey := ""
	if selected, ok := s.Selected(); ok {
		previousKey = selected.Key
	}
	s.refilterWithKey(queryChanged, previousKey)
}

func (s *selectorModel) refilterWithKey(queryChanged bool, previousKey string) {
	if s == nil {
		return
	}
	type rankedIndex struct {
		index int
		rank  int
	}
	query := strings.TrimSpace(strings.ToLower(s.Query()))
	ranked := make([]rankedIndex, 0, len(s.items))
	for index, item := range s.items {
		rank := selectorItemRank(item, query)
		if rank >= 0 {
			ranked = append(ranked, rankedIndex{index: index, rank: rank})
		}
	}
	sort.SliceStable(ranked, func(left, right int) bool {
		return ranked[left].rank < ranked[right].rank
	})
	s.filtered = make([]int, len(ranked))
	for index, candidate := range ranked {
		s.filtered[index] = candidate.index
	}
	s.selected = 0
	if query != "" && (queryChanged || previousKey == "") {
		return
	}
	for filteredIndex, itemIndex := range s.filtered {
		item := s.items[itemIndex]
		if previousKey != "" && item.Key == previousKey {
			s.selected = filteredIndex
			return
		}
		if previousKey == "" && item.Current {
			s.selected = filteredIndex
			return
		}
	}
}

func (s *selectorModel) SelectKey(key string) bool {
	if s == nil || key == "" {
		return false
	}
	for filteredIndex, itemIndex := range s.filtered {
		if s.items[itemIndex].Key == key {
			s.selected = filteredIndex
			return true
		}
	}
	return false
}

func selectorItemRank(item selectorItem, query string) int {
	if query == "" {
		return 0
	}
	haystacks := []struct {
		value   string
		penalty int
	}{
		{value: strings.ToLower(item.Key)},
		{value: strings.ToLower(item.Title), penalty: 2},
		{value: strings.ToLower(item.Badge), penalty: 4},
		{value: strings.ToLower(item.Description), penalty: 6},
		{value: strings.ToLower(item.Keywords), penalty: 8},
	}
	total := 0
	for _, term := range strings.Fields(query) {
		best := -1
		for _, field := range haystacks {
			rank := selectorTextRank(field.value, term)
			if rank >= 0 {
				rank += field.penalty
			}
			if rank >= 0 && (best < 0 || rank < best) {
				best = rank
			}
		}
		if best < 0 {
			return -1
		}
		total += best
	}
	return total
}

func selectorTextRank(value, query string) int {
	switch {
	case value == query:
		return 0
	case strings.HasPrefix(value, query):
		return 10
	case strings.Contains(value, query):
		return 20
	case selectorSubsequence(value, query):
		return 30
	default:
		return -1
	}
}

func selectorSubsequence(value, query string) bool {
	remaining := []rune(query)
	if len(remaining) == 0 {
		return true
	}
	for _, candidate := range []rune(value) {
		if unicode.ToLower(candidate) != unicode.ToLower(remaining[0]) {
			continue
		}
		remaining = remaining[1:]
		if len(remaining) == 0 {
			return true
		}
	}
	return false
}

func (s *selectorModel) View(width, maxHeight int) string {
	if s == nil || width <= 0 || maxHeight <= 0 {
		return ""
	}
	s.searchVisible = false
	innerWidth := max(1, width-4)
	innerHeight := max(1, maxHeight-2)
	// textinput.Width applies to the editable value and does not include its
	// prompt. Reserve prompt columns so the physical cursor stays inside the
	// selector frame on narrow terminals and long queries.
	s.input.SetWidth(max(1, innerWidth-VisibleWidth(s.input.Prompt)))
	// SetWidth does not recompute the textinput viewport. Initial slash-command
	// queries are installed before the selector knows the terminal width, so
	// re-applying the cursor position keeps a long first frame aligned with the
	// real cursor instead of showing the beginning of the query with the cursor
	// clamped at its end.
	s.input.SetCursor(s.input.Position())

	lines := make([]string, 0, innerHeight)
	title := lipgloss.NewStyle().Bold(true).Foreground(s.theme.color(s.theme.Primary)).Render(sanitizeDisplayText(s.title))
	lines = append(lines, Truncate(title, innerWidth, "…", false))
	if s.searchable && len(lines) < innerHeight {
		lines = append(lines, Truncate(s.input.View(), innerWidth, "", true))
		s.searchVisible = true
	}

	reserveFooter := 0
	if innerHeight-len(lines) >= 2 {
		reserveFooter = 1
	}
	reserveNotice := 0
	if s.notice != "" && innerHeight-len(lines)-reserveFooter >= 2 {
		reserveNotice = 1
	}
	reserveDetail := 0
	if !s.loading && s.err == "" {
		if _, ok := s.Selected(); ok && innerHeight-len(lines)-reserveFooter-reserveNotice >= 3 {
			reserveDetail = 1
		}
	}
	itemBudget := max(1, innerHeight-len(lines)-reserveFooter-reserveNotice-reserveDetail)
	itemLines := s.renderItems(innerWidth, itemBudget)
	for _, line := range itemLines {
		if len(lines) >= innerHeight-reserveFooter-reserveNotice-reserveDetail {
			break
		}
		lines = append(lines, line)
	}
	if reserveDetail != 0 {
		selected, _ := s.Selected()
		detail := selected.Description
		if detail == "" {
			detail = selected.Key
		}
		lines = append(lines, Truncate(s.theme.subtleStyle().Render("  "+singleLine(detail)), innerWidth, "…", false))
	}
	if reserveNotice != 0 {
		lines = append(lines, Truncate(
			lipgloss.NewStyle().Foreground(s.theme.color(s.theme.Warning)).Render(singleLine(s.notice)),
			innerWidth, "…", false,
		))
	}
	if reserveFooter != 0 {
		hint := "↑↓ navigate • enter select • esc back"
		if s.loading {
			hint = "esc back"
		} else if s.err != "" {
			hint = "ctrl+r retry • esc back"
		} else if s.multi {
			hint = "↑↓ navigate • space toggle • enter apply • esc back"
		} else if s.kind == selectorTreeSummaryCustom {
			hint = "enter summarize • esc back"
		} else if s.kind == selectorSessionRename {
			hint = "enter rename • esc back"
		} else if s.kind == selectorLoginAPIKey {
			hint = "enter save • esc back"
		} else if s.kind == selectorLoginOAuth {
			hint = "enter submit callback • esc cancel login"
		} else if s.kind == selectorSessions {
			hint = "enter open • ctrl+n named • ctrl+s sort • ctrl+p path • ctrl+e rename • ctrl+d delete"
		}
		lines = append(lines, Truncate(s.theme.subtleStyle().Render(hint), innerWidth, "…", false))
	}
	if len(lines) > innerHeight {
		lines = lines[:innerHeight]
	}
	frame := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(s.theme.color(s.theme.Primary)).
		Padding(0, 1).
		Width(width)
	return frame.Render(strings.Join(lines, "\n"))
}

func (s *selectorModel) renderItems(width, maxLines int) []string {
	if maxLines <= 0 {
		return nil
	}
	if s.loading {
		return []string{Truncate(s.theme.subtleStyle().Render("  Loading…"), width, "…", false)}
	}
	if s.err != "" {
		return []string{Truncate(
			lipgloss.NewStyle().Foreground(s.theme.color(s.theme.Danger)).Render("  "+singleLine(s.err)),
			width, "…", false,
		)}
	}
	if len(s.filtered) == 0 {
		return []string{Truncate(s.theme.subtleStyle().Render("  "+s.emptyLabel()), width, "…", false)}
	}
	count := min(maxLines, len(s.filtered))
	start := max(0, min(s.selected-count/2, len(s.filtered)-count))
	lines := make([]string, 0, count)
	for filteredIndex := start; filteredIndex < start+count; filteredIndex++ {
		item := s.items[s.filtered[filteredIndex]]
		prefix := "  "
		if filteredIndex == s.selected {
			prefix = "› "
		}
		if s.multi {
			mark := "[ ] "
			if item.Checked {
				mark = "[x] "
			}
			prefix += mark
		}
		line := prefix + sanitizeDisplayText(item.Title)
		if item.Badge != "" {
			line += " " + s.theme.subtleStyle().Render("["+singleLine(item.Badge)+"]")
		}
		if item.Current {
			line += lipgloss.NewStyle().Foreground(s.theme.color(s.theme.Success)).Render(" ✓")
		}
		style := lipgloss.NewStyle().Foreground(s.theme.color(s.theme.Foreground))
		if filteredIndex == s.selected {
			style = lipgloss.NewStyle().Bold(true).Foreground(s.theme.color(s.theme.Primary))
		}
		lines = append(lines, Truncate(style.Render(line), width, "…", false))
	}
	return lines
}

func (s *selectorModel) emptyLabel() string {
	if strings.TrimSpace(s.Query()) != "" {
		return "No matching items"
	}
	switch s.kind {
	case selectorModels:
		return "No available models"
	case selectorSessions:
		return "No sessions found"
	case selectorThinking:
		return "No supported thinking levels"
	case selectorTools:
		return "No tools available"
	case selectorSettings:
		return "No settings available"
	case selectorTree:
		return "No session entries"
	case selectorFork:
		return "No user messages to fork from"
	case selectorLoginProvider:
		return "No providers support interactive login"
	case selectorLogoutProvider:
		return "No stored provider credentials"
	default:
		return "No items available"
	}
}

func (s *selectorModel) Cursor() *tea.Cursor {
	if s == nil || !s.searchable || !s.searchVisible {
		return nil
	}
	cursor := s.input.Cursor()
	if cursor == nil {
		return nil
	}
	// Rounded border + horizontal padding, then the title row.
	cursor.Position.X += 2
	cursor.Position.Y += 2
	return cursor
}
