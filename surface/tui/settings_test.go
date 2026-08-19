package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/llm"
)

type settingsRecordingAPI struct {
	modelTestAPI
	commands     []application.Command
	themeSetting string
}

func (a *settingsRecordingAPI) SetTheme(_ context.Context, _, theme string) (application.UISettings, error) {
	a.themeSetting = theme
	return application.UISettings{Theme: theme}, nil
}

func (a *settingsRecordingAPI) Dispatch(_ context.Context, _ string, command application.Command) (application.CommandResult, error) {
	a.commands = append(a.commands, command)
	switch command.(type) {
	case application.SetAutoCompactionCommand:
		return application.SetAutoCompactionResult{}, nil
	case application.SetSteeringModeCommand:
		return application.SetSteeringModeResult{}, nil
	case application.CycleModelCommand:
		return application.CycleModelResult{}, nil
	case application.CycleThinkingLevelCommand:
		return application.CycleThinkingLevelResult{}, nil
	case application.AbortRetryCommand:
		return application.AbortRetryResult{}, nil
	default:
		return nil, application.ErrInvalidCommand
	}
}

func TestSettingsSelectorDispatchesRuntimeControls(t *testing.T) {
	api := &settingsRecordingAPI{}
	model := newModelWithAPIForTest(t, api)
	model.state.AutoCompactionEnabled = true
	model.state.SteeringMode = agent.QueueOneAtATime

	_ = model.openSettingsSelector()
	if model.selector == nil || model.selector.kind != selectorSettings || len(model.selector.items) != 7 {
		t.Fatalf("settings selector = %#v", model.selector)
	}
	model.selector.SelectKey(settingAutoCompaction)
	message := model.applySelectorSelection()()
	model.Update(message)
	if command, ok := api.commands[0].(application.SetAutoCompactionCommand); !ok || command.Enabled {
		t.Fatalf("auto compaction command = %#v", api.commands[0])
	}
	if model.state.AutoCompactionEnabled || model.selector == nil {
		t.Fatalf("settings state = enabled:%t selector:%v", model.state.AutoCompactionEnabled, model.selector != nil)
	}

	model.selector.SelectKey(settingSteeringMode)
	message = model.applySelectorSelection()()
	model.Update(message)
	if command, ok := api.commands[1].(application.SetSteeringModeCommand); !ok || command.Mode != agent.QueueAll {
		t.Fatalf("steering command = %#v", api.commands[1])
	}
}

func TestSettingsThemeChangePersistsAndRethemesLiveComponents(t *testing.T) {
	api := &settingsRecordingAPI{}
	model := newModelWithAPIForTest(t, api)
	_ = model.openSettingsSelector()
	model.selector.SelectKey(settingTheme)
	command := model.applySelectorSelection()
	if command == nil {
		t.Fatal("theme selection did not dispatch")
	}
	model.Update(command())
	if api.themeSetting != ThemeLight || model.themeSetting != ThemeLight || !model.theme.IsLight {
		t.Fatalf("theme = persisted:%q setting:%q value:%#v", api.themeSetting, model.themeSetting, model.theme)
	}
	if model.composer.theme.ID != model.theme.ID || model.renderer.theme.ID != model.theme.ID || model.selector.theme.ID != model.theme.ID {
		t.Fatal("live components retained the previous theme")
	}
}

func TestRuntimeControlHotkeysAndRetryCancellation(t *testing.T) {
	api := &settingsRecordingAPI{}
	model := newModelWithAPIForTest(t, api)
	for _, key := range []tea.Key{
		{Code: 'p', Mod: tea.ModCtrl},
		{Code: 'p', Mod: tea.ModCtrl | tea.ModShift},
		{Code: tea.KeyTab, Mod: tea.ModShift},
	} {
		handled, command := model.handleKey(tea.KeyPressMsg(key))
		if !handled || command == nil {
			t.Fatalf("hotkey %q not handled", tea.KeyPressMsg(key).String())
		}
		model.Update(command())
	}
	if forward := api.commands[0].(application.CycleModelCommand); forward.Direction != agent.CycleForward {
		t.Fatalf("forward direction = %q", forward.Direction)
	}
	if backward := api.commands[1].(application.CycleModelCommand); backward.Direction != agent.CycleBackward {
		t.Fatalf("backward direction = %q", backward.Direction)
	}
	if _, ok := api.commands[2].(application.CycleThinkingLevelCommand); !ok {
		t.Fatalf("thinking command = %#v", api.commands[2])
	}

	model.state.RetryWaiting = true
	handled, command := model.handleKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	if !handled || command == nil {
		t.Fatal("retry escape was not handled")
	}
	model.Update(command())
	if _, ok := api.commands[3].(application.AbortRetryCommand); !ok {
		t.Fatalf("retry command = %#v", api.commands[3])
	}
}

func TestThinkingVisibilityHidesReasoningOnly(t *testing.T) {
	renderer := newContentRenderer(DefaultTheme())
	item := contentItem{Role: contentRoleAssistant, Title: "Assistant", Blocks: []contentBlock{
		{Kind: contentBlockThinking, Text: "private reasoning"},
		{Kind: contentBlockText, Text: "public answer"},
	}}
	renderer.SetThinkingVisible(false)
	view := StripTerminalSequences(strings.Join(renderer.Render(item, 80), "\n"))
	if strings.Contains(view, "private reasoning") || !strings.Contains(view, "public answer") {
		t.Fatalf("hidden thinking view = %q", view)
	}
}

func TestClipboardImageHotkeyAttachesImage(t *testing.T) {
	model := newModelForTest(t)
	image, err := llm.NewImageDataBlock("image/png", tinyPNG(t))
	if err != nil {
		t.Fatal(err)
	}
	model.readClipboardImage = func(context.Context) (llm.ImageBlock, error) { return image, nil }
	handled, command := model.handleKey(tea.KeyPressMsg(tea.Key{Code: 'v', Mod: tea.ModCtrl}))
	if !handled || command == nil {
		t.Fatal("clipboard image shortcut was not handled")
	}
	model.Update(command())
	if images := model.composer.Images(); len(images) != 1 || len(images[0].Data()) == 0 {
		t.Fatalf("composer images = %#v", images)
	}
}
