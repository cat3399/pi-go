package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cat3399/pi-go/internal/application"
)

type sessionTransferAPI struct {
	modelTestAPI
	importedPath string
	state        application.State
	snapshot     application.SessionSnapshot
}

func (a *sessionTransferAPI) ExportSession(context.Context, string) (application.SessionExport, error) {
	return application.SessionExport{FileName: "default.html", HTML: []byte("<html>ok</html>")}, nil
}

func (a *sessionTransferAPI) ExportSessionJSONL(context.Context, string) (application.SessionJSONLExport, error) {
	return application.SessionJSONLExport{FileName: "default.jsonl", JSONL: []byte("{\"type\":\"session\"}\n")}, nil
}

func (a *sessionTransferAPI) ImportSession(_ context.Context, _ string, path, _ string) (application.SessionImportResult, error) {
	a.importedPath = path
	return application.SessionImportResult{State: a.state}, nil
}

func (a *sessionTransferAPI) SnapshotSession(string, string) (application.SessionSnapshot, error) {
	return a.snapshot, nil
}

func TestExportSessionCommandChoosesFormatAndWritesRequestedPath(t *testing.T) {
	api := &sessionTransferAPI{}
	directory := t.TempDir()
	message, ok := exportSessionCmd(context.Background(), api, "session-1", directory, "nested/export.jsonl")().(sessionExportedMsg)
	if !ok || message.err != nil {
		t.Fatalf("export message = %#v", message)
	}
	wantPath := filepath.Join(directory, "nested", "export.jsonl")
	if message.path != wantPath {
		t.Fatalf("export path = %q, want %q", message.path, wantPath)
	}
	data, err := os.ReadFile(wantPath)
	if err != nil || string(data) != "{\"type\":\"session\"}\n" {
		t.Fatalf("exported data = %q, %v", data, err)
	}
}

func TestImportConfirmationResolvesPathAndSwitchesSession(t *testing.T) {
	newState := application.State{SessionID: "imported", CWD: "/workspace"}
	api := &sessionTransferAPI{state: newState, snapshot: application.SessionSnapshot{
		SessionID: "imported", Info: application.SessionInfo{ID: "imported", CWD: "/workspace"}, LiveState: &newState,
	}}
	model := newModelWithAPIForTest(t, api)
	_ = model.openImportConfirm("backups/session.jsonl")
	if !model.selector.SelectKey("import") {
		t.Fatal("import option was not selectable")
	}
	command := selectorCommandAt(t, model.applySelectorSelection(), 1)
	message, ok := command().(sessionOpenedMsg)
	if !ok || message.err != nil {
		t.Fatalf("import message = %#v", message)
	}
	model.Update(message)
	if api.importedPath != "/workspace/backups/session.jsonl" || model.sessionID != "imported" {
		t.Fatalf("import path=%q session=%q", api.importedPath, model.sessionID)
	}
}
