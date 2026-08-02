package app_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/session"
)

var productionTestTime = time.Date(2026, time.August, 2, 9, 10, 11, 120_000_000, time.UTC)

type capturedProductionRequest struct {
	mu            sync.Mutex
	count         int
	method        string
	path          string
	authorization string
	payload       map[string]any
}

type productionRequestSnapshot struct {
	count         int
	method        string
	path          string
	authorization string
	payload       map[string]any
}

func (c *capturedProductionRequest) record(request *http.Request) error {
	var payload map[string]any
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count++
	c.method = request.Method
	c.path = request.URL.Path
	c.authorization = request.Header.Get("Authorization")
	c.payload = payload
	return nil
}

func (c *capturedProductionRequest) snapshot() productionRequestSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return productionRequestSnapshot{
		count:         c.count,
		method:        c.method,
		path:          c.path,
		authorization: c.authorization,
		payload:       c.payload,
	}
}

func TestRunProductionCompletesConfiguredOpenAIWorkflowWithDefaultSession(t *testing.T) {
	workingDir := t.TempDir()
	agentDir := t.TempDir()
	capture := &capturedProductionRequest{}
	server := newProductionTextServer(t, capture, "assembled answer")
	defer server.Close()
	writeModelsJSON(t, agentDir, server.URL+"/v1", stringPointer("prefix-${MODEL_KEY}-suffix"), nil)

	config := productionTestConfig(workingDir, agentDir, []string{"MODEL_KEY=configured"})
	config.SessionID = ""
	var sessionTicks atomic.Int64
	config.SessionNow = func() time.Time {
		return productionTestTime.Add(time.Duration(sessionTicks.Add(1)-1) * time.Millisecond)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := app.RunProduction(
		context.Background(),
		config,
		[]string{"--model", "openai/gpt-production", "-p", "hello from production"},
		&stdout,
		&stderr,
	)
	if exitCode != app.ExitSuccess || stdout.String() != "assembled answer\n" || stderr.Len() != 0 {
		t.Fatalf("RunProduction() = code %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
	}

	request := capture.snapshot()
	if request.count != 1 || request.method != http.MethodPost || request.path != "/v1/responses" {
		t.Fatalf("request = count %d, %s %s", request.count, request.method, request.path)
	}
	if request.authorization != "Bearer prefix-configured-suffix" {
		t.Fatalf("Authorization = %q", request.authorization)
	}
	if request.payload["model"] != "gpt-production" || request.payload["stream"] != true || request.payload["store"] != false {
		t.Fatalf("payload routing = %#v", request.payload)
	}
	input, ok := request.payload["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("payload input = %#v", request.payload["input"])
	}
	user, ok := input[0].(map[string]any)
	if !ok || user["role"] != "user" {
		t.Fatalf("payload user = %#v", input[0])
	}

	matches, err := filepath.Glob(filepath.Join(agentDir, "sessions", "*", "*.jsonl"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("default session files = %v, %v", matches, err)
	}
	sessionPath := matches[0]
	expectedDirectory := filepath.Dir(expectedProductionSessionPath(agentDir, workingDir, "placeholder"))
	if filepath.Dir(sessionPath) != expectedDirectory {
		t.Fatalf("default session directory = %q, want %q", filepath.Dir(sessionPath), expectedDirectory)
	}
	if name := filepath.Base(sessionPath); !strings.HasPrefix(name, "2026-08-02T09-10-11-120Z_") || !strings.HasSuffix(name, ".jsonl") {
		t.Fatalf("default session filename = %q", name)
	}
	transcript, err := session.Open(sessionPath, session.OpenOptions{})
	if err != nil {
		t.Fatalf("session.Open(%s) = %v", sessionPath, err)
	}
	defer transcript.Close()
	if transcript.Header().WorkingDir() != workingDir || transcript.Header().Timestamp() != productionTestTime {
		t.Fatalf("session header = id %q, cwd %q", transcript.Header().ID(), transcript.Header().WorkingDir())
	}
	if filepath.Base(sessionPath) != "2026-08-02T09-10-11-120Z_"+transcript.Header().ID()+".jsonl" {
		t.Fatalf("session filename and header ID differ: %q / %q", filepath.Base(sessionPath), transcript.Header().ID())
	}
	messages := transcript.Context().Messages()
	if len(messages) != 2 || messages[0].Role() != llm.RoleUser || messages[1].Role() != llm.RoleAssistant {
		t.Fatalf("durable messages = %#v", messages)
	}
	provenance, ok := transcript.Context().AssistantProvenance()
	if !ok || provenance.Provider != "openai" || provenance.API != "openai-responses" || provenance.Model != "gpt-production" {
		t.Fatalf("assistant provenance = %#v, %t", provenance, ok)
	}
}

func TestRunProductionOpenAIToolWorkflowReplaysDurableResult(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the production Bash executor is covered by platform-specific process tests")
	}
	workingDir := t.TempDir()
	agentDir := t.TempDir()
	sessionPath := filepath.Join(workingDir, "tool-workflow.jsonl")
	var mu sync.Mutex
	var payloads []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		payloads = append(payloads, payload)
		turn := len(payloads)
		mu.Unlock()
		writer.Header().Set("Content-Type", "text/event-stream")
		if turn == 1 {
			call := map[string]any{"type": "function_call", "id": "fc_prod", "call_id": "call_prod", "name": "bash", "arguments": `{"command":"printf tool-ok"}`}
			writeProductionSSE(t, writer,
				map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc_prod", "call_id": "call_prod", "name": "bash", "arguments": ""}},
				map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": "fc_prod", "delta": `{"command":"printf tool-ok"}`},
				map[string]any{"type": "response.function_call_arguments.done", "output_index": 0, "item_id": "fc_prod", "arguments": `{"command":"printf tool-ok"}`},
				map[string]any{"type": "response.output_item.done", "output_index": 0, "item": call},
				map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{call}}},
			)
			return
		}
		item := map[string]any{"type": "message", "id": "msg_final", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "tool completed"}}}
		writeProductionSSE(t, writer,
			map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item},
			map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{item}}},
		)
	}))
	defer server.Close()
	writeModelsJSON(t, agentDir, server.URL+"/v1", stringPointer("fixture-key"), nil)
	config := productionTestConfig(workingDir, agentDir, nil)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := app.RunProduction(context.Background(), config, []string{"--model", "openai/gpt-tool", "--session", sessionPath, "-p", "use bash"}, &stdout, &stderr)
	if exitCode != app.ExitSuccess || stdout.String() != "tool completed\n" || stderr.Len() != 0 {
		t.Fatalf("RunProduction() = code %d stdout %q stderr %q", exitCode, stdout.String(), stderr.String())
	}
	mu.Lock()
	received := append([]map[string]any(nil), payloads...)
	mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("request count = %d", len(received))
	}
	tools, ok := received[0]["tools"].([]any)
	if !ok || len(tools) != 7 {
		t.Fatalf("first request tools = %#v", received[0]["tools"])
	}
	input, ok := received[1]["input"].([]any)
	if !ok || len(input) != 3 {
		t.Fatalf("second request input = %#v", received[1]["input"])
	}
	function, ok := input[1].(map[string]any)
	if !ok || function["type"] != "function_call" || function["call_id"] != "call_prod" || function["id"] != "fc_prod" {
		t.Fatalf("second request function = %#v", input[1])
	}
	output, ok := input[2].(map[string]any)
	if !ok || output["type"] != "function_call_output" || output["call_id"] != "call_prod" || output["output"] != "tool-ok" {
		t.Fatalf("second request result = %#v", input[2])
	}
	transcript, err := session.Open(sessionPath, session.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer transcript.Close()
	messages := transcript.Context().Messages()
	if len(messages) != 4 || messages[0].Role() != llm.RoleUser || messages[1].Role() != llm.RoleAssistant || messages[2].Role() != llm.RoleToolResult || messages[3].Role() != llm.RoleAssistant {
		t.Fatalf("durable roles = %#v", messages)
	}
}

