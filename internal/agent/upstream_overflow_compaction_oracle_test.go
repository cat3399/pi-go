package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

type upstreamOverflowCompactionResponse struct {
	Text         string `json:"text"`
	InputTokens  uint64 `json:"inputTokens"`
	OutputTokens uint64 `json:"outputTokens"`
}

type upstreamOverflowCompactionScenario struct {
	Name             string                             `json:"name"`
	SessionID        string                             `json:"sessionId"`
	SystemPrompt     string                             `json:"systemPrompt"`
	FirstPrompt      string                             `json:"firstPrompt"`
	OverflowPrompt   string                             `json:"overflowPrompt"`
	ErrorMessage     string                             `json:"errorMessage"`
	ReserveTokens    uint64                             `json:"reserveTokens"`
	KeepRecentTokens uint64                             `json:"keepRecentTokens"`
	SeedResponse     upstreamOverflowCompactionResponse `json:"seedResponse"`
	SummaryResponse  upstreamOverflowCompactionResponse `json:"summaryResponse"`
	RecoveryResponse upstreamOverflowCompactionResponse `json:"recoveryResponse"`
}

// The upstream overflow scenario still exercises durable failure, continuation
// and reopen; pi-go now recovers without making an isolated summary request.
func TestOverflowHandoffReusesUpstreamWorkflowScenario(t *testing.T) {
	var corpus upstreamWorkflowCorpus
	if err := json.Unmarshal(upstreamWorkflowCorpusJSON, &corpus); err != nil {
		t.Fatal(err)
	}
	scenario := corpus.OverflowCompaction
	response := func(value upstreamOverflowCompactionResponse) llm.AssistantTerminal {
		message, err := newAssistantTextMessage([]llm.TextBlock{mustTextBlock(t, value.Text)}, llm.FinishStop, mustUsage(t, value.InputTokens, value.OutputTokens), agentTestEpoch.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return message
	}
	typed, _ := provider.NewProviderFailure(provider.ProviderFailureSpec{Kind: provider.FailureContextOverflow, Message: scenario.ErrorMessage, Cause: errors.New("overflow fixture")})
	cause, _ := llm.NewFailure(scenario.ErrorMessage, typed)
	failed, _ := newAssistantFailureMessageWithFailure(nil, llm.FinishError, cause, llm.Usage{}, agentTestEpoch.Add(time.Hour))
	implementation := newScriptedProvider(t, response(scenario.SeedResponse), failed, response(scenario.RecoveryResponse))
	manager := newSessionManager(t)
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: implementation, SessionManager: manager, Model: handoffModel(t), SystemPrompt: scenario.SystemPrompt,
		Stream: provider.StreamOptions{SessionID: scenario.SessionID}, ContextReserve: scenario.ReserveTokens})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	_ = runtime.ConfigureContextAccess(handoffInstructions(), nil, nil)
	starts, ends, settlements := 0, 0, 0
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch event := event.(type) {
		case agent.CompactionStartEvent:
			starts++
			if event.Reason != agent.CompactionContextOverflow || !event.WillRetry {
				t.Error("overflow start metadata")
			}
		case agent.CompactionEndEvent:
			ends++
			if event.Result == nil || !event.WillRetry || event.Aborted || event.ErrorMessage != "" {
				t.Error("overflow settlement metadata")
			}
		case agent.AgentSettledEvent:
			settlements++
		}
	})
	if result, err := runtime.Prompt(context.Background(), scenario.FirstPrompt); err != nil || !result.Succeeded() {
		t.Fatalf("seed: %#v %v", result, err)
	}
	recovered, err := runtime.Prompt(context.Background(), scenario.OverflowPrompt)
	if err != nil || !recovered.Succeeded() || recovered.ProviderTurns() != 2 {
		t.Fatalf("recovery: %#v %v", recovered, err)
	}
	if implementation.CallCount() != 3 || starts != 1 || ends != 1 || settlements != 2 {
		t.Fatalf("calls/events: %d %d %d %d", implementation.CallCount(), starts, ends, settlements)
	}
	request := implementation.Requests()[2]
	text := contextRequestText(t, request)
	if !strings.Contains(text, scenario.OverflowPrompt) || !strings.Contains(text, "No handoff was generated") || !strings.Contains(text, "search-original-history") || strings.Contains(text, scenario.SeedResponse.Text) {
		t.Fatal("invalid recovery context: ", text)
	}
	if request.SystemPrompt() != scenario.SystemPrompt || request.StreamOptions().SessionID != scenario.SessionID {
		t.Fatal("recovery changed runtime configuration")
	}
	var checkpoint session.Entry
	failures, users := 0, 0
	for _, entry := range manager.Entries() {
		if handoff, ok := entry.ContextHandoff(); ok {
			if !handoff.NoHandoff || len(handoff.PendingEntryIDs) != 1 {
				t.Fatal("overflow input not referenced")
			}
			checkpoint = entry
		}
		if role, _ := entry.MessageRole(); role == "user" {
			users++
		}
		if message, ok := entry.Message(); ok {
			if _, failed := message.(llm.AssistantFailureMessage); failed {
				failures++
			}
		}
	}
	if checkpoint.ID() == "" || users != 2 || failures != 1 {
		t.Fatalf("durable recovery checkpoint/users/failures: %s %d %d", checkpoint.ID(), users, failures)
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
	if window, _ := reopened.CurrentContextWindow(); window != checkpoint.ID() {
		t.Fatal("restart lost overflow boundary")
	}
	if got := len(reopened.BuildContext().Messages()); got != 3 {
		t.Fatalf("reopened recovery messages: %d", got)
	}
	for _, message := range reopened.BuildContext().Messages() {
		if _, failed := message.(llm.AssistantFailureMessage); failed {
			t.Fatal("failed response returned to active context")
		}
	}
}
