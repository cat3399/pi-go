package provider_test

import (
	"context"
	"strings"
	"testing"
	"time"

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
			options.CacheRetention != provider.CacheRetentionNone || options.MaxTokens == nil || *options.MaxTokens != 1 {
			t.Fatalf("isolated summary options = %#v", options)
		}
	}
}
