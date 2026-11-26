package app_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/testenv"
)

func TestRunProductionOpenCodeGoResumesSession(t *testing.T) {
	workingDir, agentDir := t.TempDir(), testenv.AgentDir(t)
	var sessions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer go-key" {
			t.Errorf("route or auth mismatch: %s %s", r.Method, r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "pi-go/") {
			t.Error("missing pi-go User-Agent")
		}
		sessions = append(sessions, r.Header.Get("x-opencode-session"))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"go answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	writeProviderModelsJSON(t, agentDir, map[string]any{"opencode-go": map[string]any{"baseUrl": server.URL + "/v1"}})
	config := productionTestConfig(workingDir, agentDir, []string{"OPENCODE_API_KEY=go-key", "PI_CACHE_RETENTION=none"})
	config.OpenAIHTTPClient = server.Client()
	sessionFile := filepath.Join(workingDir, "opencode.jsonl")
	for _, prompt := range []string{"first", "second"} {
		var stdout, stderr bytes.Buffer
		code := app.RunProduction(context.Background(), config, []string{
			"--model", "opencode-go/deepseek-v4.1-flash", "--session", sessionFile, "-p", prompt,
		}, &stdout, &stderr)
		if code != app.ExitSuccess || stdout.String() != "go answer\n" || stderr.Len() != 0 {
			t.Fatalf("RunProduction=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	}
	if len(sessions) != 2 || sessions[0] == "" || sessions[0] != sessions[1] {
		t.Fatalf("session headers not stable across resume: %#v", sessions)
	}
}
