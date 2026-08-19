package tui

import (
	"image/color"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestThemeSettingAndTerminalDetection(t *testing.T) {
	for input, want := range map[string]string{
		"": ThemeAuto, "auto": ThemeAuto, "light/dark": ThemeAuto,
		"dark": ThemeDark, "pi-dark": ThemeDark, "LIGHT": ThemeLight,
	} {
		got, err := ParseThemeSetting(input)
		if err != nil || got != want {
			t.Fatalf("ParseThemeSetting(%q) = %q, %v", input, got, err)
		}
	}
	if _, err := ParseThemeSetting("missing"); err == nil {
		t.Fatal("unsupported theme was accepted")
	}
	if !detectLightTerminal([]string{"COLORFGBG=0;15"}) || detectLightTerminal([]string{"COLORFGBG=15;0"}) {
		t.Fatal("COLORFGBG background was not resolved from its final value")
	}
}

func TestAutomaticThemeFollowsOSCBackground(t *testing.T) {
	model := newModelForTest(t)
	model.themeSetting, model.themeAuto = ThemeAuto, true
	model.Update(tea.BackgroundColorMsg{Color: color.RGBA{R: 255, G: 255, B: 255, A: 255}})
	if !model.theme.IsLight || model.renderer.theme.ID != LightTheme().ID {
		t.Fatalf("light OSC theme = %#v", model.theme)
	}
	model.Update(tea.BackgroundColorMsg{Color: color.RGBA{A: 255}})
	if model.theme.IsLight || model.renderer.theme.ID != DefaultTheme().ID {
		t.Fatalf("dark OSC theme = %#v", model.theme)
	}
}
