package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestOpenAIResponsesRetryAfterDeltaDatePastAndMalformed(t *testing.T) {
	clock := time.Date(2026, time.August, 2, 1, 2, 3, 0, time.UTC)
	tests := []struct {
		name   string
		header string
		want   time.Duration
		has    bool
	}{
		{name: "seconds", header: "17", want: 17 * time.Second, has: true},
		{name: "zero seconds", header: "0", has: true},
		{name: "future date", header: clock.Add(23 * time.Second).Format(http.TimeFormat), want: 23 * time.Second, has: true},
		{name: "past date", header: clock.Add(-time.Second).Format(http.TimeFormat)},
		{name: "equal date", header: clock.Format(http.TimeFormat)},
		{name: "malformed", header: "tomorrow-ish"},
		{name: "negative", header: "-1"},
		{name: "signed positive", header: "+17"},
		{name: "signed negative zero", header: "-0"},
		{name: "embedded whitespace", header: "1 7"},
		{name: "unit suffix", header: "17s"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", testCase.header)
				http.Error(w, `{"error":{"message":"busy"}}`, http.StatusTooManyRequests)
			}))
			defer server.Close()
			implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{BaseURL: server.URL, APIKey: "key", Clock: func() time.Time { return clock }})
			failure := responsesProviderFailure(t, implementation)
			got, has := failure.RetryAfter()
			if has != testCase.has || got != testCase.want {
				t.Fatalf("RetryAfter() = (%s, %v), want (%s, %v)", got, has, testCase.want, testCase.has)
			}
		})
	}
}

