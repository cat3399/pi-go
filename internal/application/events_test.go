package application

import (
	"errors"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/agent"
)

func TestEventStreamReplaysFromCursorAndContinuesInOneOrder(t *testing.T) {
	stream := newEventStream(4)
	stream.publish(Event{SessionID: "one", Value: OperationEvent{OperationID: 1, Command: CommandPrompt, Status: OperationCompleted}})
	stream.publish(Event{SessionID: "two", Value: OperationEvent{OperationID: 2, Command: CommandPrompt, Status: OperationFailed, Error: "boom"}})

	subscription, err := stream.subscribe(1)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	stream.publish(Event{SessionID: "one", Value: OperationEvent{OperationID: 3, Command: CommandPrompt, Status: OperationCompleted}})

	if len(subscription.Replay) != 1 {
		t.Fatalf("replay = %#v", subscription.Replay)
	}
	second := subscription.Replay[0]
	third := <-subscription.Events
	if second.Sequence != 2 || second.SessionID != "two" || third.Sequence != 3 || third.SessionID != "one" {
		t.Fatalf("events = %#v, %#v", second, third)
	}
}

func TestReadEventCursorRejectsRestartEvenAtSameSequence(t *testing.T) {
	before, after := newEventStream(4), newEventStream(4)
	defer before.close()
	defer after.close()
	event := Event{SessionID: "one", Value: OperationEvent{Command: CommandPrompt, Status: OperationCompleted}}
	before.publish(event)
	after.publish(event)
	cursor := before.currentCursor()
	if _, err := after.subscribeRead(ReadEventOptions{After: &cursor}, false); !errors.Is(err, ErrEventCursorUnavailable) {
		t.Fatalf("old process cursor accepted: %v", err)
	}
	current, err := after.subscribeRead(ReadEventOptions{}, false)
	if err != nil || len(current.Replay) != 0 || current.Cursor.Epoch == cursor.Epoch {
		t.Fatalf("new subscription = %+v, %v", current, err)
	}
	current.Close()
}

func TestReadEventScopeKeepsGlobalChangesAndSelectedDetailsInOrder(t *testing.T) {
	stream := newEventStream(20)
	defer stream.close()
	start := stream.currentCursor()
	details := []string{"selected"}
	subscription, err := stream.subscribeRead(ReadEventOptions{After: &start, Sessions: &details}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	for _, id := range []string{"selected", "other"} {
		stream.publish(Event{SessionID: id, Value: AgentSessionEvent{Event: agent.ToolExecutionStartEvent{ToolCallID: id, ToolName: "read"}}})
		stream.publish(Event{SessionID: id, Value: SessionCatalogEvent{Change: SessionUpdated}})
	}
	var live []Event
	for range 3 {
		live = append(live, <-subscription.Events)
	}
	if live[0].SessionID != "selected" || live[0].Sequence != 1 || live[1].Sequence != 2 || live[2].SessionID != "other" || live[2].Sequence != 4 {
		t.Fatalf("scope order = %+v", live)
	}
	select {
	case event := <-subscription.Events:
		t.Fatalf("unselected details leaked: %+v", event)
	default:
	}
	replay, err := stream.subscribeRead(ReadEventOptions{After: &start, Sessions: &details}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	if len(replay.Replay) != len(live) {
		t.Fatalf("scope differs between replay/live: %+v", replay.Replay)
	}
	for i, event := range replay.Replay {
		if event.Sequence != live[i].Sequence || event.Epoch != stream.currentCursor().Epoch {
			t.Fatal("replay changed event identity")
		}
	}
}

func TestReadEventHistoryEvictsByBytesIncludingSingleOversizedEvent(t *testing.T) {
	stream := newEventStream(100)
	defer stream.close()
	stream.byteBudget = 1024
	start := stream.currentCursor()
	stream.publish(Event{SessionID: "one", Value: OperationEvent{Error: strings.Repeat("large", 1000)}})
	if len(stream.history) != 0 || stream.retainedBytes != 0 {
		t.Fatalf("oversized event retained: len=%d bytes=%d", len(stream.history), stream.retainedBytes)
	}
	if _, err := stream.subscribeRead(ReadEventOptions{After: &start}, false); !errors.Is(err, ErrEventCursorUnavailable) {
		t.Fatalf("empty buffer concealed cursor gap: %v", err)
	}
	latest := stream.currentCursor()
	stream.publish(Event{SessionID: "one", Value: SessionCatalogEvent{Change: SessionUpdated}})
	subscription, err := stream.subscribeRead(ReadEventOptions{After: &latest}, false)
	if err != nil || len(subscription.Replay) != 1 {
		t.Fatalf("post-gap cursor unavailable: %+v %v", subscription, err)
	}
	subscription.Close()
}

func TestEventStreamRejectsCursorOlderThanRetainedHistory(t *testing.T) {
	stream := newEventStream(2)
	for operationID := uint64(1); operationID <= 3; operationID++ {
		stream.publish(Event{SessionID: "session", Value: OperationEvent{
			OperationID: operationID, Command: CommandPrompt, Status: OperationCompleted,
		}})
	}
	if _, err := stream.subscribe(0); !errors.Is(err, ErrEventCursorUnavailable) {
		t.Fatalf("subscribe error = %v", err)
	}
	if _, err := stream.subscribe(1); err != nil {
		t.Fatalf("oldest retained cursor was rejected: %v", err)
	}
}
