package tui

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"
)

const (
	defaultToolPreviewLines = 10
	bashToolPreviewLines    = 5
	grepToolPreviewLines    = 15
	listToolPreviewLines    = 20
	editToolPreviewLines    = 24
)

type toolPreviewPolicy struct {
	lines int
	tail  bool
}

func toolOnlyItem(item contentItem) bool {
	if item.Role != contentRoleTool || len(item.Blocks) == 0 {
		return false
	}
	found := false
	for _, block := range item.Blocks {
		switch block.Kind {
		case contentBlockToolCall, contentBlockToolResult:
			found = true
		case contentBlockImage, contentBlockNotice:
		default:
			return false
		}
	}
	return found
}

func (r *contentRenderer) renderToolExecution(call contentBlock, result []contentBlock, width int) []string {
	width = max(1, width)
	name := call.ToolName
	if name == "" && len(result) != 0 {
		name = result[0].ToolName
	}
	if name == "" {
		name = "tool"
	}
	failed, live := call.IsError, call.Live
	for _, block := range result {
		failed = failed || block.IsError
		live = live || block.Live
	}
	marker := "○"
	if live {
		marker = "●"
	} else if failed {
		marker = "×"
	} else if len(result) != 0 {
		marker = "✓"
	}
	style := r.theme.toolStyle()
	if failed {
		style = r.theme.errorStyle()
	}
	arguments := sanitizeDisplayText(call.Text)
	argumentObject := decodeToolObject(arguments)
	header := marker + " " + toolHeader(name, arguments, argumentObject)
	lines := []string{Truncate(style.Bold(true).Render(header), width, "…", false)}

	if r.toolsExpanded && strings.TrimSpace(arguments) != "" && shouldRenderExpandedArguments(name, arguments) {
		lines = append(lines, r.theme.subtleStyle().Render("arguments"))
		lines = append(lines, r.renderCode(arguments, width, false)...)
	} else if name == "write" {
		if content := objectString(argumentObject, "content"); content != "" {
			preview, hidden := r.renderToolTextPreview(content, width, defaultToolPreviewLines, false, false)
			lines = append(lines, preview...)
			lines = appendToolHiddenNotice(lines, hidden, r.theme)
		}
	}

	text, details, images, notices := collectToolResult(result)
	policy := previewPolicyForTool(name)
	if name == "edit" && !failed {
		if diff := detailString(details, "diff"); diff != "" {
			preview, hidden := r.renderDiffPreview(diff, width, editToolPreviewLines)
			lines = append(lines, preview...)
			lines = appendToolHiddenNotice(lines, hidden, r.theme)
			if !r.toolsExpanded {
				text = ""
			}
		}
	}
	if name == "read" && !failed && !r.toolsExpanded {
		if count := visualLineCount(text, max(1, width-2)); count > 0 {
			lines = append(lines, r.theme.subtleStyle().Render(fmt.Sprintf("… %d lines hidden", count)))
		}
		text = ""
	}
	if (name == "write" || name == "edit") && !failed && !r.toolsExpanded {
		text = ""
	}
	if strings.TrimSpace(text) != "" {
		limit := policy.lines
		if r.toolsExpanded {
			limit = 0
		}
		preview, hidden := r.renderToolTextPreview(text, width, limit, policy.tail, failed)
		lines = append(lines, preview...)
		lines = appendToolHiddenNotice(lines, hidden, r.theme)
	} else if live && len(lines) == 1 {
		lines = append(lines, r.theme.subtleStyle().Render("│ …"))
	}
	for _, image := range images {
		if rendered := renderTerminalImage(image, width, r.imageProtocol); len(rendered) != 0 {
			lines = append(lines, rendered...)
			continue
		}
		detail := sanitizeDisplayText(image.MediaType)
		if detail == "" {
			detail = "image"
		}
		if image.ByteSize > 0 {
			detail += fmt.Sprintf(" · %s", formatBytes(image.ByteSize))
		}
		lines = append(lines, r.theme.mutedStyle().Render("▧ "+detail))
	}
	for _, notice := range notices {
		noticeStyle := r.theme.mutedStyle()
		if notice.IsError {
			noticeStyle = r.theme.errorStyle()
		}
		lines = append(lines, styleLines(noticeStyle, Wrap(sanitizeDisplayText(notice.Text), width))...)
	}
	lines = append(lines, r.toolDetailNotices(details, width)...)
	return lines
}

