package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	tea "charm.land/bubbletea/v2"
)

const (
	keyInterrupt             = "app.interrupt"
	keyClear                 = "app.clear"
	keyExit                  = "app.exit"
	keySuspend               = "app.suspend"
	keyThinkingCycle         = "app.thinking.cycle"
	keyThinkingToggle        = "app.thinking.toggle"
	keyModelSelect           = "app.model.select"
	keyModelForward          = "app.model.cycleForward"
	keyModelBackward         = "app.model.cycleBackward"
	keyToolsExpand           = "app.tools.expand"
	keyExternalEditor        = "app.editor.external"
	keyMessageCopy           = "app.message.copy"
	keyMessageFollowUp       = "app.message.followUp"
	keyMessageDequeue        = "app.message.dequeue"
	keyPasteImage            = "app.clipboard.pasteImage"
	keyViewportPageUp        = "tui.altScreen.pageUp"
	keyViewportPageDown      = "tui.altScreen.pageDown"
	keyViewportPrevious      = "tui.altScreen.previousPrompt"
	keyViewportNext          = "tui.altScreen.nextPrompt"
	keyViewportTop           = "tui.altScreen.top"
	keyViewportBottom        = "tui.altScreen.bottom"
	keyEditorCursorUp        = "tui.editor.cursorUp"
	keyEditorCursorDown      = "tui.editor.cursorDown"
	keyEditorCursorLeft      = "tui.editor.cursorLeft"
	keyEditorCursorRight     = "tui.editor.cursorRight"
	keyEditorWordLeft        = "tui.editor.cursorWordLeft"
	keyEditorWordRight       = "tui.editor.cursorWordRight"
	keyEditorLineStart       = "tui.editor.cursorLineStart"
	keyEditorLineEnd         = "tui.editor.cursorLineEnd"
	keyEditorJumpForward     = "tui.editor.jumpForward"
	keyEditorJumpBackward    = "tui.editor.jumpBackward"
	keyEditorPageUp          = "tui.editor.pageUp"
	keyEditorPageDown        = "tui.editor.pageDown"
	keyEditorDeleteBackward  = "tui.editor.deleteCharBackward"
	keyEditorDeleteForward   = "tui.editor.deleteCharForward"
	keyEditorDeleteWordBack  = "tui.editor.deleteWordBackward"
	keyEditorDeleteWordFront = "tui.editor.deleteWordForward"
	keyEditorDeleteLineStart = "tui.editor.deleteToLineStart"
	keyEditorDeleteLineEnd   = "tui.editor.deleteToLineEnd"
	keyEditorYank            = "tui.editor.yank"
	keyEditorYankPop         = "tui.editor.yankPop"
	keyEditorUndo            = "tui.editor.undo"
	keyInputNewLine          = "tui.input.newLine"
	keyInputSubmit           = "tui.input.submit"
	keyInputTab              = "tui.input.tab"
	keyInputCopy             = "tui.input.copy"
	keySelectUp              = "tui.select.up"
	keySelectDown            = "tui.select.down"
	keySelectPageUp          = "tui.select.pageUp"
	keySelectPageDown        = "tui.select.pageDown"
	keySelectConfirm         = "tui.select.confirm"
	keySelectCancel          = "tui.select.cancel"
	keySessionTogglePath     = "app.session.togglePath"
	keySessionToggleSort     = "app.session.toggleSort"
	keySessionToggleNamed    = "app.session.toggleNamedFilter"
	keySessionRename         = "app.session.rename"
	keySessionDelete         = "app.session.delete"
	keySessionNew            = "app.session.new"
	keySessionTree           = "app.session.tree"
	keySessionFork           = "app.session.fork"
	keySessionResume         = "app.session.resume"
)

type appKeybindings struct {
	bindings map[string][]string
}

