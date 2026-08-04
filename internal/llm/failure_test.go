package llm_test

import (
	"errors"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

func TestFailureCauseSurvivesEventCollectorSnapshotAndResult(t *testing.T) {
	t.Parallel()

	cause := errors.New("typed cause")
	failure, err := llm.NewFailure("display message", cause)
	if err != nil {
		t.Fatalf("NewFailure() error = %v", err)
	}
	event, err := newErrorEventWithFailure(
		llm.FinishError,
		failure,
		llm.Usage{},
		time.Time{},
	)
	if err != nil {
		t.Fatalf("NewErrorEventWithFailure() error = %v", err)
	}
	if !errors.Is(event.Failure(), cause) || event.ErrorMessage() != "display message" {
		t.Fatalf("event failure = (%q, cause=%v)", event.ErrorMessage(), event.Failure().Cause())
	}

	collector := &llm.StreamCollector{}
	if err := collector.Accept(event); err != nil {
		t.Fatalf("Accept(error) error = %v", err)
	}
	snapshot, err := collector.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	snapshotFailure, ok := snapshot.Failure()
	if !ok || !errors.Is(snapshotFailure, cause) {
		t.Fatalf("snapshot failure = (%v, %t), want typed cause", snapshotFailure, ok)
	}
	if err := collector.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	terminal, err := collector.Result()
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	terminalFailure, ok := terminal.(llm.AssistantFailureMessage)
	if !ok || !errors.Is(terminalFailure.Failure(), cause) {
		t.Fatalf("terminal = %T/cause %v, want AssistantFailureMessage with typed cause", terminal, terminalFailure.Failure().Cause())
	}
	if terminalFailure.Failure().Cause() != event.Failure().Cause() || snapshotFailure.Cause() != event.Failure().Cause() {
		t.Fatal("event, snapshot, and result did not retain the same failure cause")
	}
}

func TestFailureConstructorsRejectInvalidZeroValue(t *testing.T) {
	t.Parallel()

	if _, err := llm.NewFailure(" ", errors.New("cause")); !errors.Is(err, llm.ErrInvalidFailure) {
		t.Fatalf("NewFailure(blank) error = %v, want ErrInvalidFailure", err)
	}
	if _, err := newErrorEventWithFailure(llm.FinishError, llm.Failure{}, llm.Usage{}, time.Time{}); !errors.Is(err, llm.ErrInvalidStreamEvent) {
		t.Fatalf("NewErrorEventWithFailure(zero) error = %v, want ErrInvalidStreamEvent", err)
	}
	if _, err := newAssistantFailureMessageWithFailure(nil, llm.FinishError, llm.Failure{}, llm.Usage{}, time.Time{}); !errors.Is(err, llm.ErrInvalidFailure) {
		t.Fatalf("NewAssistantFailureMessageWithFailure(zero) error = %v, want ErrInvalidFailure", err)
	}
}
