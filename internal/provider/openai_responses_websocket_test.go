package provider_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type responsesWSObservation struct {
	connection int32
	header     http.Header
	frame      map[string]any
}

func responsesWSTestServer(t *testing.T, rejectWS bool) (*httptest.Server, <-chan responsesWSObservation, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	observations := make(chan responsesWSObservation, 20)
	connections, posts := &atomic.Int32{}, &atomic.Int32{}
	var turns atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			posts.Add(1)
			if r.Header.Get("Content-Encoding") != "" {
				t.Errorf("standard Responses SSE unexpectedly compressed")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, codexCompressionSuccess())
			return
		}
		connectionID := connections.Add(1)
		if rejectWS {
			http.Error(w, "websocket unavailable", http.StatusServiceUnavailable)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.CloseNow()
		for {
			var frame map[string]any
			if err := wsjson.Read(r.Context(), conn, &frame); err != nil {
				return
			}
			observations <- responsesWSObservation{connection: connectionID, header: r.Header.Clone(), frame: frame}
			writeCodexWebSocketText(t, r.Context(), conn, int(turns.Add(1)), "answer")
		}
	}))
	return server, observations, connections, posts
}

func responsesWSModel(t *testing.T, name, baseURL string, supported *bool) provider.Model {
	t.Helper()
	model, err := newModel(provider.ModelSpec{
		Provider: name, API: provider.OpenAIResponsesAPI, ID: "gateway-model", BaseURL: baseURL + "/v1",
		Compat: provider.ModelCompat{OpenAIResponses: &provider.OpenAIResponsesCompat{SupportsWebsockets: supported}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func responsesWSRequest(t *testing.T, model provider.Model, messages []llm.ConversationMessage, options provider.StreamOptions) provider.Request {
	t.Helper()
	request, err := provider.NewRequestWithOptions(model, "system", messages, provider.RequestOptions{Stream: options})
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func TestOpenAIResponsesWebSocketAPIKeyContinuationAndEditedHistory(t *testing.T) {
	provider.ResetOpenAIResponsesWebSocketDebugStats("")
	defer provider.CloseOpenAIResponsesWebSocketSessions("")
	server, observations, connections, posts := responsesWSTestServer(t, false)
	defer server.Close()
	supported := true
	model := responsesWSModel(t, "custom-channel", server.URL, &supported)
	implementation, err := provider.NewOpenAIResponsesProvider(provider.OpenAIResponsesConfig{APIKey: "plain-api-key"})
	if err != nil {
		t.Fatal(err)
	}
	options := provider.StreamOptions{Transport: provider.TransportAuto, SessionID: "gateway-history"}
	messages := []llm.ConversationMessage{mustUser(t, "first")}
	for turn := 0; turn < 4; turn++ {
		if turn == 2 {
			messages[0] = mustUser(t, "edited first")
		}
		_, terminal := collectStream(t, implementation.Stream(context.Background(), responsesWSRequest(t, model, messages, options)))
		if terminalText(t, terminal) != "answer" {
			t.Fatalf("turn %d: %v", turn, terminal)
		}
		observed := <-observations
		if observed.header.Get("Authorization") != "Bearer plain-api-key" || observed.header.Get("Chatgpt-Account-Id") != "" || observed.frame["type"] != "response.create" {
			t.Fatalf("unexpected gateway handshake/frame")
		}
		if observed.header.Get("OpenAI-Beta") != "" {
			t.Fatal("native Codex beta header leaked into a standard Responses handshake")
		}
		if _, exists := observed.frame["stream"]; exists {
			t.Fatal("HTTP-only stream field leaked into WebSocket frame")
		}
		input := observed.frame["input"].([]any)
		if turn == 1 || turn == 3 {
			if observed.frame["previous_response_id"] != fmt.Sprintf("resp-%d", turn) || len(input) != 1 {
				t.Fatalf("turn %d was not an incremental continuation: %#v", turn, observed.frame)
			}
		} else if _, exists := observed.frame["previous_response_id"]; exists {
			t.Fatalf("turn %d reused invalid history", turn)
		} else if len(input) <= 1 {
			t.Fatalf("turn %d omitted the full context", turn)
		}
		messages = append(messages, terminal, mustUser(t, fmt.Sprintf("follow-up %d", turn)))
	}
	stats, ok := provider.GetOpenAIResponsesWebSocketDebugStats(options.SessionID)
	if !ok || connections.Load() != 1 || posts.Load() != 0 || stats.DeltaRequests != 2 || stats.FullContextRequests != 2 {
		t.Fatalf("transport stats = %#v, handshakes=%d posts=%d", stats, connections.Load(), posts.Load())
	}
}

func TestOpenAIResponsesWebSocketCapabilityAndExplicitTransport(t *testing.T) {
	yes, no := true, false
	for _, test := range []struct {
		name      string
		supported *bool
		transport provider.Transport
		wantWS    int32
	}{
		{"auto retains SSE default", nil, provider.TransportAuto, 0},
		{"auto uses declared capability", &yes, provider.TransportAuto, 1},
		{"capability disabled", &no, provider.TransportAuto, 0},
		{"explicit SSE", &yes, provider.TransportSSE, 0},
		{"explicit WebSocket opts in", nil, provider.TransportWebsocket, 1},
		{"explicit cached WebSocket opts in", nil, provider.TransportWebsocketCached, 1},
		{"explicitly unsupported provider", &no, provider.TransportWebsocket, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			defer provider.CloseOpenAIResponsesWebSocketSessions("")
			server, _, connections, posts := responsesWSTestServer(t, false)
			defer server.Close()
			implementation, err := provider.NewOpenAIResponsesProvider(provider.OpenAIResponsesConfig{APIKey: "ordinary-key"})
			if err != nil {
				t.Fatal(err)
			}
			model := responsesWSModel(t, "gateway", server.URL, test.supported)
			request := responsesWSRequest(t, model, []llm.ConversationMessage{mustUser(t, "hello")}, provider.StreamOptions{Transport: test.transport})
			_, terminal := collectStream(t, implementation.Stream(context.Background(), request))
			if terminal.FinishReason() != llm.FinishStop || connections.Load() != test.wantWS || posts.Load() != 1-test.wantWS {
				t.Fatalf("terminal/handshakes/posts = %v/%d/%d", terminal, connections.Load(), posts.Load())
			}
		})
	}
}

func TestOpenAIResponsesWebSocketIsolatesEndpointProviderAndCredentials(t *testing.T) {
	provider.ResetOpenAIResponsesWebSocketDebugStats("")
	defer provider.CloseOpenAIResponsesWebSocketSessions("")
	first, firstFrames, firstConnections, _ := responsesWSTestServer(t, false)
	defer first.Close()
	second, secondFrames, secondConnections, _ := responsesWSTestServer(t, false)
	defer second.Close()
	implementation, err := provider.NewOpenAIResponsesProvider(provider.OpenAIResponsesConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		url, provider, key string
		frames             <-chan responsesWSObservation
		connection         int32
	}{
		{first.URL, "one", "key-a", firstFrames, 1},
		{second.URL, "one", "key-a", secondFrames, 1},
		{second.URL, "one", "key-b", secondFrames, 2},
		{second.URL, "two", "key-b", secondFrames, 3},
		{first.URL, "one", "key-a", firstFrames, 1},
	} {
		model := responsesWSModel(t, test.provider, test.url, nil)
		request := responsesWSRequest(t, model, []llm.ConversationMessage{mustUser(t, "hello")}, provider.StreamOptions{Transport: provider.TransportWebsocketCached, SessionID: "same-session", APIKey: test.key})
		_, terminal := collectStream(t, implementation.Stream(context.Background(), request))
		observed := <-test.frames
		if terminal.FinishReason() != llm.FinishStop || observed.connection != test.connection || observed.header.Get("Authorization") != "Bearer "+test.key {
			t.Fatalf("wrong connection or auth for %s at %s", test.provider, test.url)
		}
		if _, exists := observed.frame["previous_response_id"]; exists {
			t.Fatal("unrelated history was continued")
		}
	}
	if firstConnections.Load() != 1 || secondConnections.Load() != 3 {
		t.Fatalf("connections = %d/%d", firstConnections.Load(), secondConnections.Load())
	}
}

func TestOpenAIResponsesWebSocketFallbackIsScopedToChannel(t *testing.T) {
	provider.ResetOpenAIResponsesWebSocketDebugStats("")
	defer provider.CloseOpenAIResponsesWebSocketSessions("")
	unsupported, _, rejected, posts := responsesWSTestServer(t, true)
	defer unsupported.Close()
	supported, _, accepted, unexpectedPosts := responsesWSTestServer(t, false)
	defer supported.Close()
	implementation, err := provider.NewOpenAIResponsesProvider(provider.OpenAIResponsesConfig{APIKey: "api-key"})
	if err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{unsupported.URL, supported.URL, unsupported.URL} {
		model := responsesWSModel(t, "gateway", url, nil)
		request := responsesWSRequest(t, model, []llm.ConversationMessage{mustUser(t, "hello")}, provider.StreamOptions{Transport: provider.TransportWebsocket, SessionID: "fallback-scope"})
		_, terminal := collectStream(t, implementation.Stream(context.Background(), request))
		if terminal.FinishReason() != llm.FinishStop {
			t.Fatalf("terminal = %v", terminal)
		}
	}
	if rejected.Load() != 1 || posts.Load() != 2 || accepted.Load() != 1 || unexpectedPosts.Load() != 0 {
		t.Fatalf("fallback crossed channels: rejected=%d posts=%d accepted=%d unexpectedPosts=%d", rejected.Load(), posts.Load(), accepted.Load(), unexpectedPosts.Load())
	}
}

func TestOpenAIResponsesWebSocketFinalHeaderAuthAndUnsupportedBackground(t *testing.T) {
	for _, test := range []string{"hook supplies auth", "hook removes auth", "background request"} {
		t.Run(test, func(t *testing.T) {
			defer provider.CloseOpenAIResponsesWebSocketSessions("")
			server, frames, connections, posts := responsesWSTestServer(t, false)
			defer server.Close()
			implementation, err := provider.NewOpenAIResponsesProvider(provider.OpenAIResponsesConfig{})
			if err != nil {
				t.Fatal(err)
			}
			hookCalls := 0
			options := provider.StreamOptions{Transport: provider.TransportWebsocket, Headers: map[string]string{"OpenAI-Beta": "gateway-option=1"}}
			options.OnHeaders = func(_ provider.Model, headers map[string]*string) error {
				hookCalls++
				if headerHookValue(headers, "OpenAI-Beta") != "gateway-option=1" {
					t.Error("hook did not see final transport headers")
				}
				if test != "hook removes auth" {
					value := "Bearer header-only-key"
					headers["Authorization"] = &value
				}
				return nil
			}
			if test == "background request" {
				options.OnPayload = func(_ provider.Model, payload []byte) ([]byte, error) {
					var body map[string]any
					if err := json.Unmarshal(payload, &body); err != nil {
						return nil, err
					}
					body["background"] = true
					return json.Marshal(body)
				}
			}
			request := responsesWSRequest(t, responsesWSModel(t, "gateway", server.URL, nil), []llm.ConversationMessage{mustUser(t, "hi")}, options)
			events, terminal := collectStream(t, implementation.Stream(context.Background(), request))
			if test == "hook supplies auth" {
				frame := <-frames
				if terminal.FinishReason() != llm.FinishStop || frame.header.Get("Authorization") != "Bearer header-only-key" || connections.Load() != 1 {
					t.Fatal("header-owned authentication failed")
				}
			} else if terminal.FinishReason() != llm.FinishError || connections.Load() != 0 || !reflect.DeepEqual(eventKinds(events), []string{"error"}) {
				t.Fatalf("unsupported request reached the transport: %v", terminal)
			}
			if posts.Load() != 0 || hookCalls != 1 {
				t.Fatalf("unexpected SSE fallback or repeated hooks: posts=%d hooks=%d", posts.Load(), hookCalls)
			}
		})
	}
}
