package tui

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/cat3399/pi-go/internal/llm"
)

func TestSplitEditorCommandPreservesQuotedArguments(t *testing.T) {
	parts, err := splitEditorCommand(`code --wait "--reuse window"`)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parts, []string{"code", "--wait", "--reuse window"}) {
		t.Fatalf("editor command = %#v", parts)
	}
	if _, err := splitEditorCommand(`code "unterminated`); err == nil {
		t.Fatal("unmatched editor quote was accepted")
	}
}

func TestPrepareExternalEditorCreatesPrivateRecoverableDraft(t *testing.T) {
	root := t.TempDir()
	command, path, err := prepareExternalEditorIn(
		root, []string{"EDITOR=code --wait"}, root, "draft text",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(command.Args, []string{"code", "--wait", path}) || command.Dir != root {
		t.Fatalf("editor process = %#v, dir=%q", command.Args, command.Dir)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "draft text" {
		t.Fatalf("draft = %q, %v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 || filepath.Dir(path) == root {
		t.Fatalf("draft info = %#v, %v", info, err)
	}
}

func TestExternalEditorCompletionUpdatesTextAndKeepsImages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt.md")
	if err := os.WriteFile(path, []byte("edited text\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	image, err := llm.NewImageDataBlock("image/png", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	model := newModelForTest(t)
	model.composer.SetDraft("old", []llm.ImageBlock{image})
	_, _ = model.Update(externalEditorFinishedMsg{path: path})
	if model.composer.Value() != "edited text" || len(model.composer.Images()) != 1 {
		t.Fatalf("updated composer = %q / %#v", model.composer.Value(), model.composer.Images())
	}
	if model.status.level != statusSuccess {
		t.Fatalf("editor status = %#v", model.status)
	}
}
