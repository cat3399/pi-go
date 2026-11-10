package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

type upstreamManualCompactionScenario struct {
	Name               string `json:"name"`
	SessionID          string `json:"sessionId"`
	SystemPrompt       string `json:"systemPrompt"`
	FirstPrompt        string `json:"firstPrompt"`
	SecondPrompt       string `json:"secondPrompt"`
	CustomInstructions string `json:"customInstructions"`
	ReserveTokens      uint64 `json:"reserveTokens"`
	KeepRecentTokens   uint64 `json:"keepRecentTokens"`
	Responses          []struct {
		Text         string `json:"text"`
		InputTokens  uint64 `json:"inputTokens"`
		OutputTokens uint64 `json:"outputTokens"`
	} `json:"responses"`
}

// Reuse the upstream task and responses, but assert the explicitly chosen
// pi-go policy: a current-model handoff, unchanged prefix and durable restart.
func TestManualHandoffReusesUpstreamWorkflowScenario(t *testing.T) {
	var corpus upstreamWorkflowCorpus
	if err := json.Unmarshal(upstreamWorkflowCorpusJSON, &corpus); err != nil {
		t.Fatal(err)
	}
	scenario := corpus.ManualCompaction
	if len(scenario.Responses) != 3 {
		t.Fatal("manual corpus must have two answers and one handoff response")
	}
	manager := newSessionManager(t)
	var responses []llm.AssistantTerminal
	for _, value := range scenario.Responses {
		message, err := newAssistantTextMessage([]llm.TextBlock{mustTextBlock(t, value.Text)}, llm.FinishStop, mustUsage(t, value.InputTokens, value.OutputTokens), agentTestEpoch)
		if err != nil {
			t.Fatal(err)
		}
		responses = append(responses, message)
	}
	responses[2] = mustToolUseTerminal(t, "submit", "work", []byte(`{}`))
	implementation := newScriptedProvider(t, responses...)
	definition, _ := provider.NewToolDefinition("work", "work", false, []byte(`{"type":"object"}`))
	var runtime *agent.AgentSession
	var err error
	runtime, err = agent.NewSession(agent.SessionConfig{Provider: implementation, SessionManager: manager, Model: handoffModel(t), SystemPrompt: scenario.SystemPrompt,
		Tools: []provider.ToolDefinition{definition}, Tool: &fakeTool{name: "work", execute: func(ctx context.Context, _ []byte, _ func(agent.ToolUpdate)) (agent.ToolOutput, error) {
			_, err := runtime.RequestContextChange(ctx, agent.ContextChangeRequest{Mode: "handoff", Handoff: scenario.Responses[2].Text})
			return agent.ToolOutput{Text: "accepted"}, err
		}},
		ContextReserve: scenario.ReserveTokens, KeepRecentTokens: scenario.KeepRecentTokens,
		Stream: provider.StreamOptions{SessionID: scenario.SessionID, Transport: provider.TransportSSE}, Now: func() time.Time { return agentTestEpoch }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	_ = runtime.ConfigureContextAccess(handoffInstructions(), nil, nil)
	for _, prompt := range []string{scenario.FirstPrompt, scenario.SecondPrompt} {
		if result, err := runtime.Prompt(context.Background(), prompt); err != nil || !result.Succeeded() {
			t.Fatalf("prompt: %#v %v", result, err)
		}
	}
	original := manager.Entries()
	var events []string
	unsubscribe := runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch event := event.(type) {
		case agent.CompactionStartEvent:
			events = append(events, "start")
			if event.Reason != agent.CompactionManual {
				t.Error("wrong reason")
			}
		case agent.CompactionEndEvent:
			events = append(events, "end")
			if event.Result == nil || event.Aborted || event.ErrorMessage != "" {
				t.Error("missing committed result")
			}
		}
	})
	result, err := runtime.Compact(context.Background(), scenario.CustomInstructions)
	unsubscribe()
	if err != nil || !result.Committed {
		t.Fatalf("compact: %#v %v", result, err)
	}
	if !reflect.DeepEqual(events, []string{"start", "end"}) {
		t.Fatal(events)
	}
	handoff, ok := result.Entry.ContextHandoff()
	if !ok || handoff.Text != scenario.Responses[2].Text || handoff.NoHandoff {
		t.Fatalf("checkpoint: %#v", handoff)
	}
	requests := implementation.Requests()
	if len(requests) != 3 {
		t.Fatalf("provider calls: %d", len(requests))
	}
	write := requests[2]
	if write.SystemPrompt() != scenario.SystemPrompt || write.StreamOptions().SessionID != scenario.SessionID || write.StreamOptions().Transport != provider.TransportSSE {
		t.Fatal("handoff changed request attribution")
	}
	choice, hasChoice := write.ToolChoice()
	if hasChoice && choice.Mode == "none" {
		t.Fatal("handoff disabled task tools")
	}
	text := contextRequestText(t, write)
	for _, originalText := range []string{scenario.FirstPrompt, scenario.SecondPrompt, scenario.Responses[0].Text, scenario.Responses[1].Text, scenario.CustomInstructions} {
		if !strings.Contains(text, originalText) {
			t.Fatalf("lost working prefix: %q", originalText)
		}
	}
	entries := manager.Entries()
	if len(entries) != len(original)+3 {
		t.Fatal("unexpected transcript writes")
	}
	for i, entry := range original {
		if !bytes.Equal(entry.RawJSON(), entries[i].RawJSON()) {
			t.Fatal("original history changed")
		}
	}
	if runtime.Activity().Phase != agent.PhaseIdle || len(runtime.State().Active.Messages()) != 1 {
		t.Fatal("manual handoff did not settle with only the checkpoint")
	}
	path, _ := manager.SessionFile()
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := session.OpenSessionManager(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if window, _ := reopened.CurrentContextWindow(); window != result.Entry.ID() {
		t.Fatal("restart lost window")
	}
	if len(reopened.BuildContext().AgentMessages()) != 1 {
		t.Fatal("restart retained old conversation")
	}
	history, err := reopened.ReadHistoryView(session.HistoryViewQuery{})
	if err != nil || len(history.Records) != 5 {
		t.Fatalf("original history after restart: %d %v", len(history.Records), err)
	}
}
