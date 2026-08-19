package tui

import (
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
)

type keybindingTestAPI struct {
	modelTestAPI
	dir string
}

func (a keybindingTestAPI) AgentDir() string { return a.dir }

func TestKeybindingsConfigOverridesAndDisablesDefaults(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "keybindings.json"), []byte(`{
  "app.model.select": "alt+m",
  "app.model.cycleForward": []
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bindings, err := loadAppKeybindings(directory)
	if err != nil {
		t.Fatal(err)
	}
	if !bindings.Matches(keyModelSelect, "alt+m") || bindings.Matches(keyModelSelect, "ctrl+l") {
		t.Fatalf("model select bindings = %#v", bindings.bindings[keyModelSelect])
	}
	if bindings.Matches(keyModelForward, "ctrl+p") {
		t.Fatal("empty binding array did not disable model cycling")
	}
}

func TestModelUsesConfiguredApplicationShortcut(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "keybindings.json"), []byte(`{
  "app.model.select": "alt+m"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	model := newModelWithAPIForTest(t, keybindingTestAPI{dir: directory})
	if handled, _ := model.handleKey(tea.KeyPressMsg(tea.Key{Code: 'l', Mod: tea.ModCtrl})); handled {
		t.Fatal("overridden default shortcut remained active")
	}
	if handled, command := model.handleKey(tea.KeyPressMsg(tea.Key{Code: 'm', Mod: tea.ModAlt})); !handled || command == nil {
		t.Fatal("configured model selector shortcut was not active")
	}
}

func TestInvalidKeybindingsReloadPreservesLastHealthyBindings(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "keybindings.json")
	if err := os.WriteFile(path, []byte(`{"app.model.select":"alt+m"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	model := newModelWithAPIForTest(t, keybindingTestAPI{dir: directory})
	if err := os.WriteFile(path, []byte(`{"app.model.select":"not+a+binding"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := model.reloadKeybindings(); err == nil {
		t.Fatal("invalid keybinding reload succeeded")
	}
	if !model.keybindings.Matches(keyModelSelect, "alt+m") {
		t.Fatal("invalid reload replaced last healthy keybindings")
	}
}
