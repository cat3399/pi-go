package provider

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/llm"
)

func TestOpenAIResponsesRejectsInvalidFunctionNameBeforeNetwork(t *testing.T) {
	t.Parallel()

	model, err := NewModelRef(OpenAIProviderID, OpenAIResponsesAPI, "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bad.name", strings.Repeat("a", 65)} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			transport := &rejectNetworkDoer{}
			implementation, err := NewOpenAIResponsesProvider(OpenAIResponsesConfig{
				BaseURL: "https://fixture.test/v1",
				APIKey:  "secret",
				Client:  transport,
			})
			if err != nil {
				t.Fatal(err)
			}
			// Build the invalid value inside the package to exercise the provider's
			// defensive revalidation independently of constructor admission.
			request := Request{
				model: model,
				tools: []ToolDefinition{{
					name:        name,
					description: "invalid function name",
					strict:      true,
					parameters:  []byte(`{"type":"object"}`),
				}},
			}
			stream := implementation.Stream(context.Background(), request)
			event, err := stream.Next()
			if err != nil || event == nil {
				t.Fatalf("preflight failure = (%T, %v), want error event", event, err)
			}
			failure, ok := event.(llm.ErrorEvent)
			if !ok || !errors.Is(failure.Failure(), ErrInvalidRequest) || !errors.Is(failure.Failure(), ErrInvalidToolDefinition) {
				t.Fatalf("preflight event = %#v, want invalid request/tool definition", event)
			}
			if transport.calls != 0 {
				t.Fatalf("HTTP calls = %d, want 0", transport.calls)
			}
			if err := stream.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
		})
	}
}

type rejectNetworkDoer struct{ calls int }

func (d *rejectNetworkDoer) Do(*http.Request) (*http.Response, error) {
	d.calls++
	return nil, errors.New("network must not be called")
}
