package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

// This drives the real OpenAI request/SSE adapter through a local server. The
// first request is the Session summary, the second gets a transient 503, and
// the third succeeds. No retry may duplicate any durable conversation entry.
func TestContextThresholdCompactsThenRetriesProductionProviderWithoutDuplicateTranscript(t *testing.T) {
	transcript := newSession(t)
	old, err := llm.NewUserTextMessage(strings.Repeat("old context ", 12), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.Append(context.Background(), old, session.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Uint32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer fixture-key" {
			t.Errorf("authorization = %q", got)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode: %v", err)
		}
		switch calls.Add(1) {
		case 1:
			writeContextSSE(t, w, "summary checkpoint")
		case 2:
			// A truncated chunked SSE response reaches the adapter as a transport
			// stream drop (not a parse/auth failure), so the retry decision is
			// exercised through the same local HTTP path as production.
			dropContextSSE(t, w)
		case 3:
			writeContextSSE(t, w, "final answer")
		default:
			t.Errorf("unexpected provider request %d", calls.Load())
		}
	}))
	defer server.Close()
	model, err := provider.NewModelRef(provider.OpenAIProviderID, provider.OpenAIResponsesAPI, "fixture-model")
	if err != nil {
		t.Fatal(err)
	}
	implementation, err := provider.NewOpenAIResponsesProvider(provider.OpenAIResponsesConfig{BaseURL: server.URL + "/v1", APIKey: "fixture-key"})
	if err != nil {
		t.Fatal(err)
	}
	summarizer, err := provider.NewContextSummarizer(implementation, model, func() time.Time { return agentTestEpoch })
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := agent.New(agent.Config{
		Provider: implementation, Transcript: transcript, Model: model, Now: func() time.Time { return agentTestEpoch },
		ContextWindow: 2, ContextReserve: 1, KeepRecentTokens: 1, Summarizer: summarizer,
		Retry: agent.RetryPolicy{MaxAttempts: 2, InitialDelay: time.Millisecond, Sleep: func(context.Context, time.Duration) error { return nil }},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.Run(context.Background(), "new prompt")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.Succeeded() || calls.Load() != 3 {
		t.Fatalf("result succeeded=%v calls=%d", result.Succeeded(), calls.Load())
	}
	entries := transcript.Entries()
	var users, finals, compactions int
	for _, entry := range entries {
		if entry.Type() == "compaction" {
			compactions++
		}
		message, ok := entry.Message()
		if !ok {
			continue
		}
		switch value := message.(type) {
		case llm.UserTextMessage:
			if strings.Contains(joinContextText(value.Content()), "new prompt") {
				users++
			}
		case llm.AssistantTextMessage:
			if strings.Contains(joinContextText(value.Content()), "final answer") {
				finals++
			}
		}
	}
	if users != 1 || finals != 1 || compactions != 1 {
		t.Fatalf("users=%d finals=%d compactions=%d entries=%d", users, finals, compactions, len(entries))
	}
	if err := coordinator.WaitForIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func joinContextText(parts []llm.TextBlock) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text())
	}
	return b.String()
}

func writeContextSSE(t *testing.T, w http.ResponseWriter, text string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	events := []map[string]any{
		{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": "m", "role": "assistant", "content": []any{}}},
		{"type": "response.output_text.delta", "output_index": 0, "item_id": "m", "delta": text},
		{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "message", "id": "m", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text}}}},
		{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message"}}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}},
	}
	for _, event := range events {
		data, _ := json.Marshal(event)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			t.Error(err)
		}
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		t.Error(err)
	}
}

func dropContextSSE(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		t.Fatal("local response writer does not support hijacking")
	}
	connection, buffered, err := hijacker.Hijack()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := buffered.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n10\r\ndata: partial"); err != nil {
		t.Fatal(err)
	}
	if err := buffered.Flush(); err != nil {
		t.Fatal(err)
	}
}