func previewPolicyForTool(name string) toolPreviewPolicy {
	switch strings.ToLower(name) {
	case "bash":
		return toolPreviewPolicy{lines: bashToolPreviewLines, tail: true}
	case "grep":
		return toolPreviewPolicy{lines: grepToolPreviewLines}
	case "find", "ls":
		return toolPreviewPolicy{lines: listToolPreviewLines}
	case "read":
		return toolPreviewPolicy{lines: defaultToolPreviewLines}
	default:
		return toolPreviewPolicy{lines: defaultToolPreviewLines}
	}
}

func toolHeader(name, rawArguments string, arguments map[string]any) string {
	name = sanitizeDisplayText(name)
	path := objectString(arguments, "path", "file_path")
	shortPath := shortenToolPath(path)
	switch strings.ToLower(name) {
	case "bash":
		command := objectString(arguments, "command")
		if command == "" && arguments == nil {
			command = strings.TrimSpace(rawArguments)
		}
		if command == "" {
			return "$ bash"
		}
		return "$ " + singleLine(command)
	case "read":
		label := "read"
		if shortPath != "" {
			label += " " + shortPath
		}
		if offset := objectNumber(arguments, "offset"); offset != "" {
			label += ":" + offset
		}
		return label
	case "write", "edit":
		if shortPath == "" {
			return name
		}
		return name + " " + shortPath
	case "grep":
		pattern := objectString(arguments, "pattern")
		label := "grep"
		if pattern != "" {
			label += " /" + singleLine(pattern) + "/"
		}
		if shortPath != "" {
			label += " " + shortPath
		}
		return label
	case "find":
		pattern := objectString(arguments, "pattern")
		label := "find"
		if pattern != "" {
			label += " " + singleLine(pattern)
		}
		if shortPath != "" {
			label += " in " + shortPath
		}
		return label
	case "ls":
		if shortPath == "" {
			return "ls"
		}
		return "ls " + shortPath
	default:
		if arguments == nil || len(arguments) == 0 {
			return name
		}
		return name + " " + compactToolArguments(arguments)
	}
}

func shouldRenderExpandedArguments(name, arguments string) bool {
	if strings.EqualFold(name, "bash") && decodeToolObject(arguments) == nil {
		return false
	}
	return true
}

func collectToolResult(blocks []contentBlock) (string, json.RawMessage, []contentBlock, []contentBlock) {
	var text []string
	var details json.RawMessage
	var images []contentBlock
	var notices []contentBlock
	for _, block := range blocks {
		if len(details) == 0 && len(block.ToolDetails) != 0 {
			details = append(json.RawMessage(nil), block.ToolDetails...)
		}
		switch block.Kind {
		case contentBlockToolResult, contentBlockCode, contentBlockText:
			text = append(text, block.Text)
		case contentBlockImage:
			images = append(images, block)
		case contentBlockNotice:
			notices = append(notices, block)
		}
	}
	return strings.Join(text, "\n"), details, images, notices
}

func (r *contentRenderer) renderToolTextPreview(text string, width, limit int, tail, failed bool) ([]string, int) {
	text = sanitizeDisplayText(text)
	bodyWidth := max(1, width-2)
	visual := make([]string, 0)
	for _, physical := range splitPhysicalLines(strings.TrimSuffix(text, "\n")) {
		visual = append(visual, Wrap(physical, bodyWidth)...)
	}
	if len(visual) == 0 {
		return nil, 0
	}
	hidden := 0
	if limit > 0 && len(visual) > limit {
		hidden = len(visual) - limit
		if tail {
			visual = visual[len(visual)-limit:]
		} else {
			visual = visual[:limit]
		}
	}
	style := lipgloss.NewStyle().Foreground(r.theme.color(r.theme.Muted))
	if failed {
		style = r.theme.errorStyle()
	}
	lines := make([]string, 0, len(visual))
	for _, line := range visual {
		lines = append(lines, style.Render("│ "+line))
	}
	return lines, hidden
}

