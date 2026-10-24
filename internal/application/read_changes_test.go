package application_test

import (
	"context"
	"testing"

	"github.com/cat3399/pi-go/internal/agent"
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

func TestApplicationPreservesMessageIdentityAndReceiptOutsideCanonicalEvents(t *testing.T) {
	harness := newSessionHarness(t)
	events := make(chan application.Event, 64)
	unsubscribe := harness.session.Subscribe(func(_ context.Context, event application.Event) { events <- event })
	defer unsubscribe()
	if _, err := harness.session.Dispatch(context.Background(), application.PromptCommand{Message: "hello"}); err != nil {
		t.Fatal(err)
	}
	starts := make(map[string]bool)
	ends := make(map[string]bool)
	receipts := 0
	for {
		event := waitApplicationEvent(t, events, func(application.Event) bool { return true })
		switch value := event.Value.(type) {
		case application.AgentSessionEvent:
			switch value.Event.(type) {
			case agent.MessageStartEvent:
				if value.MessageID == "" || value.Version.Generation == "" {
					t.Fatalf("application dropped read identity: %#v", value)
				}
				starts[value.MessageID] = true
			case agent.MessageEndEvent:
				if !starts[value.MessageID] {
					t.Fatalf("end has no matching start: %#v", value)
				}
				ends[value.MessageID] = true
			case agent.AgentSettledEvent:
				if receipts != 2 {
					t.Fatalf("receipt count = %d", receipts)
				}
				return
			}
		case application.MessageReceiptEvent:
			if !ends[value.Receipt.MessageID] || value.Receipt.EntryID == "" || !value.Receipt.Accepted {
				t.Fatalf("receipt lost its canonical message: %#v", value)
			}
			receipts++
		}
	}
}

func TestLiveReadDoesNotOpenInactiveRuntimeOrReturnSettledHistory(t *testing.T) {
	corpus := makeDataBenchmarkCorpus(t, dataBenchmarkShape{name: "live", sessions: 1, messages: 240, textBytes: 64})
	api, managers := openDataBenchmarkService(t, corpus)
	if _, active, err := api.ReadLiveSession(corpus.firstID); err != nil || active || len(managers) != 0 {
		t.Fatalf("inactive read opened a Runtime: active=%t managers=%d err=%v", active, len(managers), err)
	}
	if _, err := api.Dispatch(context.Background(), corpus.firstID, application.GetStateCommand{}); err != nil {
		t.Fatal(err)
	}
	value, active, err := api.ReadLiveSession(corpus.firstID)
	if err != nil || !active || value.State.Agent.MessageCount != 240 || value.State.Agent.Version.Generation == "" || len(value.State.Agent.Pending) != 0 || value.State.History.Entries != nil || value.State.History.Tree != nil {
		t.Fatalf("active live read = %#v, %t, %v", value, active, err)
	}
}
