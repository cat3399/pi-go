package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	keyInterrupt        = "app.interrupt"
	keyClear            = "app.clear"
	keyExit             = "app.exit"
	keyThinkingCycle    = "app.thinking.cycle"
	keyThinkingToggle   = "app.thinking.toggle"
	keyModelSelect      = "app.model.select"
	keyModelForward     = "app.model.cycleForward"
	keyModelBackward    = "app.model.cycleBackward"
	keyToolsExpand      = "app.tools.expand"
	keyExternalEditor   = "app.editor.external"
	keyMessageCopy      = "app.message.copy"
	keyMessageFollowUp  = "app.message.followUp"
	keyMessageDequeue   = "app.message.dequeue"
	keyPasteImage       = "app.clipboard.pasteImage"
	keyViewportPageUp   = "tui.altScreen.pageUp"
	keyViewportPageDown = "tui.altScreen.pageDown"
	keyViewportPrevious = "tui.altScreen.previousPrompt"
	keyViewportNext     = "tui.altScreen.nextPrompt"
	keyViewportTop      = "tui.altScreen.top"
	keyViewportBottom   = "tui.altScreen.bottom"
	keyInputSubmit      = "tui.input.submit"
)

type appKeybindings struct {
	bindings map[string][]string
}

func defaultAppKeybindings() appKeybindings {
	return appKeybindings{bindings: map[string][]string{
		keyInterrupt:        {"escape"},
		keyClear:            {"ctrl+c"},
		keyExit:             {"ctrl+d"},
		keyThinkingCycle:    {"shift+tab"},
		keyThinkingToggle:   {"ctrl+t"},
		keyModelSelect:      {"ctrl+l"},
		keyModelForward:     {"ctrl+p"},
		keyModelBackward:    {"shift+ctrl+p"},
		keyToolsExpand:      {"ctrl+o"},
		keyExternalEditor:   {"ctrl+g"},
		keyMessageCopy:      {"ctrl+x"},
		keyMessageFollowUp:  {"alt+enter"},
		keyMessageDequeue:   {"alt+up"},
		keyPasteImage:       {"ctrl+v"},
		keyViewportPageUp:   {"pageup", "ctrl+up"},
		keyViewportPageDown: {"pagedown", "ctrl+down"},
		keyViewportPrevious: {"ctrl+shift+up"},
		keyViewportNext:     {"ctrl+shift+down"},
		keyViewportTop:      {"ctrl+home"},
		keyViewportBottom:   {"ctrl+end"},
		keyInputSubmit:      {"enter"},
	}}
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

func (m *Model) reloadKeybindings() error {
	if m == nil || m.api == nil {
		return errors.New("application API is unavailable")
	}
	bindings, err := loadAppKeybindings(m.api.AgentDir())
	if err != nil {
		return err
	}
	m.keybindings = bindings
	return nil
}
