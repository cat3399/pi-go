package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestComposerUndoCoalescesWords(t *testing.T) {
	composer := newComposerModel(DefaultTheme())
	composer.SetKeybindings(defaultAppKeybindings())
	_ = composer.Init()
	for _, char := range "hello world" {
		composer.Update(tea.KeyPressMsg(tea.Key{Code: char, Text: string(char)}))
	}
	if composer.Value() != "hello world" {
		t.Fatalf("typed value = %q", composer.Value())
	}
	composer.HandleEditingKey(keyPress('-', tea.ModCtrl), defaultAppKeybindings())
	if composer.Value() != "hello" {
		t.Fatalf("first undo = %q", composer.Value())
	}
	composer.HandleEditingKey(keyPress('-', tea.ModCtrl), defaultAppKeybindings())
	if composer.Value() != "" {
		t.Fatalf("second undo = %q", composer.Value())
	}
}

func TestComposerKillRingYankAndYankPop(t *testing.T) {
	bindings := defaultAppKeybindings()
	composer := newComposerModel(DefaultTheme())
	composer.SetKeybindings(bindings)
	composer.SetDraft("alpha beta", nil)
	composer.input.MoveToEnd()
	composer.HandleEditingKey(keyPress('w', tea.ModCtrl), bindings)
	if composer.Value() != "alpha " {
		t.Fatalf("word kill = %q", composer.Value())
	}
	composer.HandleEditingKey(keyPress('y', tea.ModCtrl), bindings)
	if composer.Value() != "alpha beta" {
		t.Fatalf("yank = %q", composer.Value())
	}

	composer.SetDraft("one two", nil)
	composer.input.MoveToEnd()
	composer.HandleEditingKey(keyPress('w', tea.ModCtrl), bindings)
	composer.HandleEditingKey(keyPress('y', tea.ModCtrl), bindings)
	composer.HandleEditingKey(keyPress('y', tea.ModAlt), bindings)
	if composer.Value() != "one beta" {
		t.Fatalf("yank pop = %q", composer.Value())
	}
}

func TestComposerCharacterJumpSearchesAcrossLines(t *testing.T) {
	bindings := defaultAppKeybindings()
	composer := newComposerModel(DefaultTheme())
	composer.SetKeybindings(bindings)
	composer.SetDraft("abc\naxc", nil)
	composer.input.MoveToBegin()
	composer.HandleEditingKey(keyPress(']', tea.ModCtrl), bindings)
	composer.HandleEditingKey(tea.KeyPressMsg(tea.Key{Code: 'x', Text: "x"}), bindings)
	if composer.CursorOffset() != 5 {
		t.Fatalf("forward jump offset = %d", composer.CursorOffset())
	}
	composer.input.MoveToEnd()
	composer.HandleEditingKey(keyPress(']', tea.ModCtrl|tea.ModAlt), bindings)
	composer.HandleEditingKey(tea.KeyPressMsg(tea.Key{Code: 'a', Text: "a"}), bindings)
	if composer.CursorOffset() != 4 {
		t.Fatalf("backward jump offset = %d", composer.CursorOffset())
	}
}

func keyPress(code rune, modifiers tea.KeyMod) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: code, Mod: modifiers})
}
