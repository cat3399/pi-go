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
	theme    Theme
	markdown map[int]*glamour.TermRenderer
}

func newContentRenderer(theme Theme) *contentRenderer {
	return &contentRenderer{theme: theme, markdown: make(map[int]*glamour.TermRenderer)}
}

func (r *contentRenderer) CacheKey() string {
	if r == nil {
		return ""
	}
	return r.theme.ID
}

func (r *contentRenderer) Render(item contentItem, width int) []string {
	if width <= 0 {
		return []string{""}
	}
	if r == nil {
		return Wrap(item.Title, width)
	}
	title := item.Title
	if title == "" {
		title = "Message"
	}
	if item.Live {
		title += "  " + r.theme.mutedStyle().Render("streaming")
	}
	lines := []string{r.theme.titleStyle(item.Role, item.Failed).Render(title)}
	bodyWidth := max(1, width-2)
	for _, block := range item.Blocks {
		blockLines := r.renderBlock(block, bodyWidth)
		for _, line := range blockLines {
			lines = append(lines, "  "+line)
		}
	}
	if len(item.Blocks) == 0 && item.Live {
		lines = append(lines, "  "+r.theme.mutedStyle().Render("…"))
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
		for _, line := range Wrap(block.Text, max(1, width-2)) {
			lines = append(lines, r.theme.mutedStyle().Render("│ "+line))
		}
		return lines
	case contentBlockToolCall:
		name := block.ToolName
		if name == "" {
			name = "tool"
		}
		lines := []string{r.theme.toolStyle().Bold(true).Render("→ " + name)}
		if strings.TrimSpace(block.Text) != "" {
			lines = append(lines, r.renderCode(block.Text, width, false)...)
		}
		return lines
	case contentBlockToolResult:
		name := block.ToolName
		if name == "" {
			name = "result"
		}
		style := r.theme.toolStyle()
		if block.IsError {
			style = r.theme.errorStyle()
		}
		lines := []string{style.Bold(true).Render("← " + name)}
		lines = append(lines, r.renderCode(block.Text, width, block.IsError)...)
		return lines
	case contentBlockCode:
		return r.renderCode(block.Text, width, block.IsError)
	case contentBlockImage:
		detail := block.MediaType
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
		return styleLines(style, Wrap(block.Text, width))
	case contentBlockText:
		fallthrough
	default:
		return Wrap(block.Text, width)
	}
}

func (r *contentRenderer) renderMarkdown(text string, width int) []string {
	if strings.TrimSpace(text) == "" {
		return []string{""}
	}
	renderer := r.markdown[width]
	if renderer == nil {
		style := glamourstyles.DarkStyle
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
