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

func TestP0RequestPreservesPortableOptionsAndDeferredToolPresence(t *testing.T) {
	model, err := provider.NewModel(provider.ModelSpec{Provider: "generic", API: "generic-api", ID: "model", Cost: provider.CostRates{Input: 1, Output: 2}})
	if err != nil {
		t.Fatal(err)
	}
	text, err := llm.NewTextBlock("done")
	if err != nil {
		t.Fatal(err)
	}
	result, err := llm.NewToolResultMessageWithMetadata("call", "tool", []llm.TextBlock{text}, false, time.UnixMilli(1), llm.ToolResultMetadata{AddedToolNames: []string{}, HasAddedToolNames: true})
	if err != nil {
		t.Fatal(err)
	}
	call, err := llm.NewToolCallBlock("call", "tool", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := llm.NewAssistantToolUseMessage([]llm.AssistantBlock{call}, llm.Usage{}, time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	deleteHeader := (*string)(nil)
	request, err := provider.NewRequestWithOptions(model, "", []llm.ConversationMessage{assistant, result}, provider.RequestOptions{Stream: provider.StreamOptions{Transport: provider.TransportWebsocket, ThinkingBudgets: map[provider.ThinkingLevel]uint64{provider.ThinkingHigh: 7}, HeaderOverrides: map[string]*string{"Authorization": deleteHeader}, Metadata: map[string]any{"nested": map[string]any{"value": "keep"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if names, present := request.DeferredToolNames(); !present || names == nil || len(names) != 0 {
		t.Fatalf("deferred = %#v, %t", names, present)
	}
	if budget, ok := request.ThinkingBudget(provider.ThinkingHigh); !ok || budget != 7 {
		t.Fatalf("budget = %d, %t", budget, ok)
	}
	options := request.StreamOptions()
	if value, ok := options.HeaderOverrides["Authorization"]; !ok || value != nil {
		t.Fatalf("header deletion lost: %#v", options.HeaderOverrides)
	}
	metadata := request.StreamOptions().Metadata
	metadata["nested"].(map[string]any)["value"] = "mutated"
	if request.StreamOptions().Metadata["nested"].(map[string]any)["value"] != "keep" {
		t.Fatal("metadata was not deeply cloned")
	}
	usage, err := llm.NewUsage(llm.UsageSpec{Input: 3, Output: 4})
	if err != nil {
		t.Fatal(err)
	}
	costUsage, err := usage.WithCost(model.CalculateCost(usage))
	if err != nil {
		t.Fatal(err)
	}
	if cost, ok := costUsage.Cost(); !ok || cost.Total != 11e-6 {
		t.Fatalf("cost = %#v, %t", cost, ok)
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