func defaultAppKeybindings() appKeybindings {
	bindings := map[string][]string{
		keyInterrupt:             {"escape"},
		keyClear:                 {"ctrl+c"},
		keyExit:                  {"ctrl+d"},
		keySuspend:               nil,
		keyThinkingCycle:         {"shift+tab"},
		keyThinkingToggle:        {"ctrl+t"},
		keyModelSelect:           {"ctrl+l"},
		keyModelForward:          {"ctrl+p"},
		keyModelBackward:         {"shift+ctrl+p"},
		keyToolsExpand:           {"ctrl+o"},
		keyExternalEditor:        {"ctrl+g"},
		keyMessageCopy:           {"ctrl+x"},
		keyMessageFollowUp:       {"alt+enter"},
		keyMessageDequeue:        {"alt+up"},
		keyPasteImage:            {"ctrl+v"},
		keyViewportPageUp:        {"pageup", "ctrl+up"},
		keyViewportPageDown:      {"pagedown", "ctrl+down"},
		keyViewportPrevious:      {"ctrl+shift+up"},
		keyViewportNext:          {"ctrl+shift+down"},
		keyViewportTop:           {"ctrl+home"},
		keyViewportBottom:        {"ctrl+end"},
		keyEditorCursorUp:        {"up"},
		keyEditorCursorDown:      {"down"},
		keyEditorCursorLeft:      {"left", "ctrl+b"},
		keyEditorCursorRight:     {"right", "ctrl+f"},
		keyEditorWordLeft:        {"alt+left", "ctrl+left", "alt+b"},
		keyEditorWordRight:       {"alt+right", "ctrl+right", "alt+f"},
		keyEditorLineStart:       {"home", "ctrl+a"},
		keyEditorLineEnd:         {"end", "ctrl+e"},
		keyEditorJumpForward:     {"ctrl+]"},
		keyEditorJumpBackward:    {"ctrl+alt+]"},
		keyEditorPageUp:          {"pageup"},
		keyEditorPageDown:        {"pagedown"},
		keyEditorDeleteBackward:  {"backspace"},
		keyEditorDeleteForward:   {"delete", "ctrl+d"},
		keyEditorDeleteWordBack:  {"ctrl+w", "alt+backspace"},
		keyEditorDeleteWordFront: {"alt+d", "alt+delete"},
		keyEditorDeleteLineStart: {"ctrl+u"},
		keyEditorDeleteLineEnd:   {"ctrl+k"},
		keyEditorYank:            {"ctrl+y"},
		keyEditorYankPop:         {"alt+y"},
		keyEditorUndo:            {"ctrl+-"},
		keyInputNewLine:          {"shift+enter", "ctrl+j"},
		keyInputSubmit:           {"enter"},
		keyInputTab:              {"tab"},
		keyInputCopy:             {"ctrl+c"},
		keySelectUp:              {"up"},
		keySelectDown:            {"down"},
		keySelectPageUp:          {"pageup"},
		keySelectPageDown:        {"pagedown"},
		keySelectConfirm:         {"enter"},
		keySelectCancel:          {"escape", "ctrl+c"},
		keySessionTogglePath:     {"ctrl+p"},
		keySessionToggleSort:     {"ctrl+s"},
		keySessionToggleNamed:    {"ctrl+n"},
		keySessionRename:         {"ctrl+r", "ctrl+e"},
		keySessionDelete:         {"ctrl+d"},
		keySessionNew:            {},
		keySessionTree:           {},
		keySessionFork:           {},
		keySessionResume:         {},
	}
	if runtime.GOOS != "windows" {
		bindings[keySuspend] = []string{"ctrl+z"}
	}
	return appKeybindings{bindings: bindings}
}

func loadAppKeybindings(agentDir string) (appKeybindings, error) {
	bindings := defaultAppKeybindings()
	agentDir = strings.TrimSpace(agentDir)
	if agentDir == "" {
		return bindings, nil
	}
	data, err := os.ReadFile(filepath.Join(agentDir, "keybindings.json"))
	if errors.Is(err, os.ErrNotExist) {
		return bindings, nil
	}
	if err != nil {
		return bindings, err
	}
	var configured map[string]json.RawMessage
	if err := json.Unmarshal(data, &configured); err != nil {
		return bindings, fmt.Errorf("parse keybindings.json: %w", err)
	}
	for action, raw := range configured {
		if _, supported := bindings.bindings[action]; !supported {
			continue
		}
		values, err := decodeKeybindingValues(raw)
		if err != nil {
			return bindings, fmt.Errorf("keybindings.json %s: %w", action, err)
		}
		bindings.bindings[action] = values
	}
	return bindings, nil
}

