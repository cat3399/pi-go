package provider_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestOpenAIResponsesNormalizesRetryAfterWithoutLeakingHeaderParsing(t *testing.T) {
	clock := time.Date(2026, time.August, 2, 0, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		http.Error(w, `{"error":{"message":"busy"}}`, http.StatusTooManyRequests)
	}))
	defer server.Close()
	implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{BaseURL: server.URL, APIKey: "key", Clock: func() time.Time { return clock }})
	_, terminal, err := collectStreamResult(implementation.Stream(context.Background(), mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "hello")})))
	if err != nil {
		t.Fatal(err)
	}
	failure, ok := terminal.(llm.AssistantFailureMessage)
	if !ok {
		t.Fatalf("terminal = %T", terminal)
	}
	var providerFailure *provider.ProviderFailure
	if !errors.As(failure.Failure().Cause(), &providerFailure) {
		t.Fatalf("failure cause = %T", failure.Failure().Cause())
	}
	if delay, ok := providerFailure.RetryAfter(); !ok || delay != 7*time.Second {
		t.Fatalf("RetryAfter() = (%s, %v)", delay, ok)
	}
}