func TestRunProductionPersistsAndReplaysResponsesReasoning(t *testing.T) {
	workingDir, agentDir := t.TempDir(), t.TempDir()
	sessionPath := filepath.Join(workingDir, "reasoning.jsonl")
	var mu sync.Mutex
	var payloads []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode: %v", err)
			return
		}
		mu.Lock()
		payloads = append(payloads, payload)
		turn := len(payloads)
		mu.Unlock()
		writer.Header().Set("Content-Type", "text/event-stream")
		if turn == 1 {
			reasoningDone := map[string]any{"type": "reasoning", "id": "rs_persist"}
			reasoningTerminal := map[string]any{"type": "reasoning", "id": "rs_persist", "encrypted_content": "cipher"}
			text := map[string]any{"type": "message", "id": "msg_persist", "role": "assistant", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": "first"}}}
			writeProductionSSE(t, writer, map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "reasoning", "id": "rs_persist"}}, map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": 0, "item_id": "rs_persist", "delta": "plan"}, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": reasoningDone}, map[string]any{"type": "response.output_item.done", "output_index": 1, "item": text}, map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{reasoningTerminal, text}}})
			return
		}
		text := map[string]any{"type": "message", "id": "msg_next", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "second"}}}
		writeProductionSSE(t, writer, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": text}, map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{text}}})
	}))
	defer server.Close()
	writeModelsJSON(t, agentDir, server.URL+"/v1", stringPointer("fixture-key"), nil)
	config := productionTestConfig(workingDir, agentDir, nil)
	for _, prompt := range []string{"first prompt", "continue"} {
		var stdout, stderr bytes.Buffer
		if code := app.RunProduction(context.Background(), config, []string{"--model", "openai/gpt-reason", "--session", sessionPath, "-p", prompt}, &stdout, &stderr); code != app.ExitSuccess || stderr.Len() != 0 {
			t.Fatalf("run %q: code=%d stdout=%q stderr=%q", prompt, code, stdout.String(), stderr.String())
		}
	}
	mu.Lock()
	received := append([]map[string]any(nil), payloads...)
	mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("requests=%d", len(received))
	}
	input := received[1]["input"].([]any)
	foundReasoning := false
	foundText := false
	for _, item := range input {
		wire := item.(map[string]any)
		if wire["type"] == "reasoning" {
			foundReasoning = wire["id"] == "rs_persist" && wire["encrypted_content"] == "cipher"
		}
		if wire["type"] == "message" && wire["id"] == "msg_persist" {
			foundText = wire["phase"] == "final_answer"
		}
	}
	if !foundReasoning || !foundText {
		t.Fatalf("resumed input=%#v", input)
	}
	transcript, err := session.Open(sessionPath, session.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer transcript.Close()
	messages := transcript.Context().Messages()
	if len(messages) != 4 {
		t.Fatalf("durable messages=%#v", messages)
	}
	if _, ok := messages[1].(llm.AssistantRichMessage); !ok {
		t.Fatalf("first assistant=%T", messages[1])
	}
	if replay, ok := messages[1].(llm.AssistantRichMessage).OpenAIResponsesMetadata(); !ok || replay.RawStopReason != "completed" {
		t.Fatalf("response replay=%#v", replay)
	}
}

