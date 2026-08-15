package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"
)

func (m *Model) renderStateLine(width int) string {
	width = max(1, width)
	left, color := m.stateLineLeft(width)
	right := m.stateLineRight(width)
	left = lipgloss.NewStyle().Foreground(m.theme.color(color)).Render(left)
	right = m.theme.subtleStyle().Render(right)
	return joinStateLine(left, right, width)
}

func (m *Model) stateLineLeft(width int) (string, string) {
	if text := sanitizeDisplayText(strings.TrimSpace(m.status.text)); text != "" {
		marker := "•"
		if m.busy() {
			marker = "●"
		}
		return marker + " " + singleLine(text), statusLevelColor(m.theme, m.status.level)
	}
	if m.busy() {
		return "● " + sanitizeDisplayText(m.state.Phase.String()), m.theme.Warning
	}
	cwd := m.state.CWD
	if cwd == "" {
		cwd = m.api.DefaultCWD()
	}
	path := formatStateLineCWD(cwd, width >= 90)
	sessionName := shortID(m.sessionID)
	if m.state.SessionName != nil && strings.TrimSpace(*m.state.SessionName) != "" {
		sessionName = sanitizeDisplayText(strings.TrimSpace(*m.state.SessionName))
	}
	left := path
	if sessionName != "" {
		left += " • " + sessionName
	}
	if !m.transcript.Following() {
		left = "↑ scrolled • " + left
	}
	return left, m.theme.Muted
}

func (m *Model) stateLineRight(width int) string {
	parts := make([]string, 0, 5)
	if m.state.RetryWaiting {
		parts = append(parts, fmt.Sprintf("retry %d", m.state.RetryAttempt))
	}
	if m.state.PendingMessageCount > 0 {
		parts = append(parts, fmt.Sprintf("queue %d", m.state.PendingMessageCount))
	}
	if m.state.ContextUsage != nil && m.state.ContextUsage.Percent != nil {
		parts = append(parts, fmt.Sprintf("ctx %.1f%%", *m.state.ContextUsage.Percent))
	}
	if width >= 48 {
		model := "no model"
		if m.state.HasModel {
			model = m.state.Model.ID()
			if width >= 110 {
				model = m.state.Model.Provider() + "/" + model
			}
		}
		parts = append(parts, sanitizeDisplayText(model))
	}
	if width >= 82 && m.state.ThinkingLevel.Valid() {
		parts = append(parts, "thinking "+sanitizeDisplayText(string(m.state.ThinkingLevel)))
	}
	return strings.Join(parts, " • ")
}

func (m *Model) renderQueueDock(width, maxLines int) []string {
	if maxLines <= 0 || m.state.PendingMessageCount <= 0 {
		return nil
	}
	type queuedLine struct {
		kind string
		text string
	}
	queued := make([]queuedLine, 0, m.state.PendingMessageCount)
	for _, message := range m.state.QueuedMessages.Steering {
		queued = append(queued, queuedLine{kind: "steer", text: message})
	}
	for _, message := range m.state.QueuedMessages.FollowUp {
		queued = append(queued, queuedLine{kind: "follow-up", text: message})
	}
	for len(queued) < m.state.PendingMessageCount {
		queued = append(queued, queuedLine{kind: "queued", text: "[rich message]"})
	}
	style := lipgloss.NewStyle().Foreground(m.theme.color(m.theme.Warning))
	if len(queued) <= maxLines {
		lines := make([]string, 0, len(queued))
		for _, message := range queued {
			if strings.TrimSpace(message.text) == "" {
				message.text = "[rich message]"
			}
			text := "↳ " + message.kind + ": " + singleLine(message.text)
			lines = append(lines, Truncate(style.Render(text), width, "…", false))
		}
		return lines
	}
	if maxLines == 1 {
		return []string{Truncate(style.Render(fmt.Sprintf("↳ %d queued messages", len(queued))), width, "…", false)}
	}
	lines := make([]string, 0, maxLines)
	for _, message := range queued[:maxLines-1] {
		if strings.TrimSpace(message.text) == "" {
			message.text = "[rich message]"
		}
		text := "↳ " + message.kind + ": " + singleLine(message.text)
		lines = append(lines, Truncate(style.Render(text), width, "…", false))
	}
	lines = append(lines, Truncate(
		m.theme.subtleStyle().Render(fmt.Sprintf("↳ … %d more queued", len(queued)-len(lines))),
		width, "…", false,
	))
	return lines
}

func statusLevelColor(theme Theme, level statusLevel) string {
	switch level {
	case statusSuccess:
		return theme.Success
	case statusWarning:
		return theme.Warning
	case statusError:
		return theme.Danger
	default:
		return theme.Muted
	}
}

func formatStateLineCWD(cwd string, expanded bool) string {
	cwd = sanitizeDisplayText(cwd)
	if !expanded {
		base := filepath.Base(cwd)
		if base != "." && base != string(filepath.Separator) && base != "" {
			return base
		}
		return cwd
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return cwd
	}
	relative, err := filepath.Rel(home, cwd)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return cwd
	}
	if relative == "." {
		return "~"
	}
	return "~" + string(filepath.Separator) + relative
}

func joinStateLine(left, right string, width int) string {
	if width <= 0 {
		return ""
	}
	if VisibleWidth(right) == 0 {
		return Truncate(left, width, "…", true)
	}
	rightBudget := min(VisibleWidth(right), max(8, width/2))
	right = Truncate(right, rightBudget, "…", false)
	rightWidth := VisibleWidth(right)
	leftBudget := max(0, width-rightWidth-1)
	left = Truncate(left, leftBudget, "…", false)
	padding := max(1, width-VisibleWidth(left)-rightWidth)
	return Truncate(left+strings.Repeat(" ", padding)+right, width, "", true)
}
