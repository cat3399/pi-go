package agent_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestSessionRetriesRemoteFailuresAndPersistsEvidence(t *testing.T) {
	for _, scenario := range []struct{ name, body string }{
		{"server error in stream", "data: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"please retry your request\"}\n\n"},
		{"invalid JSON", "data: {\"type\":invalid}\n\n"},
		{"premature end", "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n"},
		{"partial frame", "data: {\"type\":"},
		{"incomplete tool arguments", "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"echo\",\"arguments\":\"\"}}\n\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"item_id\":\"fc_1\",\"delta\":\"{\\\"value\\\":\"}\n\n"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var calls atomic.Uint32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, scenario.body)
					return
				}
				body, _ := io.ReadAll(request.Body)
				if strings.Contains(string(body), "please retry your request") || strings.Contains(string(body), "call_1") {
					t.Errorf("failed response was sent back to the model: %s", body)
				}
				writeContextSSE(t, w, "recovered")
			}))
			defer server.Close()
			model, implementation := contextRetryProvider(t, server.URL)
			manager := newSessionManager(t)
			coordinator, err := agent.NewSession(agent.SessionConfig{
				Provider: implementation, Model: model, SessionManager: manager,
				Stream: provider.StreamOptions{DiagnosticsDir: t.TempDir()},
				Retry:  provider.RetryPolicy{MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil }},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := coordinator.Run(context.Background(), "retry this model response")
			if err != nil || !result.Succeeded() || calls.Load() != 2 {
				t.Fatalf("result=%v calls=%d err=%v", result, calls.Load(), err)
			}
			assertContextRetryEntries(t, manager, "retry this model response", "recovered", 0, 1)
			var recordPath string
			for _, entry := range manager.Entries() {
				message, ok := entry.Message()
				if !ok {
					continue
				}
				failure, ok := message.(llm.AssistantFailureMessage)
				if !ok {
					continue
				}
				if len(failure.Diagnostics()) != 1 {
					t.Fatalf("failure diagnostics=%v", failure.Diagnostics())
				}
				var details struct{ RecordPath, ResponsePath string }
				if err := json.Unmarshal(failure.Diagnostics()[0].Details(), &details); err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(details.ResponsePath)
				if err != nil || string(raw) != scenario.body {
					t.Fatalf("original failed stream=%q err=%v", raw, err)
				}
				recordPath = details.RecordPath
			}
			path, _ := manager.SessionFile()
			persisted, err := os.ReadFile(path)
			if err != nil || recordPath == "" || !strings.Contains(string(persisted), recordPath) {
				t.Fatalf("session did not persist diagnostic reference: %s err=%v", persisted, err)
			}
		})
	}
}

func TestMalformedModelResponseRetryBudgetAndDisable(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "exhausted"}[enabled], func(t *testing.T) {
			var calls atomic.Uint32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: invalid\n\n")
			}))
			defer server.Close()
			model, implementation := contextRetryProvider(t, server.URL)
			coordinator, err := agent.NewSession(agent.SessionConfig{
				Provider: implementation, Model: model, SessionManager: newSessionManager(t), AutoRetryEnabled: &enabled,
				Retry: provider.RetryPolicy{MaxAttempts: 4, Sleep: func(context.Context, time.Duration) error { return nil }},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := coordinator.Run(context.Background(), "try")
			want := uint32(1)
			if enabled {
				want = 4
			}
			if err != nil || result.Succeeded() || calls.Load() != want {
				t.Fatalf("result=%v calls=%d want=%d err=%v", result, calls.Load(), want, err)
			}
		})
	}
}
