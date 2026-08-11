package application

import (
	"errors"
	"testing"
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
