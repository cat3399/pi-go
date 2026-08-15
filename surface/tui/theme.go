package tui

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// Theme uses semantic roles rather than component-local colors. The names are
// intentionally shared with the GUI/Web design vocabulary even though each
// surface maps them to its own rendering primitives.
type Theme struct {
	ID string

	Background string
	Foreground string
	Muted      string
	Subtle     string
	Border     string
	Primary    string
	Secondary  string
	Success    string
	Warning    string
	Danger     string
	User       string
	Assistant  string
	Tool       string
}

func DefaultTheme() Theme {
	return Theme{
		ID: "pi-dark", Background: "#0b0d10", Foreground: "#d9dee7",
		Muted: "#8992a3", Subtle: "#596273", Border: "#343b48",
		Primary: "#8aadf4", Secondary: "#c6a0f6", Success: "#a6da95",
		Warning: "#eed49f", Danger: "#ed8796", User: "#91d7e3",
		Assistant: "#8aadf4", Tool: "#f5bde6",
	}
}

func (t Theme) color(value string) color.Color { return lipgloss.Color(value) }

func (t Theme) titleStyle(role contentRole, failed bool) lipgloss.Style {
	color := t.Primary
	switch role {
	case contentRoleUser:
		color = t.User
	case contentRoleAssistant:
		color = t.Assistant
	case contentRoleTool:
		color = t.Tool
	case contentRoleSystem:
		color = t.Secondary
	}
	if failed {
		color = t.Danger
	}
	return lipgloss.NewStyle().Bold(true).Foreground(t.color(color))
}

func (t Theme) mutedStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(t.color(t.Muted))
}

func (t Theme) subtleStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(t.color(t.Subtle))
}

func (t Theme) errorStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(t.color(t.Danger))
}

func (t Theme) toolStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(t.color(t.Tool))
}
