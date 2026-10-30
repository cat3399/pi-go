package application_test

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

// Interactive surfaces submit prompt with a streaming behavior, matching pi's
// interactive-mode. Low-level steer/follow_up intentionally remain queue APIs.
func TestStreamingInputAcrossAnswerCompletion(t *testing.T) {
	for _, behavior := range []agent.StreamingBehavior{agent.StreamingSteer, agent.StreamingFollowUp} {
		for _, boundary := range []string{"message_update", "agent_end", "idle"} {
			t.Run(string(behavior)+"/"+boundary, func(t *testing.T) {
				first, _ := provider.FixedResponseStep(sessionTextTerminal(t, "first answer"))
				second, _ := provider.FixedResponseStep(sessionTextTerminal(t, "second answer"))
				harness := newSessionHarness(t, first, second)
				reached, release := make(chan struct{}), make(chan struct{})
				var released sync.Once
				defer released.Do(func() { close(release) })
				var held atomic.Bool
				unsubscribe := harness.runtime.Session().Subscribe(func(ctx context.Context, event agent.SessionEvent) {
					if string(event.Type()) != boundary || !held.CompareAndSwap(false, true) {
						return
					}
					close(reached)
					select {
					case <-release:
					case <-ctx.Done():
					}
				})
				defer unsubscribe()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if _, err := harness.session.Dispatch(ctx, application.PromptCommand{Message: "start"}); err != nil {
					t.Fatal(err)
				}
				if boundary == "idle" {
					if err := harness.runtime.Session().WaitForIdle(ctx); err != nil {
						t.Fatal(err)
					}
				} else {
					select {
					case <-reached:
					case <-ctx.Done():
						t.Fatal("answer boundary was not reached")
					}
				}
				accepted := make(chan error, 1)
				go func() {
					_, err := harness.session.Dispatch(ctx, application.PromptCommand{Message: "inserted", StreamingBehavior: behavior})
					accepted <- err
				}()
				if boundary != "idle" {
					for harness.runtime.Session().PendingMessageCount() == 0 {
						select {
						case <-ctx.Done():
							t.Fatal("streaming input did not enter queue")
						case <-time.After(time.Millisecond):
						}
					}
					released.Do(func() { close(release) })
				}
				select {
				case err := <-accepted:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("input was not accepted")
				}
				if err := harness.runtime.Session().WaitForIdle(ctx); err != nil {
					t.Fatal(err)
				}
				requests := harness.implementation.Requests()
				if len(requests) != 2 {
					t.Fatalf("provider requests = %d", len(requests))
				}
				var inputs []string
				for _, message := range requests[1].Messages() {
					switch value := message.(type) {
					case llm.UserTextMessage:
						for _, block := range value.Content() {
							inputs = append(inputs, block.Text())
						}
					case llm.UserContentMessage:
						for _, block := range value.Content() {
							if text, ok := block.(llm.TextBlock); ok {
								inputs = append(inputs, text.Text())
							}
						}
					}
				}
				if !reflect.DeepEqual(inputs, []string{"start", "inserted"}) {
					t.Fatalf("provider input = %v", inputs)
				}
				if count := harness.runtime.Session().PendingMessageCount(); count != 0 {
					t.Fatalf("stranded messages = %d", count)
				}
			})
		}
	}
}

func TestPromptPreflightAcknowledgementSurvivesImmediateCompletion(t *testing.T) {
	harness := newSessionHarnessWithOptions(t, sessionHarnessOptions{Configure: func(config *agent.SessionConfig) {
		config.Hooks.InputHandlers = []agent.InputHook{func(context.Context, agent.InputEvent) (agent.InputResult, error) {
			return agent.InputResult{Action: agent.InputHandled}, nil
		}}
	}})
	var writers sync.WaitGroup
	for i := 0; i < 100; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			result, err := harness.session.Dispatch(context.Background(), application.PromptCommand{Message: "handled"})
			if err != nil {
				t.Error(err)
				return
			}
			if _, ok := result.(application.PromptStartedResult); !ok {
				t.Errorf("result = %T", result)
			}
		}()
	}
	writers.Wait()
}
