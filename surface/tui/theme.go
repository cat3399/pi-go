package tui

import (
	"fmt"
	"image/color"
	"math"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
)

// Theme uses semantic roles rather than component-local colors. The names are
// intentionally shared with the GUI/Web design vocabulary even though each
// surface maps them to its own rendering primitives.
type Theme struct {
	ID      string
	IsLight bool

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

const (
	ThemeAuto  = "auto"
	ThemeDark  = "dark"
	ThemeLight = "light"
)

func DefaultTheme() Theme {
	return Theme{
		ID: "pi-dark", Background: "#0b0d10", Foreground: "#d9dee7",
		Muted: "#8992a3", Subtle: "#596273", Border: "#343b48",
		Primary: "#8aadf4", Secondary: "#c6a0f6", Success: "#a6da95",
		Warning: "#eed49f", Danger: "#ed8796", User: "#91d7e3",
		Assistant: "#8aadf4", Tool: "#f5bde6",
	}
}

func LightTheme() Theme {
	return Theme{
		ID: "pi-light", IsLight: true, Background: "#eff1f5", Foreground: "#4c4f69",
		Muted: "#6c6f85", Subtle: "#9ca0b0", Border: "#9ca0b0",
		Primary: "#1e66f5", Secondary: "#8839ef", Success: "#40a02b",
		Warning: "#df8e1d", Danger: "#d20f39", User: "#179299",
		Assistant: "#1e66f5", Tool: "#ea76cb",
	}
}

// ParseThemeSetting accepts pi's built-in theme names and its light/dark auto
// pair. The latter is the persisted representation used by upstream pi.
func ParseThemeSetting(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", ThemeAuto, "light/dark":
		return ThemeAuto, nil
	case ThemeDark, "pi-dark":
		return ThemeDark, nil
	case ThemeLight, "pi-light":
		return ThemeLight, nil
	default:
		return "", fmt.Errorf("unsupported theme %q (expected auto, dark, or light)", value)
	}
}

func persistedThemeSetting(value string) string {
	if value == ThemeAuto {
		return "light/dark"
	}
	return value
}

func themeForSetting(setting string, environment []string) Theme {
	switch setting {
	case ThemeLight:
		return LightTheme()
	case ThemeAuto:
		if detectLightTerminal(environment) {
			return LightTheme()
		}
	}
	return DefaultTheme()
}

// COLORFGBG provides a no-round-trip first render. Bubble Tea's OSC 11 result
// supersedes it as soon as BackgroundColorMsg arrives.
func detectLightTerminal(environment []string) bool {
	parts := strings.Split(environmentValue(environment, "COLORFGBG"), ";")
	for index := len(parts) - 1; index >= 0; index-- {
		value, err := strconv.Atoi(strings.TrimSpace(parts[index]))
		if err != nil || value < 0 || value > 255 {
			continue
		}
		return isLightColor(lipgloss.ANSIColor(value))
	}
	return false
}

func isLightColor(value color.Color) bool {
	if value == nil {
		return false
	}
	r, g, b, _ := value.RGBA()
	linear := func(channel uint32) float64 {
		component := float64(channel) / 65535
		if component <= 0.03928 {
			return component / 12.92
		}
		return math.Pow((component+0.055)/1.055, 2.4)
	}
	return 0.2126*linear(r)+0.7152*linear(g)+0.0722*linear(b) >= 0.5
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
