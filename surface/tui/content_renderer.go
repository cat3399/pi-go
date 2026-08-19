package tui

import (
	"fmt"
	"strings"

	"charm.land/glamour/v2"
	glamourstyles "charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
)

type itemRenderer interface {
	Render(contentItem, int) []string
	CacheKey() string
}

type contentRenderer struct {
	theme           Theme
	markdown        map[int]*glamour.TermRenderer
	toolsExpanded   bool
	thinkingVisible bool
	imageProtocol   terminalImageProtocol
}

func newContentRenderer(theme Theme) *contentRenderer {
	return &contentRenderer{theme: theme, markdown: make(map[int]*glamour.TermRenderer), thinkingVisible: true}
}

func (r *contentRenderer) CacheKey() string {
	if r == nil {
		return ""
	}
	return fmt.Sprintf("%s:tools=%t:thinking=%t:images=%d", r.theme.ID, r.toolsExpanded, r.thinkingVisible, r.imageProtocol)
}

func (r *contentRenderer) SetToolsExpanded(expanded bool) {
	if r != nil {
		r.toolsExpanded = expanded
	}
}

func (r *contentRenderer) SetThinkingVisible(visible bool) {
	if r != nil {
		r.thinkingVisible = visible
	}
}

func (r *contentRenderer) SetImageProtocol(protocol terminalImageProtocol) {
	if r != nil {
		r.imageProtocol = protocol
	}
}

func (r *contentRenderer) SetTheme(theme Theme) {
	if r == nil {
		return
	}
	r.theme = theme
	r.markdown = make(map[int]*glamour.TermRenderer)
}

func (r *contentRenderer) Render(item contentItem, width int) []string {
	if width <= 0 {
		return []string{""}
	}
	if r == nil {
		return Wrap(item.Title, width)
	}
	title := sanitizeDisplayText(item.Title)
	if title == "" {
		title = "Message"
	}
	if item.Live {
		title += "  " + r.theme.mutedStyle().Render("streaming")
	}
	lines := make([]string, 0)
	if !toolOnlyItem(item) {
		lines = append(lines, r.theme.titleStyle(item.Role, item.Failed).Render(title))
	}
	bodyWidth := max(1, width-2)
	for index := 0; index < len(item.Blocks); index++ {
		block := item.Blocks[index]
		if block.Kind == contentBlockThinking && !r.thinkingVisible {
			continue
		}
		var blockLines []string
		if block.Kind == contentBlockToolCall {
			result := make([]contentBlock, 0)
			for index+1 < len(item.Blocks) {
				next := item.Blocks[index+1]
				if next.ToolCallID == "" || next.ToolCallID != block.ToolCallID || next.Kind == contentBlockToolCall {
					break
				}
				result = append(result, next)
				index++
			}
			blockLines = r.renderToolExecution(block, result, bodyWidth)
		} else if block.Kind == contentBlockToolResult {
			blockLines = r.renderToolExecution(contentBlock{ToolName: block.ToolName}, []contentBlock{block}, bodyWidth)
		} else {
			blockLines = r.renderBlock(block, bodyWidth)
		}
		for _, line := range blockLines {
			lines = append(lines, "  "+line)
		}
	}
	if len(item.Blocks) == 0 && item.Live {
		lines = append(lines, "  "+r.theme.mutedStyle().Render("…"))
	}
	if len(lines) == 0 {
		lines = append(lines, r.theme.titleStyle(item.Role, item.Failed).Render(title))
	}
	for index, line := range lines {
		if VisibleWidth(line) > width {
			lines[index] = Truncate(line, width, "…", false)
		}
	}
	return lines
}

