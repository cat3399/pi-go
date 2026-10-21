package surfacewire

import (
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/application"
)

type snapshotOnlyAPI struct {
	application.API
	value application.Snapshot
	reads int
}

func (a *snapshotOnlyAPI) Snapshot() (application.Snapshot, error) {
	a.reads++
	return a.value, nil
}

func TestSnapshotEncodesOneApplicationProjection(t *testing.T) {
	at := time.UnixMilli(1000)
	api := &snapshotOnlyAPI{value: application.Snapshot{
		Revision: 42, AgentDir: "/agent", DefaultCWD: "/project", RunningSessionIDs: []string{"session"},
		Sessions: []application.SessionInfo{{ID: "session", CWD: "/project", FirstMessage: "first", MessageCount: 3, Modified: at, Created: at}},
		Projects: []application.ProjectInfo{{Path: "/project", SessionCount: 1, Modified: at}},
	}}
	// The embedded API is nil: any extra catalog, state, or cursor query would
	// fail instead of silently deriving a second projection in the wire layer.
	value, err := Snapshot(api)
	if err != nil {
		t.Fatal(err)
	}
	if api.reads != 1 || value.Revision != 42 || value.AgentDir != "/agent" || value.DefaultCWD != "/project" || len(value.Sessions) != 1 || len(value.Projects) != 1 || len(value.RunningSessionIDs) != 1 {
		t.Fatalf("wire snapshot = %#v, reads=%d", value, api.reads)
	}
	if value.Sessions[0].ProjectRoot != "/project" || value.Sessions[0].Modified != "1970-01-01T00:00:01Z" || value.Projects[0].SessionCount != 1 {
		t.Fatalf("wire catalog = %#v", value)
	}
}
