package application_test

import (
	"context"
	"testing"

	"github.com/cat3399/pi-go/internal/application"
)

func TestApplicationForwardsOwnerReadChangesIncludingDirectBranchMutation(t *testing.T) {
	harness := newSessionHarness(t)
	updates := make(chan application.Event, 8)
	unsubscribe := harness.session.Subscribe(func(_ context.Context, event application.Event) {
		if _, ok := event.Value.(application.SessionResourceEvent); ok {
			updates <- event
		}
	})
	defer unsubscribe()
	manager := harness.runtime.Session().SessionManager()
	entry, err := manager.AppendCustomEntry(context.Background(), "marker", nil)
	if err != nil {
		t.Fatal(err)
	}
	appended := waitApplicationEvent(t, updates, func(event application.Event) bool {
		return event.Value.(application.SessionResourceEvent).Change.LeafID == entry.ID()
	})
	if err := manager.ResetLeaf(); err != nil {
		t.Fatal(err)
	}
	branched := waitApplicationEvent(t, updates, func(event application.Event) bool {
		return event.Value.(application.SessionResourceEvent).Change.LeafID == ""
	})
	before, after := appended.Value.(application.SessionResourceEvent).Change, branched.Value.(application.SessionResourceEvent).Change
	if appended.SessionID != manager.SessionID() || before.Version.Generation == "" || before.Version.Generation != after.Version.Generation || before.Version.Log != after.Version.Log || before.Version.View >= after.Version.View {
		t.Fatalf("owner changes = %#v, %#v", before, after)
	}
}
