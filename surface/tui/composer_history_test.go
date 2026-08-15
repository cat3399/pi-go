package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/cat3399/pi-go/internal/llm"
)

func TestComposerHistoryNavigatesAndRestoresDraft(t *testing.T) {
	composer := newComposerModel(DefaultTheme())
	composer.AddToHistory("first prompt")
	composer.AddToHistory("second prompt")
	composer.SetDraft("unfinished", nil)

	if !composer.NavigateHistory("up") || composer.Value() != "unfinished" || composer.input.Column() != 0 {
		t.Fatalf("first Up at editor edge = %q at %d", composer.Value(), composer.input.Column())
	}
	if !composer.NavigateHistory("up") || composer.Value() != "second prompt" {
		t.Fatalf("first history step = %q", composer.Value())
	}
	if !composer.NavigateHistory("up") || composer.Value() != "first prompt" {
		t.Fatalf("second history step = %q", composer.Value())
	}
	if !composer.NavigateHistory("down") || composer.Value() != "second prompt" || composer.input.Column() != len("second prompt") {
		t.Fatalf("newer history step = %q at %d", composer.Value(), composer.input.Column())
	}
	if !composer.NavigateHistory("down") || composer.Value() != "unfinished" || composer.input.Column() != 0 {
		t.Fatalf("restored draft = %q at %d", composer.Value(), composer.input.Column())
	}
}

func TestComposerHistoryRespectsMultilineCursorAndEdits(t *testing.T) {
	composer := newComposerModel(DefaultTheme())
	_ = composer.Init()
	composer.AddToHistory("previous")
	composer.SetDraft("line one\nline two", nil)
	if composer.NavigateHistory("up") {
		t.Fatal("Up on the last logical line replaced the multiline draft")
	}
	composer.input.MoveToBegin()
	if !composer.NavigateHistory("up") || composer.Value() != "previous" {
		t.Fatalf("Up at the first visual line = %q", composer.Value())
	}
	composer.Update(tea.KeyPressMsg(tea.Key{Code: 'x', Text: "x"}))
	if composer.historyIndex != -1 || composer.Value() != "xprevious" {
		t.Fatalf("editing recalled history = index %d, value %q", composer.historyIndex, composer.Value())
	}
}

func TestComposerHistoryDoesNotReplaceAttachmentDraft(t *testing.T) {
	image, err := llm.NewImageDataBlock("image/png", []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	composer := newComposerModel(DefaultTheme())
	composer.AddToHistory("previous")
	composer.SetDraft("image draft", []llm.ImageBlock{image})
	if composer.NavigateHistory("up") {
		t.Fatal("history navigation replaced an attachment draft")
	}
	if composer.Value() != "image draft" || len(composer.Images()) != 1 {
		t.Fatalf("attachment draft = %q / %#v", composer.Value(), composer.Images())
	}
}

func TestComposerAttachmentHeightUsesReservedRow(t *testing.T) {
	image, err := llm.NewImageDataBlock("image/png", []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	composer := newComposerModel(DefaultTheme())
	composer.SetMaxHeight(4)
	composer.SetDraft("one\ntwo\nthree\nfour", []llm.ImageBlock{image})
	if composer.input.MaxHeight != 3 {
		t.Fatalf("input max height with attachment = %d, want 3", composer.input.MaxHeight)
	}
	composer.Reset()
	if composer.input.MaxHeight != 4 {
		t.Fatalf("input max height after reset = %d, want 4", composer.input.MaxHeight)
	}
}