func (r *contentRenderer) renderBlock(block contentBlock, width int) []string {
	if width <= 0 {
		return []string{""}
	}
	switch block.Kind {
	case contentBlockMarkdown:
		return r.renderMarkdown(block.Text, width)
	case contentBlockThinking:
		label := r.theme.mutedStyle().Italic(true).Render("Thinking")
		lines := []string{label}
		for _, line := range Wrap(sanitizeDisplayText(block.Text), max(1, width-2)) {
			lines = append(lines, r.theme.mutedStyle().Render("│ "+line))
		}
		return lines
	case contentBlockToolCall:
		return r.renderToolExecution(block, nil, width)
	case contentBlockToolResult:
		return r.renderToolExecution(contentBlock{ToolName: block.ToolName}, []contentBlock{block}, width)
	case contentBlockCode:
		return r.renderCode(block.Text, width, block.IsError)
	case contentBlockImage:
		if lines := renderTerminalImage(block, width, r.imageProtocol); len(lines) != 0 {
			return lines
		}
		detail := sanitizeDisplayText(block.MediaType)
		if detail == "" {
			detail = "image"
		}
		if block.ByteSize > 0 {
			detail += fmt.Sprintf(" · %s", formatBytes(block.ByteSize))
		}
		return []string{r.theme.mutedStyle().Render("▧ " + detail)}
	case contentBlockNotice:
		style := r.theme.mutedStyle()
		if block.IsError {
			style = r.theme.errorStyle()
		}
		return styleLines(style, Wrap(sanitizeDisplayText(block.Text), width))
	case contentBlockText:
		fallthrough
	default:
		return Wrap(sanitizeDisplayText(block.Text), width)
	}
}

func (r *contentRenderer) renderMarkdown(text string, width int) []string {
	text = sanitizeDisplayText(text)
	if strings.TrimSpace(text) == "" {
		return []string{""}
	}
	renderer := r.markdown[width]
	if renderer == nil {
		style := glamourstyles.DarkStyle
		if r.theme.IsLight {
			style = glamourstyles.LightStyle
		}
		created, err := glamour.NewTermRenderer(
			glamour.WithStandardStyle(style),
			glamour.WithWordWrap(width),
			glamour.WithTableWrap(true),
		)
		if err == nil {
			renderer = created
			r.markdown[width] = renderer
		}
	}
	if renderer == nil {
		return Wrap(text, width)
	}
	rendered, err := renderer.Render(text)
	if err != nil {
		return Wrap(text, width)
	}
	return normalizeBlockLines(rendered, width)
}

func (r *contentRenderer) renderCode(text string, width int, failed bool) []string {
	text = sanitizeDisplayText(text)
	style := lipgloss.NewStyle().Foreground(r.theme.color(r.theme.Muted))
	if failed {
		style = lipgloss.NewStyle().Foreground(r.theme.color(r.theme.Danger))
	}
	wrapped := make([]string, 0)
	for _, physical := range splitPhysicalLines(strings.TrimSuffix(text, "\n")) {
		for _, line := range Wrap(physical, max(1, width-2)) {
			wrapped = append(wrapped, style.Render("│ "+line))
		}
	}
	if len(wrapped) == 0 {
		wrapped = append(wrapped, style.Render("│"))
	}
	return wrapped
}

func normalizeBlockLines(value string, width int) []string {
	value = strings.Trim(value, "\n")
	if value == "" {
		return []string{""}
	}
	result := make([]string, 0)
	for _, physical := range splitPhysicalLines(value) {
		result = append(result, Wrap(physical, width)...)
	}
	for len(result) > 1 && strings.TrimSpace(StripTerminalSequences(result[0])) == "" {
		result = result[1:]
	}
	for len(result) > 1 && strings.TrimSpace(StripTerminalSequences(result[len(result)-1])) == "" {
		result = result[:len(result)-1]
	}
	return result
}

func styleLines(style lipgloss.Style, lines []string) []string {
	result := make([]string, len(lines))
	for index, line := range lines {
		result[index] = style.Render(line)
	}
	return result
}

func formatBytes(value int) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	units := []string{"KiB", "MiB", "GiB"}
	number := float64(value)
	for _, suffix := range units {
		number /= unit
		if number < unit {
			return fmt.Sprintf("%.1f %s", number, suffix)
		}
	}
	return fmt.Sprintf("%.1f TiB", number/unit)
}
