package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

// This exercises the shipped executable, its production provider, real bash
// child process, live CLI connection, original output truncation, read tool,
// durable checkpoint and a fresh-process resume. Only the model is scripted.
func TestSessionHandoffBinaryEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	name := "pi-go"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/pi-go")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	cwd, agentDir := t.TempDir(), t.TempDir()
	file := filepath.Join(cwd, "session.jsonl")
	const original = "ORIGINAL_TASK_KEEP_THIS_CORRECTION"
	prompt := original + "\n" + strings.Repeat("original history filler line\n", 3000) + "END_ORIGINAL_TASK"
	var mu sync.Mutex
	calls := 0
	systemPrefix := ""
	fullOutput := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		calls++
		if len(payload.Messages) == 0 {
			t.Error("missing provider messages")
			w.WriteHeader(400)
			return
		}
		if calls == 1 {
			systemPrefix = string(payload.Messages[0].Content)
		} else if string(payload.Messages[0].Content) != systemPrefix {
			t.Error("system/tool capability prefix changed across handoff")
		}
		text := ""
		toolName := ""
		var arguments any
		switch calls {
		case 1:
			toolName = "bash"
			arguments = map[string]any{"command": `"$PI_EXECUTABLE" session context new --request-id binary-handoff --handoff 'Continue by reading original dialogue through session history.'`}
		case 2:
			encoded, _ := json.Marshal(payload.Messages)
			if strings.Contains(string(encoded), original) || strings.Contains(string(encoded), "original history filler") || !strings.Contains(string(encoded), "Continue the same task in context window") {
				t.Error("new window did not contain only the handoff")
			}
			toolName = "bash"
			arguments = map[string]any{"command": `"$PI_EXECUTABLE" session history read --view dialogue --window initial`}
		case 3:
			encoded := string(payload.Messages[len(payload.Messages)-1].Content)
			var content string
			_ = json.Unmarshal([]byte(encoded), &content)
			match := regexp.MustCompile(`Full output: ([^\]\n]+)`).FindStringSubmatch(content)
			if len(match) != 2 {
				t.Errorf("bash did not use its unified full-output file: %.200s", content)
				text = "FAIL"
				break
			}
			fullOutput = match[1]
			toolName = "read"
			arguments = map[string]any{"path": fullOutput, "offset": 1, "limit": 8}
		case 4:
			if !strings.Contains(string(payload.Messages[len(payload.Messages)-1].Content), original) {
				t.Error("read did not recover the original user correction")
			}
			text = "HANDOFF_E2E_OK"
		case 5:
			encoded, _ := json.Marshal(payload.Messages)
			if !strings.Contains(string(encoded), "Continue the same task in context window") || !strings.Contains(string(encoded), "resume after restart") {
				t.Error("fresh process did not restore the handoff context")
			}
			text = "RESTART_OK"
		default:
			t.Errorf("unexpected model call %d (summary or duplicate run)", calls)
			text = "UNEXPECTED"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		delta := map[string]any{"content": text}
		finish := "stop"
		if toolName != "" {
			raw, _ := json.Marshal(arguments)
			delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("call-%d", calls), "type": "function", "function": map[string]any{"name": toolName, "arguments": string(raw)}}}}
			finish = "tool_calls"
		}
		chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}})
		fmt.Fprintf(w, "data: %s\n\n", chunk)
		tokens := 100
		if calls == 1 {
			tokens = 120000
		}
		chunk, _ = json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}, "usage": map[string]any{"prompt_tokens": tokens, "completion_tokens": 10, "total_tokens": tokens + 10}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	defer server.Close()
	models, _ := json.Marshal(map[string]any{"providers": map[string]any{"fixture": map[string]any{"baseUrl": server.URL + "/v1", "apiKey": "fixture-key", "api": provider.OpenAICompletionsAPI, "models": []any{map[string]any{"id": "handoff", "name": "handoff", "reasoning": false, "input": []string{"text"}, "contextWindow": 128000, "maxTokens": 4000}}}}})
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), models, 0600); err != nil {
		t.Fatal(err)
	}
	settings := []byte(`{"compaction":{"enabled":true,"reserveTokens":16384,"keepRecentTokens":100},"retry":{"enabled":false}}`)
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), settings, 0600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "PI_GO_AGENT_DIR="+agentDir)
	runBinary := func(args ...string) string {
		t.Helper()
		command := exec.CommandContext(ctx, binary, args...)
		command.Dir = cwd
		command.Env = env
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		if err := command.Run(); err != nil {
			t.Fatalf("binary %s: %v\n%s", args[0], err, stderr.String())
		}
		return stdout.String()
	}
	if output := runBinary("run", "--model", "fixture/handoff", "--session", file, "-p", prompt); !strings.Contains(output, "HANDOFF_E2E_OK") {
		t.Fatalf("result %s", output)
	}
	if output := runBinary("run", "--model", "fixture/handoff", "--session", file, "-p", "resume after restart"); !strings.Contains(output, "RESTART_OK") {
		t.Fatalf("resume %s", output)
	}
	index, err := session.OpenReadIndex(file)
	if err != nil {
		t.Fatal(err)
	}
	view, err := index.ReadHistoryView(session.HistoryViewQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Windows) != 2 || view.Records[0].Blocks[0].Text != prompt {
		t.Fatal("durable original history or window boundary lost")
	}
	mu.Lock()
	count, artifact := calls, fullOutput
	mu.Unlock()
	if count != 5 {
		t.Fatalf("model calls = %d", count)
	}
	data, err := os.ReadFile(artifact)
	if err != nil || !bytes.Contains(data, []byte(prompt)) {
		t.Fatal("unified bash artifact lost full history", err)
	}
	output := runBinary("session", "--file", file, "--json", "history", "search", original, "--view", "dialogue")
	var found session.HistoryView
	if err := json.Unmarshal([]byte(output), &found); err != nil || len(found.Records) != 1 {
		t.Fatalf("offline query: %v %.300s", err, output)
	}
}