func (r *contentRenderer) renderDiffPreview(diff string, width, limit int) ([]string, int) {
	diff = sanitizeDisplayText(diff)
	bodyWidth := max(1, width-2)
	visual := make([]string, 0)
	for _, physical := range splitPhysicalLines(strings.TrimSuffix(diff, "\n")) {
		visual = append(visual, Wrap(physical, bodyWidth)...)
	}
	hidden := 0
	if !r.toolsExpanded && limit > 0 && len(visual) > limit {
		hidden = len(visual) - limit
		visual = visual[:limit]
	}
	lines := make([]string, 0, len(visual))
	for _, line := range visual {
		style := r.theme.mutedStyle()
		switch {
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			style = lipgloss.NewStyle().Foreground(r.theme.color(r.theme.Success))
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			style = r.theme.errorStyle()
		}
		lines = append(lines, style.Render("│ "+line))
	}
	return lines, hidden
}

func (r *contentRenderer) toolDetailNotices(details json.RawMessage, width int) []string {
	object := decodeToolDetails(details)
	if object == nil {
		return nil
	}
	var notices []string
	if path := objectString(object, "fullOutputPath"); path != "" {
		notices = append(notices, "Full output: "+path)
	}
	if truncation, ok := object["truncation"].(map[string]any); ok && objectBool(truncation, "truncated") {
		outputLines := objectNumber(truncation, "outputLines")
		totalLines := objectNumber(truncation, "totalLines")
		if outputLines != "" && totalLines != "" {
			notices = append(notices, "Truncated: showing "+outputLines+" of "+totalLines+" lines")
		} else {
			notices = append(notices, "Output truncated")
		}
	}
	for _, key := range []struct {
		name  string
		label string
	}{
		{name: "matchLimitReached", label: "match limit reached"},
		{name: "resultLimitReached", label: "result limit reached"},
		{name: "entryLimitReached", label: "entry limit reached"},
	} {
		if value := objectNumber(object, key.name); value != "" {
			notices = append(notices, value+" "+key.label)
		}
	}
	if objectBool(object, "linesTruncated") {
		notices = append(notices, "Some matching lines were truncated")
	}
	result := make([]string, 0, len(notices))
	for _, notice := range notices {
		result = append(result, styleLines(r.theme.mutedStyle(), Wrap("["+sanitizeDisplayText(notice)+"]", width))...)
	}
	return result
}

func appendToolHiddenNotice(lines []string, hidden int, theme Theme) []string {
	if hidden <= 0 {
		return lines
	}
	return append(lines, theme.subtleStyle().Render(fmt.Sprintf("… %d more lines", hidden)))
}

func decodeToolObject(value string) map[string]any {
	var object map[string]any
	if json.Unmarshal([]byte(value), &object) != nil {
		return nil
	}
	return object
}

func decodeToolDetails(value json.RawMessage) map[string]any {
	if len(value) == 0 {
		return nil
	}
	var object map[string]any
	if json.Unmarshal(value, &object) != nil {
		return nil
	}
	return object
}

func detailString(value json.RawMessage, key string) string {
	return objectString(decodeToolDetails(value), key)
}

func objectString(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := object[key].(string); ok {
			return sanitizeDisplayText(value)
		}
	}
	return ""
}

func objectNumber(object map[string]any, key string) string {
	if object == nil {
		return ""
	}
	switch value := object[key].(type) {
	case float64:
		return fmt.Sprintf("%g", value)
	case json.Number:
		return string(value)
	case int:
		return fmt.Sprintf("%d", value)
	case uint64:
		return fmt.Sprintf("%d", value)
	default:
		return ""
	}
}

func objectBool(object map[string]any, key string) bool {
	value, _ := object[key].(bool)
	return value
}

func compactToolArguments(arguments map[string]any) string {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return ""
	}
	return singleLine(string(encoded))
}

func shortenToolPath(value string) string {
	value = sanitizeDisplayText(value)
	if value == "" {
		return ""
	}
	cleaned := filepath.Clean(value)
	if strings.HasPrefix(cleaned, "/") {
		return cleaned
	}
	return cleaned
}

func singleLine(value string) string {
	return strings.Join(strings.Fields(sanitizeDisplayText(value)), " ")
}

func visualLineCount(value string, width int) int {
	if strings.TrimSpace(value) == "" {
		return 0
	}
	count := 0
	for _, line := range splitPhysicalLines(strings.TrimSuffix(sanitizeDisplayText(value), "\n")) {
		count += len(Wrap(line, max(1, width)))
	}
	return count
}
