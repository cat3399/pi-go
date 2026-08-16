package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/llm"
)

func TestSlashPaletteFiltersAcceptsAndMergesDynamicCommands(t *testing.T) {
	commands := mergeSlashCommands([]application.SlashCommandInfo{
		{Name: "review", Description: "Review the current change", Source: application.CommandSourcePrompt},
		{Name: "help", Description: "must not shadow builtin", Source: application.CommandSourcePrompt},
	})
	palette := slashPaletteModel{}
	palette.SetCommands(commands)
	palette.Update("/rev")
	if !palette.Visible() || len(palette.matches) != 1 || palette.matches[0].name != "review" {
		t.Fatalf("review matches = %#v", palette.matches)
	}
	if value, ok := palette.Accept(); !ok || value != "/review " || palette.Visible() {
		t.Fatalf("accepted palette = %q, %t, visible=%t", value, ok, palette.Visible())
	}
	helpCount := 0
	for _, command := range commands {
		if command.name == "help" {
			helpCount++
		}
	}
	if helpCount != 1 {
		t.Fatalf("merged help command count = %d", helpCount)
	}
}

func TestModelSlashPaletteHandlesSelectionBeforePromptHistory(t *testing.T) {
	model := newModelForTest(t)
	model.composer.AddToHistory("historical prompt")
	model.composer.SetDraft("/", nil)
	model.slashPalette.Update(model.composer.Value())
	if !model.slashPalette.Visible() {
		t.Fatal("slash palette is not visible")
	}
	first := model.slashPalette.matches[0].name
	_, _ = model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	selected := model.slashPalette.matches[model.slashPalette.selected].name
	if selected == first || model.composer.Value() != "/" {
		t.Fatalf("Down selected %q from %q and changed composer to %q", selected, first, model.composer.Value())
	}
	_, _ = model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if !strings.HasPrefix(model.composer.Value(), "/") || !strings.HasSuffix(model.composer.Value(), " ") {
		t.Fatalf("accepted slash command = %q", model.composer.Value())
	}
	if model.slashPalette.Visible() {
		t.Fatal("palette remained visible after completion")
	}
}

func TestModelRendersSlashPaletteAboveComposer(t *testing.T) {
	model := newModelForTest(t)
	model.width, model.height = 80, 18
	model.composer.SetWidth(model.width)
	model.composer.SetDraft("/thi", nil)
	model.slashPalette.Update(model.composer.Value())
	view := StripTerminalSequences(model.View().Content)
	if !strings.Contains(view, "/thinking [level]") || !strings.Contains(view, "reasoning level") {
		t.Fatalf("slash palette view:\n%s", view)
	}
	if rows := strings.Count(model.View().Content, "\n") + 1; rows != model.height {
		t.Fatalf("palette view rows = %d, want %d", rows, model.height)
	}
}

func TestModelHidesSlashPaletteForAttachmentDraft(t *testing.T) {
	image, err := llm.NewImageDataBlock("image/png", []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	model := newModelForTest(t)
	model.composer.SetDraft("/help", []llm.ImageBlock{image})
	model.updateSlashPalette()
	if model.slashPalette.Visible() {
		t.Fatal("slash palette offered a local command for an attachment prompt")
	}
}

func TestCommandsLoadedMessageRejectsStaleSession(t *testing.T) {
	model := newModelForTest(t)
	model.commandsRequest = 2
	_, _ = model.Update(commandsLoadedMsg{
		sessionID: "other", sessionGeneration: model.sessionGeneration, request: 2,
		commands: []application.SlashCommandInfo{{Name: "stale", Source: application.CommandSourcePrompt}},
	})
	model.slashPalette.Update("/stale")
	if model.slashPalette.Visible() {
		t.Fatal("stale command result entered the palette")
	}
	_, _ = model.Update(commandsLoadedMsg{
		sessionID: model.sessionID, sessionGeneration: model.sessionGeneration, request: 2,
		commands: []application.SlashCommandInfo{{Name: "fresh", Source: application.CommandSourcePrompt}},
	})
	model.slashPalette.Update("/fresh")
	if !model.slashPalette.Visible() || model.slashPalette.matches[0].name != "fresh" {
		t.Fatalf("fresh commands = %#v", model.slashPalette.matches)
	}
}

func TestRequestCommandsDropsPreviousDynamicCommandsWhileLoading(t *testing.T) {
	model := newModelForTest(t)
	model.slashPalette.SetCommands(mergeSlashCommands([]application.SlashCommandInfo{{
		Name: "old-command", Description: "belongs to the previous resource set", Source: application.CommandSourcePrompt,
	}}))
	model.slashPalette.Update("/old")
	if !model.slashPalette.Visible() {
		t.Fatal("dynamic command was not installed for the test")
	}

	if command := model.requestCommands(); command == nil {
		t.Fatal("command refresh returned no command")
	}
	model.slashPalette.Update("/old")
	if model.slashPalette.Visible() {
		t.Fatalf("old dynamic command remained during refresh: %#v", model.slashPalette.matches)
	}
}
