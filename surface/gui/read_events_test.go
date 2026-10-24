package main

import (
	"context"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/surfacewire"
)

func TestGUIReadEventStreamReplaysAndRejectsAnotherEpoch(t *testing.T) {
	config := app.ProductionConfig{WorkingDir: t.TempDir(), AgentDir: t.TempDir()}
	api, err := application.NewService(application.ServiceOptions{Production: config, DisableReaper: true})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close(context.Background())
	bridge := NewGUIBridge(config, "", "test")
	bridge.local = api
	before := surfacewire.EncodeEventCursor(api.CurrentCursor())
	if _, err := api.AddProject(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	opened, err := bridge.OpenReadEventStream(before, `[]`)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.CloseEventStream(opened.StreamID)
	if opened.ResetRequired || len(opened.Replay) != 1 || opened.Cursor == before || opened.Replay[0].Cursor != opened.Cursor {
		t.Fatalf("IPC replay = %+v", opened)
	}
	reset, err := bridge.OpenReadEventStream("anotherEpoch:1", `[]`)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.CloseEventStream(reset.StreamID)
	if !reset.ResetRequired || len(reset.Replay) != 0 || reset.Cursor != surfacewire.EncodeEventCursor(api.CurrentCursor()) {
		t.Fatalf("IPC reset = %+v", reset)
	}
}
