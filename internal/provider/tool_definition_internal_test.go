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

func TestOpenAIResponsesRejectsInvalidStrictSchemaBeforeNetwork(t *testing.T) {
	t.Parallel()

	model, err := NewModelRef(OpenAIProviderID, OpenAIResponsesAPI, "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		schema []byte
	}{
		{name: "missing object closure", schema: []byte(`{"type":"object","properties":{},"required":[]}`)},
		{name: "optional property", schema: []byte(`{"type":"object","additionalProperties":false,"properties":{"optional":{"type":"string"}},"required":[]}`)},
		{name: "missing reference", schema: []byte(`{"type":"object","additionalProperties":false,"properties":{"value":{"$ref":"#/$defs/missing"}},"required":["value"],"$defs":{}}`)},
		{name: "invalid pointer", schema: []byte(`{"type":"object","additionalProperties":false,"properties":{"value":{"$ref":"#/$defs/value~2"}},"required":["value"],"$defs":{}}`)},
		{name: "non-schema reference target", schema: []byte(`{"type":"object","additionalProperties":false,"properties":{"value":{"$ref":"#/required"}},"required":["value"]}`)},
		{name: "referenced object non-boolean closure", schema: []byte(`{"type":"object","additionalProperties":false,"properties":{"value":{"$ref":"#/x-target"}},"required":["value"],"x-target":{"type":"object","additionalProperties":null,"properties":{},"required":[]}}`)},
	} {
		transport := &rejectNetworkDoer{}
		implementation, err := NewOpenAIResponsesProvider(OpenAIResponsesConfig{
			BaseURL: "https://fixture.test/v1",
			APIKey:  "secret",
			Client:  transport,
		})
		if err != nil {
			t.Fatal(err)
		}
		request := Request{
			model: model,
			tools: []ToolDefinition{{
				name:        "strict_tool",
				description: "invalid strict schema",
				strict:      true,
				parameters:  test.schema,
			}},
		}
		stream := implementation.Stream(context.Background(), request)
		event, err := stream.Next()
		if err != nil || event == nil {
			t.Fatalf("%s preflight failure = (%T, %v), want error event", test.name, event, err)
		}
		failure, ok := event.(llm.ErrorEvent)
		if !ok || !errors.Is(failure.Failure(), ErrInvalidRequest) || !errors.Is(failure.Failure(), ErrInvalidToolDefinition) {
			t.Fatalf("%s preflight event = %#v, want invalid request/tool definition", test.name, event)
		}
		if transport.calls != 0 {
			t.Fatalf("%s HTTP calls = %d, want 0", test.name, transport.calls)
		}
		if err := stream.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}
}

type rejectNetworkDoer struct{ calls int }

func (d *rejectNetworkDoer) Do(*http.Request) (*http.Response, error) {
	d.calls++
	return nil, errors.New("network must not be called")
}
