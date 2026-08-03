package provider_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

// This contract test intentionally uses no OpenAI API name, response ID, or
// wire metadata. It guards the boundary that lets another provider be added
// without changing AgentLoop's request/message contracts.
func TestP0GenericProviderContractHasNoOpenAIShape(t *testing.T) {
	model, err := provider.NewModel(provider.ModelSpec{Provider: "local-generic", API: "pi-messages", ID: "model", Name: "Generic", Input: []provider.InputKind{provider.InputText, provider.InputImage}, Reasoning: true, Cost: provider.CostRates{Input: 1, Output: 2, CacheRead: .5, CacheWrite: 3, Tiers: []provider.CostTier{{InputTokensAbove: 100, Input: 4, Output: 5, CacheRead: 6, CacheWrite: 7}}}, Compat: provider.ModelCompat{Additional: map[string]json.RawMessage{"pi-messages": json.RawMessage(`{"native":true}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	usage, err := llm.NewUsage(llm.UsageSpec{Input: 101, Output: 2, CacheRead: 3, CacheWrite: 4})
	if err != nil {
		t.Fatal(err)
	}
	cost := model.CalculateCost(usage)
	if cost.Input != 4.0*101/1_000_000 || cost.Output != 5.0*2/1_000_000 {
		t.Fatalf("tier cost=%#v", cost)
	}
	user, err := llm.NewUserTextMessage("hello", time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	req, err := provider.NewRequestWithOptions(model, "system", []llm.ConversationMessage{user}, provider.RequestOptions{ThinkingLevel: provider.ThinkingHigh, Stream: provider.StreamOptions{Transport: provider.TransportWebsocket, CacheRetention: provider.CacheRetentionLong, Metadata: map[string]any{"tenant": "local"}, Env: map[string]string{"GENERIC": "1"}, Extra: map[string]any{"dialect": "native"}}})
	if err != nil {
		t.Fatal(err)
	}
	stream := (&genericProvider{}).Stream(context.Background(), req)
	defer stream.Close()
	event, err := stream.Next()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := event.(llm.StartEvent); !ok {
		t.Fatalf("start=%T", event)
	}
}

type genericProvider struct{}

func (*genericProvider) Stream(_ context.Context, request provider.Request) provider.EventStream {
	return &genericStream{events: []llm.StreamEvent{llm.NewStartEvent(), mustDone(request)}}
}

type genericStream struct{ events []llm.StreamEvent }

func (s *genericStream) Next() (llm.StreamEvent, error) {
	if len(s.events) == 0 {
		return nil, context.Canceled
	}
	event := s.events[0]
	s.events = s.events[1:]
	return event, nil
}
func (*genericStream) Close() error { return nil }
func mustDone(request provider.Request) llm.DoneEvent {
	usage, _ := llm.NewUsage(llm.UsageSpec{})
	event, err := llm.NewDoneEvent(llm.FinishStop, usage, time.UnixMilli(2))
	if err != nil {
		panic(err)
	}
	if request.Model().Provider() != "local-generic" {
		panic("wrong generic model")
	}
	return event
}