func TestRunProductionReplaysDurableImageAfterRestart(t *testing.T) {
	workingDir, agentDir := t.TempDir(), t.TempDir()
	sessionPath := filepath.Join(workingDir, "image-restart.jsonl")
	entryIDs := []string{"seed-image", "new-user", "assistant"}
	transcript, err := session.Create(sessionPath, session.CreateOptions{
		ID:         "image-restart",
		WorkingDir: workingDir,
		Now:        func() time.Time { return productionTestTime },
		NewEntryID: func() (string, error) {
			id := entryIDs[0]
			entryIDs = entryIDs[1:]
			return id, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	const pngBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	png, err := base64.StdEncoding.DecodeString(pngBase64)
	if err != nil {
		t.Fatal(err)
	}
	image, err := llm.NewImageDataBlock("image/png", png)
	if err != nil {
		t.Fatal(err)
	}
	caption, err := llm.NewTextBlock("seed image")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := llm.NewUserContentMessage([]llm.UserContentBlock{caption, image}, productionTestTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.Append(context.Background(), seed, session.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}

	capture := &capturedProductionRequest{}
	server := newProductionTextServer(t, capture, "image resumed")
	defer server.Close()
	writeModelsJSON(t, agentDir, server.URL+"/v1", stringPointer("fixture-key"), nil)
	config := productionTestConfig(workingDir, agentDir, nil)
	config.NewSessionEntryID = func() (string, error) {
		id := entryIDs[0]
		entryIDs = entryIDs[1:]
		return id, nil
	}
	var stdout, stderr bytes.Buffer
	if code := app.RunProduction(context.Background(), config, []string{"--model", "openai/gpt-image", "--session", sessionPath, "-p", "continue"}, &stdout, &stderr); code != app.ExitSuccess || stderr.Len() != 0 {
		t.Fatalf("RunProduction() = code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	input := capture.snapshot().payload["input"].([]any)
	if len(input) < 2 {
		t.Fatalf("resumed input = %#v", input)
	}
	seedWire := input[0].(map[string]any)
	content := seedWire["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("seed content = %#v", content)
	}
	imageWire := content[1].(map[string]any)
	if imageWire["type"] != "input_image" || imageWire["image_url"] != "data:image/png;base64,"+pngBase64 {
		t.Fatalf("durable image replay = %#v", imageWire)
	}
}

func TestRunProductionCredentialPrecedenceAndModelAdmission(t *testing.T) {
	testCases := []struct {
		name          string
		args          []string
		authJSON      string
		configuredKey *string
		environment   []string
		wantKey       string
		wantModel     string
		storedFile    bool
	}{
		{
			name:          "CLI overrides stored configured and ambient",
			args:          []string{"--provider", "openai", "--model", "custom-cli", "--api-key", "cli-key", "-p", "hello"},
			authJSON:      `{this lower source is deliberately malformed`,
			configuredKey: stringPointer("configured-key"),
			environment:   []string{"OPENAI_API_KEY=ambient-key"},
			wantKey:       "cli-key",
			wantModel:     "custom-cli",
		},
		{
			name:          "stored scoped template overrides configured and ambient",
			args:          []string{"--model", "openai/custom-stored", "-p", "hello"},
			authJSON:      `{"openai":{"type":"api_key","key":"stored-${STORED_KEY}","env":{"STORED_KEY":"scoped"}}}`,
			configuredKey: stringPointer("configured-key"),
			environment:   []string{"STORED_KEY=ambient", "OPENAI_API_KEY=ambient-key"},
			wantKey:       "stored-scoped",
			wantModel:     "custom-stored",
			storedFile:    true,
		},
		{
			name:          "models JSON template overrides ambient",
			args:          []string{"--model", "gpt-5.5", "-p", "hello"},
			configuredKey: stringPointer("configured-$$-$!-${MODEL_KEY}"),
			environment:   []string{"MODEL_KEY=template", "OPENAI_API_KEY=ambient-key"},
			wantKey:       "configured-$-!-template",
			wantModel:     "gpt-5.5",
		},
		{
			name:        "ambient key with fixed default model",
			args:        []string{"-p", "hello"},
			environment: []string{"OPENAI_API_KEY=ambient-key"},
			wantKey:     "ambient-key",
			wantModel:   "gpt-5.5",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && testCase.storedFile {
				t.Skip("Windows v0.1 persistent auth is fail-closed; covered by Windows-specific tests")
			}
			workingDir := t.TempDir()
			agentDir := t.TempDir()
			capture := &capturedProductionRequest{}
			server := newProductionTextServer(t, capture, "ok")
			defer server.Close()
			writeModelsJSON(t, agentDir, server.URL+"/v1", testCase.configuredKey, nil)
			if testCase.authJSON != "" {
				writeAuthJSON(t, agentDir, testCase.authJSON)
			}
			sessionPath := filepath.Join(workingDir, "explicit", "session.jsonl")
			args := append(append([]string(nil), testCase.args...), "--session", sessionPath)
			config := productionTestConfig(workingDir, agentDir, testCase.environment)
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := app.RunProduction(context.Background(), config, args, &stdout, &stderr)
			if exitCode != app.ExitSuccess || stdout.String() != "ok\n" || stderr.Len() != 0 {
				t.Fatalf("RunProduction() = code %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
			}
			request := capture.snapshot()
			if request.count != 1 || request.authorization != "Bearer "+testCase.wantKey {
				t.Fatalf("request = count %d, authorization %q", request.count, request.authorization)
			}
			if request.payload["model"] != testCase.wantModel {
				t.Fatalf("model = %#v, want %q", request.payload["model"], testCase.wantModel)
			}
			if _, err := os.Stat(sessionPath); err != nil {
				t.Fatalf("explicit session not created: %v", err)
			}
			if _, err := os.Stat(filepath.Join(agentDir, "sessions")); !os.IsNotExist(err) {
				t.Fatalf("explicit session unexpectedly provisioned default tree: %v", err)
			}
		})
	}
}

type countingProductionDoer struct {
	calls atomic.Uint32
}

func (d *countingProductionDoer) Do(*http.Request) (*http.Response, error) {
	d.calls.Add(1)
	return nil, fmt.Errorf("network must not be reached")
}

func TestRunProductionPreflightIsSecretSafeAndSideEffectFree(t *testing.T) {
	testCases := []struct {
		name        string
		args        []string
		environment []string
		prepare     func(*testing.T, string)
		want        string
		secrets     []string
		storedFile  bool
	}{
		{
			name:        "stored OAuth owns provider",
			args:        []string{"--model", "openai/gpt-test", "-p", "hello"},
			environment: []string{"OPENAI_API_KEY=ambient-secret"},
			prepare: func(t *testing.T, agentDir string) {
				writeModelsJSON(t, agentDir, "https://fixture.invalid/v1", nil, nil)
				writeAuthJSON(t, agentDir, `{"openai":{"type":"oauth","access":"stored-secret"}}`)
			},
			want:       "credential type is not migrated",
			secrets:    []string{"ambient-secret", "stored-secret"},
			storedFile: true,
		},
		{
			name:        "stored unresolved template does not fall through",
			args:        []string{"--model", "openai/gpt-test", "-p", "hello"},
			environment: []string{"OPENAI_API_KEY=ambient-secret"},
			prepare: func(t *testing.T, agentDir string) {
				writeModelsJSON(t, agentDir, "https://fixture.invalid/v1", nil, nil)
				writeAuthJSON(t, agentDir, `{"openai":{"type":"api_key","key":"prefix-${MISSING_KEY}-stored-secret"}}`)
			},
			want:       "references missing environment variable",
			secrets:    []string{"ambient-secret", "stored-secret"},
			storedFile: true,
		},
		{
			name:        "command-backed configured key is explicit",
			args:        []string{"--model", "openai/gpt-test", "-p", "hello"},
			environment: []string{"OPENAI_API_KEY=ambient-secret"},
			prepare: func(t *testing.T, agentDir string) {
				writeModelsJSON(t, agentDir, "https://fixture.invalid/v1", stringPointer("!printf command-secret"), nil)
			},
			want:    "command-backed configured OpenAI API key is not migrated",
			secrets: []string{"ambient-secret", "command-secret"},
		},
		{
			name:        "malformed models JSON",
			args:        []string{"--model", "openai/gpt-test", "-p", "hello"},
			environment: []string{"OPENAI_API_KEY=ambient-secret"},
			prepare: func(t *testing.T, agentDir string) {
				content := `{"providers":{"openai":{"apiKey":"models-secret",}} trailing-secret`
				if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want:    "parse models.json",
			secrets: []string{"ambient-secret", "models-secret", "trailing-secret"},
		},
		{
			name:        "unsupported models request configuration",
			args:        []string{"--model", "openai/gpt-test", "-p", "hello"},
			environment: []string{"OPENAI_API_KEY=ambient-secret"},
			prepare: func(t *testing.T, agentDir string) {
				writeModelsJSON(t, agentDir, "https://fixture.invalid/v1", nil, map[string]any{
					"headers": map[string]any{"X-Secret": "models-secret"},
				})
			},
			want:    "field outside the migrated projection",
			secrets: []string{"ambient-secret", "models-secret"},
		},
		{
			name:        "unknown selected models field is not ignored",
			args:        []string{"--model", "openai/gpt-test", "-p", "hello"},
			environment: []string{"OPENAI_API_KEY=ambient-secret"},
			prepare: func(t *testing.T, agentDir string) {
				writeModelsJSON(t, agentDir, "https://fixture.invalid/v1", nil, map[string]any{
					"futureRequestOption": "future-secret",
				})
			},
			want:    "field outside the migrated projection",
			secrets: []string{"ambient-secret", "future-secret"},
		},
		{
			name: "missing all credential sources",
			args: []string{"--model", "openai/gpt-test", "-p", "hello"},
			prepare: func(t *testing.T, agentDir string) {
				writeModelsJSON(t, agentDir, "https://fixture.invalid/v1", nil, nil)
			},
			want: "OpenAI API key is not configured",
		},
		{
			name:        "unknown provider route",
			args:        []string{"--model", "unknown/model", "-p", "hello"},
			environment: []string{"OPENAI_API_KEY=ambient-secret"},
			prepare:     func(*testing.T, string) {},
			want:        "selects an unknown provider",
			secrets:     []string{"ambient-secret"},
		},
		{
			name:        "CLI key requires explicit model",
			args:        []string{"--api-key", "cli-secret", "-p", "hello"},
			environment: []string{"OPENAI_API_KEY=ambient-secret"},
			prepare:     func(*testing.T, string) {},
			want:        "--api-key requires an explicit --model",
			secrets:     []string{"cli-secret", "ambient-secret"},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && testCase.storedFile {
				t.Skip("Windows v0.1 rejects existing persistent auth before credential parsing")
			}
			workingDir := t.TempDir()
			agentDir := t.TempDir()
			testCase.prepare(t, agentDir)
			sessionParent := filepath.Join(workingDir, "must-not-exist")
			sessionPath := filepath.Join(sessionParent, "session.jsonl")
			args := append(append([]string(nil), testCase.args...), "--session", sessionPath)
			doer := &countingProductionDoer{}
			config := productionTestConfig(workingDir, agentDir, testCase.environment)
			config.OpenAIHTTPClient = doer
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := app.RunProduction(context.Background(), config, args, &stdout, &stderr)
			if exitCode != app.ExitFailure || stdout.Len() != 0 || !strings.Contains(stderr.String(), testCase.want) {
				t.Fatalf("RunProduction() = code %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
			}
			for _, secret := range testCase.secrets {
				if strings.Contains(stderr.String(), secret) {
					t.Fatalf("stderr leaked secret %q: %q", secret, stderr.String())
				}
			}
			if doer.calls.Load() != 0 {
				t.Fatalf("preflight made %d HTTP calls", doer.calls.Load())
			}
			if _, err := os.Stat(sessionParent); !os.IsNotExist(err) {
				t.Fatalf("preflight changed session tree: %v", err)
			}
		})
	}
}

func TestRunProductionHTTPFailureIsDurable(t *testing.T) {
	workingDir := t.TempDir()
	agentDir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(writer, `{"error":{"message":"fixture rejected request","code":"invalid_api_key"}}`)
	}))
	defer server.Close()
	writeModelsJSON(t, agentDir, server.URL+"/v1", stringPointer("fixture-key"), nil)
	sessionPath := filepath.Join(workingDir, "failure.jsonl")
	config := productionTestConfig(workingDir, agentDir, nil)
	config.Environment = []string{}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := app.RunProduction(context.Background(), config, []string{
		"--model", "openai/gpt-failure", "-p", "fail", "--session", sessionPath,
	}, &stdout, &stderr)
	if exitCode != app.ExitFailure || stdout.Len() != 0 || stderr.Len() == 0 {
		t.Fatalf("RunProduction() = code %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
	}
	transcript, err := session.Open(sessionPath, session.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer transcript.Close()
	messages := transcript.Context().Messages()
	if len(messages) != 2 || messages[0].Role() != llm.RoleUser {
		t.Fatalf("durable failure messages = %#v", messages)
	}
	failure, ok := messages[1].(llm.AssistantFailureMessage)
	if !ok || failure.FinishReason() != llm.FinishError {
		t.Fatalf("durable terminal = %T", messages[1])
	}
}

func TestRunProductionResumeDoesNotReplayCreationClock(t *testing.T) {
	workingDir := t.TempDir()
	agentDir := t.TempDir()
	sessionPath := filepath.Join(workingDir, "existing.jsonl")
	existing, err := session.Create(sessionPath, session.CreateOptions{
		ID:         "existing-session",
		WorkingDir: workingDir,
		Now:        func() time.Time { return productionTestTime.Add(-time.Hour) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := existing.Close(); err != nil {
		t.Fatal(err)
	}

	capture := &capturedProductionRequest{}
	server := newProductionTextServer(t, capture, "resumed")
	defer server.Close()
	writeModelsJSON(t, agentDir, server.URL+"/v1", stringPointer("fixture-key"), nil)
	config := productionTestConfig(workingDir, agentDir, nil)
	var ticks atomic.Int64
	config.SessionNow = func() time.Time {
		return productionTestTime.Add(time.Duration(ticks.Add(1)-1) * time.Millisecond)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := app.RunProduction(context.Background(), config, []string{
		"--model", "openai/gpt-resume", "-p", "continue", "--session", sessionPath,
	}, &stdout, &stderr)
	if exitCode != app.ExitSuccess || stdout.String() != "resumed\n" || stderr.Len() != 0 {
		t.Fatalf("RunProduction() = code %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
	}

	transcript, err := session.Open(sessionPath, session.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer transcript.Close()
	entries := transcript.Entries()
	if len(entries) != 2 {
		t.Fatalf("resumed entries = %d, want 2", len(entries))
	}
	if entries[0].Timestamp() != productionTestTime.Add(time.Millisecond) ||
		entries[1].Timestamp() != productionTestTime.Add(2*time.Millisecond) {
		t.Fatalf("resumed entry timestamps = %v, %v", entries[0].Timestamp(), entries[1].Timestamp())
	}
}

func productionTestConfig(workingDir, agentDir string, environment []string) app.ProductionConfig {
	var entryIDs atomic.Uint64
	environmentCopy := make([]string, len(environment))
	copy(environmentCopy, environment)
	return app.ProductionConfig{
		WorkingDir:  workingDir,
		AgentDir:    agentDir,
		Environment: environmentCopy,
		OpenAIClock: func() time.Time { return productionTestTime },
		SessionID:   "production-session",
		SessionNow:  func() time.Time { return productionTestTime },
		NewSessionEntryID: func() (string, error) {
			return fmt.Sprintf("production-entry-%06d", entryIDs.Add(1)), nil
		},
		AgentNow: func() time.Time { return productionTestTime },
	}
}

func newProductionTextServer(
	t *testing.T,
	capture *capturedProductionRequest,
	text string,
) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := capture.record(request); err != nil {
			t.Errorf("decode production request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		item := map[string]any{
			"type": "message", "id": "msg-production", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text}},
		}
		writeProductionSSE(t, writer,
			map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item},
			map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"status": "completed",
					"output": []any{item},
					"usage":  map[string]any{"input_tokens": 3, "output_tokens": 2, "total_tokens": 5},
				},
			},
		)
	}))
}

func writeProductionSSE(t *testing.T, writer io.Writer, events ...map[string]any) {
	t.Helper()
	for _, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Errorf("encode SSE event: %v", err)
			return
		}
		if _, err := fmt.Fprintf(writer, "data: %s\n\n", encoded); err != nil {
			t.Errorf("write SSE event: %v", err)
			return
		}
	}
}

func writeModelsJSON(
	t *testing.T,
	agentDir string,
	baseURL string,
	apiKey *string,
	extra map[string]any,
) {
	t.Helper()
	openAI := map[string]any{"baseUrl": baseURL}
	if apiKey != nil {
		openAI["apiKey"] = *apiKey
	}
	for key, value := range extra {
		openAI[key] = value
	}
	encoded, err := json.MarshalIndent(map[string]any{
		"providers": map[string]any{"openai": openAI},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the fixed upstream's JSONC admission, including trailing commas.
	content := "// production fixture\n" + strings.TrimSuffix(string(encoded), "\n")
	content = strings.Replace(content, "\n  }\n}", ",\n  }\n}", 1)
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeAuthJSON(t *testing.T, agentDir, content string) {
	t.Helper()
	path := filepath.Join(agentDir, "auth.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func expectedProductionSessionPath(agentDir, workingDir, sessionID string) string {
	encoded := filepath.Clean(workingDir)
	if len(encoded) > 0 && (encoded[0] == '/' || encoded[0] == '\\') {
		encoded = encoded[1:]
	}
	encoded = strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(encoded)
	return filepath.Join(
		agentDir,
		"sessions",
		"--"+encoded+"--",
		"2026-08-02T09-10-11-120Z_"+sessionID+".jsonl",
	)
}

func stringPointer(value string) *string {
	return &value
}