func TestRetryControllerDefaultCapJitterAndCancellation(t *testing.T) {
	status := http.StatusTooManyRequests
	serverDelay := 5 * time.Minute
	failure, err := provider.NewProviderFailure(provider.ProviderFailureSpec{
		Kind: provider.FailureHTTPStatus, Message: "busy", Cause: errors.New("fixture"), HTTPStatus: &status, RetryAfter: &serverDelay,
	})
	if err != nil {
		t.Fatal(err)
	}
	var jitterAttempt uint32
	controller, err := provider.NewRetryController(provider.RetryPolicy{
		MaxAttempts: 3, InitialDelay: time.Second, MaxDelay: 10 * time.Second,
		Jitter: func(attempt uint32, _ time.Duration) time.Duration { jitterAttempt = attempt; return 2 * time.Minute },
	})
	if err != nil {
		t.Fatal(err)
	}
	if delay := controller.Delay(2, failure); delay != provider.DefaultMaxRetryAfter || jitterAttempt != 2 {
		t.Fatalf("Delay() = %s, jitter attempt=%d", delay, jitterAttempt)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("stop retry")
	cancel(cause)
	if err := controller.Wait(ctx, time.Hour); !errors.Is(err, cause) {
		t.Fatalf("Wait() error = %v", err)
	}
	invalid := []provider.RetryPolicy{{InitialDelay: -1}, {MaxDelay: -1}, {MaxRetryAfter: -1}}
	for _, policy := range invalid {
		if _, err := provider.NewRetryController(policy); !errors.Is(err, provider.ErrInvalidRetryPolicy) {
			t.Fatalf("NewRetryController(%+v) error = %v", policy, err)
		}
	}
}

func TestTransientFailureAdmissionMatrix(t *testing.T) {
	for _, status := range []int{408, 409, 425, 429, 500, 503, 599} {
		status := status
		failure, err := provider.NewProviderFailure(provider.ProviderFailureSpec{
			Kind: provider.FailureHTTPStatus, Message: "fixture", Cause: errors.New("fixture"), HTTPStatus: &status,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !provider.IsTransientFailure(failure) {
			t.Errorf("status %d was not transient", status)
		}
	}
	for _, status := range []int{400, 401, 403, 404, 422} {
		status := status
		failure, err := provider.NewProviderFailure(provider.ProviderFailureSpec{
			Kind: provider.FailureHTTPStatus, Message: "fixture", Cause: errors.New("fixture"), HTTPStatus: &status,
		})
		if err != nil {
			t.Fatal(err)
		}
		if provider.IsTransientFailure(failure) {
			t.Errorf("status %d was transient", status)
		}
	}
	for _, kind := range []provider.FailureKind{provider.FailureConfiguration, provider.FailureInvalidRequest, provider.FailureContextOverflow, provider.FailureInvalidResponse, provider.FailureCancelled} {
		failure, err := provider.NewProviderFailure(provider.ProviderFailureSpec{Kind: kind, Message: "fixture", Cause: errors.New("fixture")})
		if err != nil {
			t.Fatal(err)
		}
		if provider.IsTransientFailure(failure) {
			t.Errorf("kind %s was transient", kind)
		}
	}
}

func TestOpenAIContextOverflowClassificationIsSpecificAndSecretSafe(t *testing.T) {
	const secret = "sk-classifier-secret"
	tests := []struct {
		name      string
		status    int
		errorType string
		code      string
		message   string
		want      provider.FailureKind
	}{
		{name: "structured code takes priority", status: 400, errorType: "invalid_request_error", code: "context_length_exceeded", message: "max_output_tokens parameter rejected " + secret, want: provider.FailureContextOverflow},
		{name: "type", status: 400, errorType: "context_window_exceeded", message: "request rejected", want: provider.FailureContextOverflow},
		{name: "canonical messages result", status: 400, errorType: "invalid_request_error", message: "This model's maximum context length is 100 tokens. However, your messages resulted in 101 tokens. Please reduce the length of the messages.", want: provider.FailureContextOverflow},
		{name: "explicit input context window", status: 400, errorType: "invalid_request_error", message: "Your input exceeds the context window of this model. Please adjust your input and try again.", want: provider.FailureContextOverflow},
		{name: "explicit input length", status: 400, errorType: "invalid_request_error", message: "Input length (265330) exceeds model's maximum context length (262144).", want: provider.FailureContextOverflow},
		{name: "too many input tokens", status: 400, errorType: "invalid_request_error", message: "Too many input tokens for the maximum context length.", want: provider.FailureContextOverflow},
		{name: "prompt too long", status: 400, errorType: "invalid_request_error", message: "The prompt is too long for this model's context window.", want: provider.FailureContextOverflow},
		{name: "ordinary 400", status: 400, errorType: "invalid_request_error", code: "invalid_value", message: "context field is malformed", want: provider.FailureHTTPStatus},
		{name: "generic context wording", status: 400, errorType: "invalid_request_error", message: "This model's maximum context length is 100 tokens, but the request exceeded it", want: provider.FailureHTTPStatus},
		{name: "output limit", status: 400, errorType: "invalid_request_error", message: "Maximum output tokens exceed the context window", want: provider.FailureHTTPStatus},
		{name: "output token wording", status: 400, errorType: "invalid_request_error", message: "Your output tokens exceed the context window", want: provider.FailureHTTPStatus},
		{name: "max output parameter", status: 400, errorType: "invalid_request_error", message: "Your input exceeds the context window because the max_output_tokens parameter is invalid", want: provider.FailureHTTPStatus},
		{name: "max tokens parameter", status: 400, errorType: "invalid_request_error", message: "Invalid max_tokens parameter; maximum context length is 100", want: provider.FailureHTTPStatus},
		{name: "completion limit", status: 400, errorType: "invalid_request_error", message: "Completion tokens exceed the maximum context length", want: provider.FailureHTTPStatus},
		{name: "input parameter error", status: 400, errorType: "invalid_request_error", message: "The input exceeds the context window because parameter input is malformed", want: provider.FailureHTTPStatus},
		{name: "output code is not overflow", status: 400, errorType: "invalid_request_error", code: "max_output_tokens", message: "maximum context length exceeded", want: provider.FailureHTTPStatus},
		{name: "same code non-400", status: 429, code: "context_length_exceeded", message: "busy", want: provider.FailureHTTPStatus},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(testCase.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": testCase.errorType, "code": testCase.code, "message": testCase.message}})
			}))
			defer server.Close()
			implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{BaseURL: server.URL, APIKey: "key"})
			failure := responsesProviderFailure(t, implementation)
			if failure.Kind() != testCase.want {
				t.Fatalf("failure kind = %s, want %s", failure.Kind(), testCase.want)
			}
			if testCase.want == provider.FailureContextOverflow && (strings.Contains(failure.Error(), secret) || strings.Contains(fmt.Sprint(failure.Cause()), secret)) {
				t.Fatalf("classified failure leaked response body: failure=%q cause=%q", failure.Error(), failure.Cause())
			}
		})
	}
}

func responsesProviderFailure(t *testing.T, implementation provider.Provider) *provider.ProviderFailure {
	t.Helper()
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
		t.Fatalf("failure cause = %T (%s)", failure.Failure().Cause(), fmt.Sprint(failure.Failure().Cause()))
	}
	return providerFailure
}
