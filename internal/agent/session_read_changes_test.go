package agent_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
)

func TestActivityReadVersionsIncludeRetryWaitAfterCanonicalStart(t *testing.T) {
	entered := make(chan struct{})
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, sessionHTTPFailure(t, 429)), SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		Retry: agent.RetryPolicy{MaxAttempts: 2, InitialDelay: time.Hour, Sleep: func(ctx context.Context, _ time.Duration) error {
			close(entered)
			<-ctx.Done()
			return context.Cause(ctx)
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	_, changes, cancel := runtime.SubscribeReadChanges()
	defer cancel()
	started := make(chan agent.ActivityReadSnapshot, 1)
	runtime.SubscribeObserved(func(_ context.Context, event agent.SessionObservation) {
		if _, ok := event.Event.(agent.AutoRetryStartEvent); ok && event.Activity != nil {
			started <- *event.Activity
		}
	})
	done := make(chan error, 1)
	go func() { _, err := runtime.Prompt(context.Background(), "retry"); done <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("retry wait did not start")
	}
	before := <-started
	read, err := runtime.ReadSnapshot()
	if err != nil || before.State.RetryWaiting || !read.Activity.RetryWaiting || before.Version.Generation != read.ActivityVersion.Generation || before.Version.Revision >= read.ActivityVersion.Revision {
		t.Fatalf("canonical start and actual wait were conflated: before=%+v after=%+v, %v", before, read.ActivityVersion, err)
	}
	select {
	case change := <-changes:
		if change.Generation != read.ActivityVersion.Generation || change.Activity != read.ActivityVersion.Revision {
			t.Fatalf("watch/read mismatch: %+v %+v", change, read.ActivityVersion)
		}
	default:
		t.Fatal("retryCancel mutation had no invalidation")
	}
	runtime.AbortRetry()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after, err := runtime.ReadSnapshot()
	if err != nil || after.Activity.RetryWaiting || after.Activity.IsStreaming || after.ActivityVersion.Revision <= read.ActivityVersion.Revision {
		t.Fatalf("settlement reused running activity: %+v, %v", after.Activity, err)
	}
}

func TestEventlessConfigurationAndControlsHaveCoalescedInvalidations(t *testing.T) {
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: newScriptedProvider(t), SessionManager: newSessionManager(t), Model: sessionTestModel(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	initial, configChanges, cancelConfiguration := runtime.SubscribeConfigurationChanges()
	defer cancelConfiguration()
	_, changes, cancel := runtime.SubscribeReadChanges()
	defer cancel()
	for i := 0; i < 1000; i++ {
		if err := runtime.SetSystemPrompt(fmt.Sprintf("prompt %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	latest := <-configChanges
	read, err := runtime.ReadSnapshot()
	if err != nil || latest.Generation != initial.Generation || latest.Configuration != initial.Configuration+1000 || latest != read.Agent.Version || read.Agent.SystemPrompt != "prompt 999" {
		t.Fatalf("config watch/read = %+v %+v %v", latest, read.Agent.Version, err)
	}
	select {
	case <-configChanges:
		t.Fatal("unread mutations were not coalesced")
	default:
	}
	if err := runtime.SetAutoRetryEnabled(false); err != nil {
		t.Fatal(err)
	}
	if err := runtime.SetAutoCompactionEnabled(false); err != nil {
		t.Fatal(err)
	}
	change := <-changes
	read, err = runtime.ReadSnapshot()
	if err != nil || change.Controls != 2 || read.Controls.AutoRetryEnabled || read.Controls.AutoCompactionEnabled {
		t.Fatalf("controls = %+v %+v, %v", change, read.Controls, err)
	}
	// Both cancellation functions are safe to call repeatedly.
	cancel()
	cancelConfiguration()
	cancel()
	cancelConfiguration()
}
