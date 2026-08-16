package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func TestSelectorFiltersRanksAndNavigatesModels(t *testing.T) {
	selector := newSelectorModel(DefaultTheme(), selectorModels, "Select model", "", true, false)
	selector.SetItems([]selectorItem{
		{Key: "openrouter/anthropic/claude-sonnet", Title: "anthropic/claude-sonnet", Badge: "openrouter", Description: "Claude Sonnet"},
		{Key: "deepseek/deepseek-v4-flash", Title: "deepseek-v4-flash", Badge: "deepseek", Description: "DeepSeek V4 Flash", Current: true},
		{Key: "openai/gpt-5", Title: "gpt-5", Badge: "openai", Description: "GPT-5"},
	}, "")
	selected, ok := selector.Selected()
	if !ok || selected.Key != "deepseek/deepseek-v4-flash" {
		t.Fatalf("initial selection = %#v, %t", selected, ok)
	}

	selector.Move(1)
	selected, _ = selector.Selected()
	if selected.Key != "openai/gpt-5" {
		t.Fatalf("selection after move = %#v", selected)
	}
	selector.Move(1)
	selected, _ = selector.Selected()
	if selected.Key != "openrouter/anthropic/claude-sonnet" {
		t.Fatalf("wrapped selection = %#v", selected)
	}

	selector.input.SetValue("or sonnet")
	selector.refilter(true)
	selected, ok = selector.Selected()
	if !ok || len(selector.filtered) != 1 || selected.Key != "openrouter/anthropic/claude-sonnet" {
		t.Fatalf("filtered selection = %#v, matches=%#v", selected, selector.filtered)
	}
}

func TestSelectorRanksCanonicalReferenceBeforeProxyModelID(t *testing.T) {
	selector := newSelectorModel(DefaultTheme(), selectorModels, "Select model", "", true, false)
	selector.SetItems([]selectorItem{
		{Key: "openrouter/openai/gpt-5", Title: "openai/gpt-5", Badge: "openrouter", Current: true},
		{Key: "openai/gpt-5", Title: "gpt-5", Badge: "openai"},
	}, "")
	selector.input.SetValue("openai/gpt-5")
	selector.refilter(true)
	selected, ok := selector.Selected()
	if !ok || selected.Key != "openai/gpt-5" {
		t.Fatalf("canonical query selected %#v, %t", selected, ok)
	}
}

func TestSelectorMultiSelectKeepsOriginalItemOrder(t *testing.T) {
	selector := newSelectorModel(DefaultTheme(), selectorTools, "Configure tools", "", true, true)
	selector.SetItems([]selectorItem{
		{Key: "read", Title: "read", Checked: true},
		{Key: "edit", Title: "edit"},
		{Key: "bash", Title: "bash", Checked: true},
	}, "")
	selector.Move(1)
	if !selector.ToggleSelected() {
		t.Fatal("toggle was not handled")
	}
	if got := selector.CheckedKeys(); strings.Join(got, ",") != "read,edit,bash" {
		t.Fatalf("checked keys = %#v", got)
	}
}

func TestSelectorViewIsBoundedAndSanitizesItems(t *testing.T) {
	selector := newSelectorModel(DefaultTheme(), selectorSessions, "Resume session", "", true, false)
	selector.SetItems([]selectorItem{{
		Key: "session-1", Title: "unsafe\x1b]52;c;payload\a", Badge: "workspace\nother", Description: "first message",
	}}, "warning\x1b[31m")
	_ = selector.Focus()
	view := selector.View(48, 8)
	if width := lipgloss.Width(view); width != 48 {
		t.Fatalf("selector width = %d, want 48:\n%s", width, view)
	}
	if height := lipgloss.Height(view); height > 8 {
		t.Fatalf("selector height = %d:\n%s", height, view)
	}
	plain := StripTerminalSequences(view)
	if strings.Contains(plain, "]52") || strings.Contains(plain, "[31m") || !strings.Contains(plain, "unsafe") {
		t.Fatalf("unsafe selector view: %q", plain)
	}
	cursor := selector.Cursor()
	if cursor == nil || cursor.Position.X < 2 || cursor.Position.Y != 2 {
		t.Fatalf("selector cursor = %#v", cursor)
	}
}

func TestSelectorHidesCursorWhenTinyLayoutOmitsSearch(t *testing.T) {
	selector := newSelectorModel(DefaultTheme(), selectorModels, "Select model", "", true, false)
	_ = selector.Focus()
	_ = selector.View(30, 3)
	if cursor := selector.Cursor(); cursor != nil {
		t.Fatalf("hidden search exposed cursor %#v", cursor)
	}
	_ = selector.View(30, 4)
	if cursor := selector.Cursor(); cursor == nil {
		t.Fatal("visible search omitted cursor")
	}
}

func TestSelectorInitialLongQueryAlignsViewportAndCursor(t *testing.T) {
	selector := newSelectorModel(
		DefaultTheme(), selectorModels, "Select model", "a-very-long-model-query-that-must-scroll", true, false,
	)
	_ = selector.Focus()
	view := selector.View(20, 5)
	plain := StripTerminalSequences(view)
	if !strings.Contains(plain, "scroll") {
		t.Fatalf("initial query viewport did not follow the cursor:\n%s", plain)
	}
	cursor := selector.Cursor()
	if cursor == nil || cursor.Position.X != 18 || cursor.Position.Y != 2 {
		t.Fatalf("initial long-query cursor = %#v", cursor)
	}
}

func TestSelectorInputRefiltersOnPaste(t *testing.T) {
	selector := newSelectorModel(DefaultTheme(), selectorModels, "Select model", "", true, false)
	selector.SetItems([]selectorItem{
		{Key: "deepseek/model", Title: "model", Badge: "deepseek"},
		{Key: "openai/other", Title: "other", Badge: "openai"},
	}, "")
	_ = selector.Focus()
	selector.Update(tea.PasteMsg{Content: "deepseek"})
	selected, ok := selector.Selected()
	if !ok || len(selector.filtered) != 1 || selected.Key != "deepseek/model" {
		t.Fatalf("paste filter = %#v, matches=%#v", selected, selector.filtered)
	}
}
