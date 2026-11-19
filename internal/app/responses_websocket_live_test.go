package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

// Opt-in acceptance through the production config/auth, AgentSession, tools,
// durable history, and real Responses WebSocket transport. Credentials and
// gateway addresses come only from the selected local agent configuration.
func TestLiveResponsesWebSocketAgentSession(t *testing.T) {
	model := strings.TrimSpace(os.Getenv("PI_GO_LIVE_RESPONSES_MODEL"))
	agentDir := strings.TrimSpace(os.Getenv("PI_GO_LIVE_RESPONSES_AGENT_DIR"))
	if model == "" || agentDir == "" {
		t.Skip("PI_GO_LIVE_RESPONSES_MODEL and PI_GO_LIVE_RESPONSES_AGENT_DIR are required")
	}
	root := os.Getenv("PI_GO_LIVE_RESPONSES_ARTIFACT_DIR")
	if root == "" {
		root = t.TempDir()
	}
	workingDir, err := os.MkdirTemp(root, "scenario-")
	if err != nil {
		t.Fatal(err)
	}
	sessionID := fmt.Sprintf("responses-live-%d", time.Now().UnixNano())
	sessionPath := filepath.Join(workingDir, "session.jsonl")
	provider.ResetOpenAIResponsesWebSocketDebugStats(sessionID)
	defer provider.CloseOpenAIResponsesWebSocketSessions(sessionID)
	transport := &responsesLiveTransport{next: http.DefaultTransport.(*http.Transport).Clone()}
	defer transport.next.CloseIdleConnections()
	config := ProductionConfig{
		WorkingDir: workingDir, AgentDir: agentDir, SessionID: sessionID,
		OpenAIHTTPClient: &http.Client{Transport: transport},
	}
	const (
		fileName = "responses-live.txt"
		content  = "PI_GO_RESPONSES_WEBSOCKET_TOOL_CONTENT"
		firstOK  = "PI_GO_RESPONSES_WRITE_OK"
		secondOK = "PI_GO_RESPONSES_READ_OK"
		thirdOK  = "PI_GO_RESPONSES_RESUME_OK"
	)
	turns := []struct{ prompt, answer string }{
		{"This is an acceptance test. Use the write tool exactly once to write " + fileName + " with exactly this content: " + content + ". Do not use any other tool. After it succeeds reply only " + firstOK + ".", firstOK},
		{"Use the read tool exactly once to read " + fileName + ". If its content is " + content + ", reply only " + secondOK + ". Otherwise reply FAILURE. Do not use any other tool.", secondOK},
		{"Do not call any tools. Verify the preceding conversation contains a successful write and read of " + content + ". If it does, reply only " + thirdOK + "; otherwise reply FAILURE.", thirdOK},
	}
	for index, turn := range turns {
		if index == 2 {
			// Discard connection-local continuation and reopen the physical session
			// through a fresh production Runtime, as a restarted client would.
			provider.CloseOpenAIResponsesWebSocketSessions(sessionID)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		var stdout, stderr bytes.Buffer
		code := RunProduction(ctx, config, []string{"--model", model, "--session", sessionPath, "-p", turn.prompt}, &stdout, &stderr)
		cancel()
		if code != ExitSuccess || !strings.Contains(stdout.String(), turn.answer) || stderr.Len() != 0 {
			t.Fatalf("turn %d failed: exit=%d stdout=%q stderr=%q (artifacts %s)", index+1, code, stdout.String(), stderr.String(), workingDir)
		}
		t.Logf("turn %d completed: %s", index+1, turn.answer)
	}
	written, err := os.ReadFile(filepath.Join(workingDir, fileName))
	if err != nil || string(written) != content {
		t.Fatalf("real tool write = %q, %v", written, err)
	}
	reopened, err := session.Open(sessionPath, session.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	messages := reopened.BuildContext().Messages()
	actualSessionID := reopened.Header().ID()
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if actualSessionID != sessionID {
		t.Fatalf("session ID = %s, expected %s", actualSessionID, sessionID)
	}
	toolResults := map[string]int{}
	for _, message := range messages {
		if result, ok := message.(llm.ToolResultMessage); ok {
			if result.IsError() {
				t.Fatalf("persisted failed tool: %s", result.ToolName())
			}
			toolResults[result.ToolName()]++
		}
	}
	if toolResults["write"] != 1 || toolResults["read"] != 1 || len(toolResults) != 2 {
		t.Fatalf("unexpected durable tool results: %v", toolResults)
	}
	stats, ok := provider.GetOpenAIResponsesWebSocketDebugStats(sessionID)
	transport.mu.Lock()
	upgrades, posts := transport.upgrades, transport.posts
	secrets := append([]string(nil), transport.secrets...)
	transport.mu.Unlock()
	if !ok || upgrades < 2 || posts != 0 || stats.Requests < 5 || stats.DeltaRequests < 2 || stats.ConnectionsReused < 2 || stats.WebSocketFailures != 0 {
		t.Fatalf("real WebSocket/continuation acceptance failed: upgrades=%d posts=%d stats=%+v", upgrades, posts, stats)
	}
	data, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if secret != "" && bytes.Contains(data, []byte(secret)) {
			t.Fatal("request credential appeared in the durable session")
		}
	}
	report, err := json.MarshalIndent(map[string]any{
		"model": model, "session": sessionPath, "http101": upgrades, "httpPosts": posts,
		"transport": stats, "toolResults": toolResults, "messageCount": len(messages),
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(workingDir, "result.json")
	if err := os.WriteFile(reportPath, append(report, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("verified HTTP 101=%d, SSE POST=%d, requests=%d, deltas=%d, reused=%d; report=%s", upgrades, posts, stats.Requests, stats.DeltaRequests, stats.ConnectionsReused, reportPath)
}

type responsesLiveTransport struct {
	next     *http.Transport
	mu       sync.Mutex
	upgrades int
	posts    int
	secrets  []string
}

func (t *responsesLiveTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(request)
	if strings.HasSuffix(request.URL.Path, "/responses") {
		t.mu.Lock()
		if request.Method == http.MethodPost {
			t.posts++
		}
		if response != nil && response.StatusCode == http.StatusSwitchingProtocols {
			t.upgrades++
		}
		if authorization := request.Header.Get("Authorization"); strings.HasPrefix(authorization, "Bearer ") {
			t.secrets = append(t.secrets, strings.TrimPrefix(authorization, "Bearer "))
		}
		t.mu.Unlock()
	}
	return response, err
}
