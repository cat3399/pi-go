package provider_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

func TestContextSummarizerCreatesFreshIsolatedRequestPerSummarize(t *testing.T) {
	implementation := mustProvider(t, provider.ScriptedConfig{})
	mustSetResponses(t, implementation,
		mustFixedStep(t, mustTextTerminal(t, "first summary")),
		mustFixedStep(t, mustTextTerminal(t, "second summary")),
	)
	model, err := newModel(provider.ModelSpec{
		Provider: "openai", API: provider.OpenAIResponsesAPI, ID: "summary-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	zero := uint64(0)
	summarizer, err := provider.NewContextSummarizerWithOptions(implementation, model, func() time.Time { return responsesTestTime }, provider.ContextSummarizerOptions{
		Stream: provider.StreamOptions{SessionID: "durable-session", CacheRetention: provider.CacheRetentionShort, MaxTokens: &zero},
		Retry:  provider.RetryPolicy{MaxAttempts: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"first", "second"} {
		output, summarizeErr := summarizer.Summarize(context.Background(), session.SummaryInput{SystemPrompt: "summarize", Prompt: prompt})
		if summarizeErr != nil || output.Text == "" {
			t.Fatalf("Summarize(%q) = %#v, %v", prompt, output, summarizeErr)
		}
	}
	requests := implementation.Requests()
	if len(requests) != 2 {
		t.Fatalf("requests = %d", len(requests))
	}
	first := requests[0].StreamOptions()
	second := requests[1].StreamOptions()
	if first.SessionID == "" || second.SessionID == "" || first.SessionID == second.SessionID ||
		first.SessionID == "durable-session" || second.SessionID == "durable-session" {
		t.Fatalf("summary session IDs = %q / %q", first.SessionID, second.SessionID)
	}
	for _, options := range []provider.StreamOptions{first, second} {
		if len(options.SessionID) != 36 || options.SessionID[14] != '7' || !strings.ContainsRune("89ab", rune(options.SessionID[19])) ||
			options.CacheRetention != provider.CacheRetentionNone || options.MaxTokens == nil || *options.MaxTokens != 0 {
			t.Fatalf("isolated summary options = %#v", options)
		}
	}
}

func summaryAgentMessage(t *testing.T, text string) agentmsg.Message {
	t.Helper()
	message, err := llm.NewUserTextMessage(text, responsesTestTime)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := agentmsg.NewLLM(message)
	if err != nil {
		t.Fatal(err)
	}
	return wrapped
}

func summaryTerminalWithUsage(t *testing.T, text string, input, output uint64) llm.AssistantTextMessage {
	t.Helper()
	usage, err := llm.NewUsage(llm.UsageSpec{Input: input, Output: output})
	if err != nil {
		t.Fatal(err)
	}
	message, err := newAssistantTextMessage([]llm.TextBlock{mustTextBlock(t, text)}, llm.FinishStop, usage, responsesTestTime)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func TestContextSummarizerSplitTurnUsesTwoIsolatedCallsAndPiBudgets(t *testing.T) {
	implementation := mustProvider(t, provider.ScriptedConfig{})
	mustSetResponses(t, implementation,
		mustFixedStep(t, summaryTerminalWithUsage(t, "history", 10, 2)),
		mustFixedStep(t, summaryTerminalWithUsage(t, "prefix", 3, 1)),
	)
	model, err := newModel(provider.ModelSpec{
		Provider: "openai", API: provider.OpenAIResponsesAPI, ID: "split-model", MaxTokens: 1_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	summarizer, err := provider.NewContextSummarizerWithRetry(implementation, model, func() time.Time { return responsesTestTime }, provider.RetryPolicy{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	output, err := summarizer.Summarize(context.Background(), session.SummaryInput{
		SystemPrompt: "system", Prompt: "history prompt", TurnPrefixPrompt: "prefix prompt",
		MessagesToSummarize: []agentmsg.Message{summaryAgentMessage(t, "history")},
		TurnPrefixMessages:  []agentmsg.Message{summaryAgentMessage(t, "prefix")},
		IsSplitTurn:         true, Settings: session.CompactionSettings{Enabled: true, ReserveTokens: 101, KeepRecentTokens: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "history\n\n---\n\n**Turn Context (split turn):**\n\nprefix"
	if output.Text != want || output.Usage == nil || output.Usage.Usage.Input() != 13 || output.Usage.Usage.Output() != 3 || output.Usage.Usage.TotalTokens() != 16 {
		t.Fatalf("split output = %#v", output)
	}
	requests := implementation.Requests()
	if len(requests) != 2 {
		t.Fatalf("requests = %d", len(requests))
	}
	first, second := requests[0].StreamOptions(), requests[1].StreamOptions()
	if first.SessionID == "" || second.SessionID == "" || first.SessionID == second.SessionID ||
		first.CacheRetention != provider.CacheRetentionNone || second.CacheRetention != provider.CacheRetentionNone ||
		first.MaxTokens == nil || *first.MaxTokens != 80 || second.MaxTokens == nil || *second.MaxTokens != 50 {
		t.Fatalf("split request options = %#v / %#v", first, second)
	}
	if requests[0].SystemPrompt() != "system" || requests[1].SystemPrompt() != "system" ||
		requestUserText(t, requests[0]) != "history prompt" || requestUserText(t, requests[1]) != "prefix prompt" {
		t.Fatalf("split prompts = %#v / %#v", requests[0], requests[1])
	}
}

func TestContextSummarizerJoinsResponseTextBlocksLikeContentText(t *testing.T) {
	implementation := mustProvider(t, provider.ScriptedConfig{})
	rich, err := newAssistantRichMessage([]llm.AssistantBlock{
		mustTextBlock(t, "first"), mustTextBlock(t, "second"),
	}, llm.FinishStop, llm.Usage{}, responsesTestTime)
	if err != nil {
		t.Fatal(err)
	}
	mustSetResponses(t, implementation, mustFixedStep(t, rich))
	model, err := newModel(provider.ModelSpec{Provider: "openai", API: provider.OpenAIResponsesAPI, ID: "summary-model"})
	if err != nil {
		t.Fatal(err)
	}
	summarizer, err := provider.NewContextSummarizerWithRetry(implementation, model, nil, provider.RetryPolicy{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	output, err := summarizer.Summarize(context.Background(), session.SummaryInput{SystemPrompt: "system", Prompt: "prompt"})
	if err != nil || output.Text != "first\nsecond" {
		t.Fatalf("Summarize() = %#v, %v", output, err)
	}
}

func TestContextSummarizerSplitWithoutHistoryUsesPiPlaceholderAndOneCall(t *testing.T) {
	implementation := mustProvider(t, provider.ScriptedConfig{})
	mustSetResponses(t, implementation, mustFixedStep(t, mustTextTerminal(t, "prefix")))
	model, err := newModel(provider.ModelSpec{Provider: "openai", API: provider.OpenAIResponsesAPI, ID: "split-model", MaxTokens: 40})
	if err != nil {
		t.Fatal(err)
	}
	summarizer, err := provider.NewContextSummarizerWithRetry(implementation, model, func() time.Time { return responsesTestTime }, provider.RetryPolicy{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	output, err := summarizer.Summarize(context.Background(), session.SummaryInput{
		SystemPrompt: "system", Prompt: "must not run", TurnPrefixPrompt: "prefix prompt",
		TurnPrefixMessages: []agentmsg.Message{summaryAgentMessage(t, "prefix")}, IsSplitTurn: true,
		Settings: session.CompactionSettings{Enabled: true, ReserveTokens: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if output.Text != "No prior history.\n\n---\n\n**Turn Context (split turn):**\n\nprefix" {
		t.Fatalf("output = %q", output.Text)
	}
	requests := implementation.Requests()
	if len(requests) != 1 || requestUserText(t, requests[0]) != "prefix prompt" || requests[0].StreamOptions().MaxTokens == nil || *requests[0].StreamOptions().MaxTokens != 40 {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestContextSummarizerSplitFailureReturnsNoPartialSummary(t *testing.T) {
	for _, test := range []struct {
		name          string
		responses     []provider.ScriptStep
		wantRequests  int
		wantErrorPart string
	}{
		{name: "history", responses: []provider.ScriptStep{mustFixedStep(t, mustSummaryFailure(t, "history failed"))}, wantRequests: 1, wantErrorPart: "history failed"},
		{name: "prefix", responses: []provider.ScriptStep{
			mustFixedStep(t, mustTextTerminal(t, "history")), mustFixedStep(t, mustSummaryFailure(t, "prefix failed")),
		}, wantRequests: 2, wantErrorPart: "turn prefix summarization"},
	} {
		t.Run(test.name, func(t *testing.T) {
			implementation := mustProvider(t, provider.ScriptedConfig{})
			mustSetResponses(t, implementation, test.responses...)
			model, err := newModel(provider.ModelSpec{Provider: "openai", API: provider.OpenAIResponsesAPI, ID: "failure-model", MaxTokens: 100})
			if err != nil {
				t.Fatal(err)
			}
			summarizer, err := provider.NewContextSummarizerWithRetry(implementation, model, func() time.Time { return responsesTestTime }, provider.RetryPolicy{MaxAttempts: 1})
			if err != nil {
				t.Fatal(err)
			}
			output, err := summarizer.Summarize(context.Background(), session.SummaryInput{
				SystemPrompt: "system", Prompt: "history", TurnPrefixPrompt: "prefix",
				MessagesToSummarize: []agentmsg.Message{summaryAgentMessage(t, "history")},
				TurnPrefixMessages:  []agentmsg.Message{summaryAgentMessage(t, "prefix")}, IsSplitTurn: true,
				Settings: session.CompactionSettings{Enabled: true, ReserveTokens: 100},
			})
			if err == nil || !strings.Contains(err.Error(), test.wantErrorPart) || output.Text != "" || len(implementation.Requests()) != test.wantRequests {
				t.Fatalf("Summarize() = %#v, %v; requests=%d", output, err, len(implementation.Requests()))
			}
		})
	}
}

func mustSummaryFailure(t *testing.T, message string) llm.AssistantFailureMessage {
	t.Helper()
	cause := errors.New(message)
	failure, err := llm.NewFailure(message, cause)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := newAssistantFailureMessageWithFailure(nil, llm.FinishError, failure, llm.Usage{}, responsesTestTime)
	if err != nil {
		t.Fatal(err)
	}
	return terminal
}

func requestUserText(t *testing.T, request provider.Request) string {
	t.Helper()
	messages := request.Messages()
	if len(messages) != 1 {
		t.Fatalf("request messages = %#v", messages)
	}
	user, ok := messages[0].(llm.UserTextMessage)
	if !ok {
		t.Fatalf("request message = %T", messages[0])
	}
	var result strings.Builder
	for _, block := range user.Content() {
		result.WriteString(block.Text())
	}
	return result.String()
}