func decodeKeybindingValues(raw json.RawMessage) ([]string, error) {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return validateKeybindingValues([]string{single})
	}
	var multiple []string
	if err := json.Unmarshal(raw, &multiple); err != nil {
		return nil, errors.New("binding must be a string or string array")
	}
	return validateKeybindingValues(multiple)
}

func validateKeybindingValues(values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if _, _, ok := parseBindingID(value); !ok {
			return nil, fmt.Errorf("invalid key %q", value)
		}
		result = append(result, value)
	}
	return result, nil
}

func (k appKeybindings) Matches(action, key string) bool {
	inputKey, inputModifiers, ok := parseBindingID(key)
	if !ok {
		return false
	}
	for _, binding := range k.bindings[action] {
		bindingKey, bindingModifiers, valid := parseBindingID(binding)
		if valid && bindingKey == inputKey && bindingModifiers == inputModifiers {
			return true
		}
	}
	return false
}

func (k appKeybindings) MatchesPress(action string, message tea.KeyPressMsg) bool {
	return k.Matches(action, message.Keystroke()) || k.Matches(action, message.String())
}

// WidgetKeys converts accepted config aliases and modifier ordering to the
// exact strings emitted by Bubble Tea's Keystroke method.
func (k appKeybindings) WidgetKeys(action string) []string {
	values := k.bindings[action]
	result := make([]string, 0, len(values))
	for _, value := range values {
		keyName, modifiers, ok := parseBindingID(value)
		if !ok {
			continue
		}
		var parts []string
		if modifiers&Ctrl != 0 {
			parts = append(parts, "ctrl")
		}
		if modifiers&Alt != 0 {
			parts = append(parts, "alt")
		}
		if modifiers&Shift != 0 {
			parts = append(parts, "shift")
		}
		if modifiers&Super != 0 {
			parts = append(parts, "super")
		}
		switch keyName {
		case "escape":
			keyName = "esc"
		case "pageup":
			keyName = "pgup"
		case "pagedown":
			keyName = "pgdown"
		}
		parts = append(parts, keyName)
		result = append(result, strings.Join(parts, "+"))
	}
	return result
}

func (k appKeybindings) Hint(action string) string {
	values := k.bindings[action]
	if len(values) == 0 {
		return "unbound"
	}
	limit := min(2, len(values))
	hints := make([]string, 0, limit)
	for _, value := range values[:limit] {
		keyName, modifiers, ok := parseBindingID(value)
		if !ok {
			continue
		}
		var parts []string
		if modifiers&Ctrl != 0 {
			parts = append(parts, "Ctrl")
		}
		if modifiers&Alt != 0 {
			parts = append(parts, "Alt")
		}
		if modifiers&Shift != 0 {
			parts = append(parts, "Shift")
		}
		if modifiers&Super != 0 {
			parts = append(parts, "Super")
		}
		switch keyName {
		case "escape":
			keyName = "Esc"
		case "enter":
			keyName = "Enter"
		case "pageup":
			keyName = "PgUp"
		case "pagedown":
			keyName = "PgDn"
		case "up":
			keyName = "↑"
		case "down":
			keyName = "↓"
		case "left":
			keyName = "←"
		case "right":
			keyName = "→"
		case "tab":
			keyName = "Tab"
		case "home":
			keyName = "Home"
		case "end":
			keyName = "End"
		default:
			keyName = strings.ToUpper(keyName)
		}
		parts = append(parts, keyName)
		hints = append(hints, strings.Join(parts, "+"))
	}
	if len(hints) == 0 {
		return "unbound"
	}
	return strings.Join(hints, " / ")
}

func (m *Model) reloadKeybindings() error {
	if m == nil || m.api == nil {
		return errors.New("application API is unavailable")
	}
	bindings, err := loadAppKeybindings(m.api.AgentDir())
	if err != nil {
		return err
	}
	m.keybindings = bindings
	m.composer.SetKeybindings(bindings)
	if m.selector != nil {
		m.selector.SetKeybindings(bindings)
	}
	return nil
}
