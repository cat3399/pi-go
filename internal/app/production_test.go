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
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/resource"
	"github.com/cat3399/pi-go/internal/session"
	"github.com/cat3399/pi-go/internal/tool"
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
	if exitCode != app.ExitSuccess || stdout.String() != "assembled answer\n" || stderr.String() != customModelWarning("gpt-production") {
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
	if !ok || len(input) != 2 {
		t.Fatalf("payload input = %#v", request.payload["input"])
	}
	system, ok := input[0].(map[string]any)
	if !ok || system["role"] != "developer" || !strings.Contains(system["content"].(string), "Available tools:") {
		t.Fatalf("system prompt = %#v", input[0])
	}
	user, ok := input[1].(map[string]any)
	if !ok || user["role"] != "user" {
		t.Fatalf("payload user = %#v", input[1])
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

func TestRunProductionRoutesOpenAIChatCompletionsModel(t *testing.T) {
	workingDir, agentDir := t.TempDir(), t.TempDir()
	capture := &capturedProductionRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := capture.record(r); err != nil {
			t.Errorf("record: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"completion answer"},"finish_reason":null}]}`+"\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	key := "key"
	writeModelsJSON(t, agentDir, server.URL+"/v1", &key, map[string]any{"models": []map[string]any{{"id": "gpt-5.5", "api": "openai-completions"}}})
	config := productionTestConfig(workingDir, agentDir, nil)
	var stdout, stderr bytes.Buffer
	if code := app.RunProduction(context.Background(), config, []string{"--model", "openai/gpt-5.5", "--session", filepath.Join(workingDir, "session.jsonl"), "-p", "hello"}, &stdout, &stderr); code != app.ExitSuccess || stdout.String() != "completion answer\n" || stderr.Len() != 0 {
		t.Fatalf("RunProduction=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	request := capture.snapshot()
	if request.path != "/v1/chat/completions" || request.payload["stream"] != true {
		t.Fatalf("request=%#v", request)
	}
}

func TestRunProductionRoutesBuiltinDeepSeekWithProviderEnvironment(t *testing.T) {
	workingDir, agentDir := t.TempDir(), t.TempDir()
	capture := &capturedProductionRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := capture.record(r); err != nil {
			t.Errorf("record: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"deepseek answer"},"finish_reason":null}]}`+"\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	models, err := json.Marshal(map[string]any{"providers": map[string]any{"deepseek": map[string]any{"baseUrl": server.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), models, 0o600); err != nil {
		t.Fatal(err)
	}
	config := productionTestConfig(workingDir, agentDir, []string{"DEEPSEEK_API_KEY=deepseek-key"})
	var stdout, stderr bytes.Buffer
	code := app.RunProduction(context.Background(), config, []string{
		"--model", "deepseek/deepseek-v4-pro", "--session", filepath.Join(workingDir, "deepseek.jsonl"), "-p", "hello",
	}, &stdout, &stderr)
	if code != app.ExitSuccess || stdout.String() != "deepseek answer\n" || stderr.Len() != 0 {
		t.Fatalf("RunProduction=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	request := capture.snapshot()
	if request.path != "/chat/completions" || request.authorization != "Bearer deepseek-key" || request.payload["model"] != "deepseek-v4-pro" {
		t.Fatalf("DeepSeek request = %#v", request)
	}
}

func TestRunProductionCustomFallbackNeverSendsKeyToConfiguredModelURL(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		args     []string
		settings string
	}{
		{name: "explicit", args: []string{"--model", "OPENAI/custom", "-p", "hello"}},
		{name: "settings default", args: []string{"-p", "hello"}, settings: `{"defaultProvider":"openai","defaultModel":"custom"}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			workingDir, agentDir := t.TempDir(), t.TempDir()
			capture := &capturedProductionRequest{}
			providerServer := newProductionTextServer(t, capture, "safe")
			defer providerServer.Close()
			var poisonCalls atomic.Uint32
			var poisonAuthorization atomic.Value
			poisonServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				poisonCalls.Add(1)
				poisonAuthorization.Store(request.Header.Get("Authorization"))
				writer.WriteHeader(http.StatusInternalServerError)
			}))
			defer poisonServer.Close()
			key := "provider-key"
			models := []any{map[string]any{"id": "aaa", "api": "openai-responses", "baseUrl": poisonServer.URL + "/v1"}}
			if testCase.settings != "" {
				models = append(models, map[string]any{"id": "custom", "api": "openai-responses", "baseUrl": providerServer.URL + "/v1"})
			}
			writeModelsJSON(t, agentDir, providerServer.URL+"/v1", &key, map[string]any{"models": models})
			if testCase.settings != "" {
				if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), []byte(testCase.settings), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			args := append(append([]string(nil), testCase.args...), "--session", filepath.Join(workingDir, "session.jsonl"))
			config := productionTestConfig(workingDir, agentDir, []string{})
			var stdout, stderr bytes.Buffer
			exitCode := app.RunProduction(context.Background(), config, args, &stdout, &stderr)
			wantStderr := ""
			if testCase.name == "explicit" {
				wantStderr = customModelWarning("custom")
			}
			if exitCode != app.ExitSuccess || stdout.String() != "safe\n" || stderr.String() != wantStderr {
				t.Fatalf("RunProduction = %d, %q, %q", exitCode, stdout.String(), stderr.String())
			}
			request := capture.snapshot()
			if request.count != 1 || request.authorization != "Bearer provider-key" || request.payload["model"] != "custom" {
				t.Fatalf("provider request = %#v", request)
			}
			if poisonCalls.Load() != 0 {
				t.Fatalf("aaa endpoint received %d request(s), authorization=%v", poisonCalls.Load(), poisonAuthorization.Load())
			}
		})
	}
}

type concurrentProductionRunner struct {
	slowStarted  chan struct{}
	fastStarted  chan struct{}
	fastDone     chan struct{}
	slowOnce     sync.Once
	fastOnce     sync.Once
	fastDoneOnce sync.Once

	mu        sync.Mutex
	completed []string
}

func newConcurrentProductionRunner() *concurrentProductionRunner {
	return &concurrentProductionRunner{
		slowStarted: make(chan struct{}),
		fastStarted: make(chan struct{}),
		fastDone:    make(chan struct{}),
	}
}

func (r *concurrentProductionRunner) Run(ctx context.Context, request tool.RunRequest, sink tool.OutputSink) (tool.ExitStatus, error) {
	command := request.Command()
	switch command {
	case "slow":
		r.slowOnce.Do(func() { close(r.slowStarted) })
		if err := waitProductionRunner(ctx, r.fastStarted); err != nil {
			return tool.UnknownExitStatus(), err
		}
		if err := waitProductionRunner(ctx, r.fastDone); err != nil {
			return tool.UnknownExitStatus(), err
		}
		if err := sink([]byte("slow-output")); err != nil {
			return tool.UnknownExitStatus(), err
		}
		r.recordCompletion(command)
	case "fast":
		r.fastOnce.Do(func() { close(r.fastStarted) })
		if err := waitProductionRunner(ctx, r.slowStarted); err != nil {
			return tool.UnknownExitStatus(), err
		}
		if err := sink([]byte("fast-output")); err != nil {
			return tool.UnknownExitStatus(), err
		}
		r.recordCompletion(command)
		r.fastDoneOnce.Do(func() { close(r.fastDone) })
	default:
		return tool.UnknownExitStatus(), fmt.Errorf("unexpected production command %q", command)
	}
	status, err := tool.NewExitStatus(0)
	if err != nil {
		return tool.UnknownExitStatus(), err
	}
	return status, nil
}

func waitProductionRunner(ctx context.Context, ready <-chan struct{}) error {
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (r *concurrentProductionRunner) recordCompletion(command string) {
	r.mu.Lock()
	r.completed = append(r.completed, command)
	r.mu.Unlock()
}

func (r *concurrentProductionRunner) completionOrder() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.completed...)
}

func TestRunProductionOpenAIMultiToolWorkflowExecutesConcurrentlyAndReplaysSourceOrder(t *testing.T) {
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
		if err := simulatedOpenAIResponsesAdmission(payload); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		payloads = append(payloads, payload)
		turn := len(payloads)
		mu.Unlock()
		writer.Header().Set("Content-Type", "text/event-stream")
		if turn == 1 {
			slow := map[string]any{"type": "function_call", "id": "fc_slow", "call_id": "call_slow", "name": "bash", "arguments": `{"command":"slow"}`}
			fast := map[string]any{"type": "function_call", "id": "fc_fast", "call_id": "call_fast", "name": "bash", "arguments": `{"command":"fast"}`}
			writeProductionSSE(t, writer,
				map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc_slow", "call_id": "call_slow", "name": "bash", "arguments": ""}},
				map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": "fc_slow", "delta": `{"command":"slow"}`},
				map[string]any{"type": "response.function_call_arguments.done", "output_index": 0, "item_id": "fc_slow", "arguments": `{"command":"slow"}`},
				map[string]any{"type": "response.output_item.done", "output_index": 0, "item": slow},
				map[string]any{"type": "response.output_item.added", "output_index": 1, "item": map[string]any{"type": "function_call", "id": "fc_fast", "call_id": "call_fast", "name": "bash", "arguments": ""}},
				map[string]any{"type": "response.function_call_arguments.delta", "output_index": 1, "item_id": "fc_fast", "delta": `{"command":"fast"}`},
				map[string]any{"type": "response.function_call_arguments.done", "output_index": 1, "item_id": "fc_fast", "arguments": `{"command":"fast"}`},
				map[string]any{"type": "response.output_item.done", "output_index": 1, "item": fast},
				map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{slow, fast}}},
			)
			return
		}
		item := map[string]any{"type": "message", "id": "msg_final", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "tools completed"}}}
		writeProductionSSE(t, writer,
			map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item},
			map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{item}}},
		)
	}))
	defer server.Close()
	writeModelsJSON(t, agentDir, server.URL+"/v1", stringPointer("fixture-key"), nil)
	config := productionTestConfig(workingDir, agentDir, nil)
	runner := newConcurrentProductionRunner()
	config.BashRunner = runner
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exitCode := app.RunProduction(ctx, config, []string{"--model", "openai/gpt-tool", "--session", sessionPath, "-p", "use bash twice"}, &stdout, &stderr)
	if exitCode != app.ExitSuccess || stdout.String() != "tools completed\n" || stderr.String() != customModelWarning("gpt-tool") {
		t.Fatalf("RunProduction() = code %d stdout %q stderr %q", exitCode, stdout.String(), stderr.String())
	}
	if got := strings.Join(runner.completionOrder(), ","); got != "fast,slow" {
		t.Fatalf("tool completion order = %q, want fast,slow", got)
	}
	mu.Lock()
	received := append([]map[string]any(nil), payloads...)
	mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("request count = %d", len(received))
	}
	for requestIndex, payload := range received {
		if _, sent := payload["parallel_tool_calls"]; sent {
			t.Fatalf("request %d sent upstream-absent parallel_tool_calls = %#v", requestIndex+1, payload["parallel_tool_calls"])
		}
		tools, ok := payload["tools"].([]any)
		if !ok || len(tools) != 4 {
			t.Fatalf("request %d tools = %#v", requestIndex+1, payload["tools"])
		}
		wantToolNames := []string{"read", "bash", "edit", "write"}
		for toolIndex, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok || tool["name"] != wantToolNames[toolIndex] || tool["strict"] != false {
				t.Fatalf("request %d tool %d strict = %#v, want false", requestIndex+1, toolIndex, raw)
			}
		}
	}
	input, ok := received[1]["input"].([]any)
	if !ok || len(input) != 6 {
		t.Fatalf("second request input = %#v", received[1]["input"])
	}
	if system, ok := input[0].(map[string]any); !ok || system["role"] != "developer" || !strings.Contains(system["content"].(string), "Available tools:") {
		t.Fatalf("second request system prompt = %#v", input[0])
	}
	wantReplay := []struct {
		index    int
		typeName string
		callID   string
		itemID   string
		output   string
	}{
		{index: 2, typeName: "function_call", callID: "call_slow", itemID: "fc_slow"},
		{index: 3, typeName: "function_call", callID: "call_fast", itemID: "fc_fast"},
		{index: 4, typeName: "function_call_output", callID: "call_slow", output: "slow-output"},
		{index: 5, typeName: "function_call_output", callID: "call_fast", output: "fast-output"},
	}
	for _, want := range wantReplay {
		item, ok := input[want.index].(map[string]any)
		if !ok || item["type"] != want.typeName || item["call_id"] != want.callID ||
			(want.itemID != "" && item["id"] != want.itemID) || (want.output != "" && item["output"] != want.output) {
			t.Fatalf("second request item %d = %#v", want.index, input[want.index])
		}
	}
	transcript, err := session.Open(sessionPath, session.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer transcript.Close()
	messages := transcript.Context().Messages()
	if len(messages) != 5 || messages[0].Role() != llm.RoleUser || messages[1].Role() != llm.RoleAssistant || messages[2].Role() != llm.RoleToolResult || messages[3].Role() != llm.RoleToolResult || messages[4].Role() != llm.RoleAssistant {
		t.Fatalf("durable roles = %#v", messages)
	}
	firstResult := messages[2].(llm.ToolResultMessage)
	secondResult := messages[3].(llm.ToolResultMessage)
	if firstResult.ToolCallID() != "call_slow|fc_slow" || firstResult.Content()[0].Text() != "slow-output" ||
		secondResult.ToolCallID() != "call_fast|fc_fast" || secondResult.Content()[0].Text() != "fast-output" {
		t.Fatalf("durable source-order results = %#v / %#v", firstResult, secondResult)
	}
}

func TestRunProductionReplaysRichMultiToolSessionAfterRestart(t *testing.T) {
	workingDir, agentDir := t.TempDir(), t.TempDir()
	sessionPath := filepath.Join(workingDir, "rich-tool-restart.jsonl")
	entryIDs := []string{"seed-user", "seed-assistant", "result-slow", "result-fast", "foreign-assistant"}
	transcript, err := session.Create(sessionPath, session.CreateOptions{
		ID:         "rich-tool-restart",
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
	seedText, err := llm.NewTextBlock("inspect the image with both tools")
	if err != nil {
		t.Fatal(err)
	}
	seedUser, err := llm.NewUserContentMessage([]llm.UserContentBlock{seedText, image}, productionTestTime)
	if err != nil {
		t.Fatal(err)
	}
	reasoning, err := llm.NewThinkingBlockWithSignature("parallel plan", `{"type":"reasoning","id":"rs_integrated","summary":[{"type":"summary_text","text":"parallel plan"}],"encrypted_content":"integrated-cipher"}`, false)
	if err != nil {
		t.Fatal(err)
	}
	commentary, err := llm.NewTextBlockWithSignature("starting both tools", `{"v":1,"id":"msg_integrated","phase":"commentary"}`)
	if err != nil {
		t.Fatal(err)
	}
	slowCall, err := llm.NewToolCallBlock("call_slow|fc_slow", "bash", []byte(`{"command":"slow"}`))
	if err != nil {
		t.Fatal(err)
	}
	fastCall, err := llm.NewToolCallBlock("call_fast|fc_fast", "bash", []byte(`{"command":"fast"}`))
	if err != nil {
		t.Fatal(err)
	}
	responsesProvenance := llm.AssistantProvenance{
		Provider: provider.OpenAIProviderID,
		API:      provider.OpenAIResponsesAPI,
		Model:    "gpt-rich-tools",
	}
	assistant, err := llm.NewAssistantToolUseMessageWithMetadata(
		[]llm.AssistantBlock{reasoning, commentary, slowCall, fastCall},
		llm.Usage{},
		productionTestTime,
		responsesProvenance,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	slowResult, err := llm.NewToolResultMessage(slowCall.ID(), slowCall.Name(), []llm.TextBlock{mustProductionTextBlock(t, "slow-output")}, false, productionTestTime)
	if err != nil {
		t.Fatal(err)
	}
	fastResult, err := llm.NewToolResultMessage(fastCall.ID(), fastCall.Name(), []llm.TextBlock{mustProductionTextBlock(t, "fast-output")}, false, productionTestTime)
	if err != nil {
		t.Fatal(err)
	}
	foreignReasoning, err := llm.NewThinkingBlockWithSignature("foreign readable plan", `{"type":"reasoning","id":"rs_foreign","encrypted_content":"foreign-cipher"}`, false)
	if err != nil {
		t.Fatal(err)
	}
	foreignProvenance := llm.AssistantProvenance{Provider: "anthropic", API: "anthropic-messages", Model: "claude-test"}
	foreignAssistant, err := llm.NewAssistantRichMessageWithMetadata(
		[]llm.AssistantBlock{foreignReasoning},
		llm.FinishStop,
		llm.Usage{},
		productionTestTime,
		foreignProvenance,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, appendCase := range []struct {
		message llm.ConversationMessage
		options session.AppendOptions
	}{
		{message: seedUser},
		{message: assistant},
		{message: slowResult},
		{message: fastResult},
		{message: foreignAssistant},
	} {
		if _, err := transcript.Append(context.Background(), appendCase.message, appendCase.options); err != nil {
			t.Fatal(err)
		}
	}
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}
	beforeRun, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(beforeRun, []byte("foreign-cipher")) {
		t.Fatal("foreign signature was not durably preserved before restart")
	}

	capture := &capturedProductionRequest{}
	server := newProductionTextServer(t, capture, "rich tools resumed")
	defer server.Close()
	writeModelsJSON(t, agentDir, server.URL+"/v1", stringPointer("fixture-key"), nil)
	config := productionTestConfig(workingDir, agentDir, nil)
	var stdout, stderr bytes.Buffer
	if code := app.RunProduction(context.Background(), config, []string{
		"--model", "openai/gpt-rich-tools", "--session", sessionPath, "-p", "continue after restart",
	}, &stdout, &stderr); code != app.ExitSuccess || stdout.String() != "rich tools resumed\n" || stderr.String() != customModelWarning("gpt-rich-tools") {
		t.Fatalf("RunProduction() = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}

	snapshot := capture.snapshot()
	if snapshot.count != 1 {
		t.Fatalf("request count = %d, want 1", snapshot.count)
	}
	if err := simulatedOpenAIResponsesAdmission(snapshot.payload); err != nil {
		t.Fatal(err)
	}
	tools, ok := snapshot.payload["tools"].([]any)
	if !ok || len(tools) != 4 {
		t.Fatalf("tools = %#v, want four default production definitions", snapshot.payload["tools"])
	}
	encodedPayload, err := json.Marshal(snapshot.payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"rs_foreign", "foreign-cipher"} {
		if bytes.Contains(encodedPayload, []byte(forbidden)) {
			t.Fatalf("foreign signature %q entered request: %s", forbidden, encodedPayload)
		}
	}

	input, ok := snapshot.payload["input"].([]any)
	if !ok {
		t.Fatalf("input = %#v", snapshot.payload["input"])
	}
	positions := make(map[string]int)
	foundPNG := false
	foundForeignFallback := false
	for index, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch item["type"] {
		case "reasoning":
			if item["id"] == "rs_integrated" && item["encrypted_content"] == "integrated-cipher" {
				positions["reasoning"] = index
			}
		case "message":
			content, _ := item["content"].([]any)
			if len(content) == 1 {
				block, _ := content[0].(map[string]any)
				switch block["text"] {
				case "starting both tools":
					if item["id"] != "msg_integrated" || item["phase"] != "commentary" {
						t.Fatalf("same-provenance text replay = %#v", item)
					}
					positions["text"] = index
				case "foreign readable plan":
					foundForeignFallback = true
				}
			}
		case "function_call":
			switch item["call_id"] {
			case "call_slow":
				if item["id"] != "fc_slow" {
					t.Fatalf("slow function replay = %#v", item)
				}
				positions["call-slow"] = index
			case "call_fast":
				if item["id"] != "fc_fast" {
					t.Fatalf("fast function replay = %#v", item)
				}
				positions["call-fast"] = index
			}
		case "function_call_output":
			switch item["call_id"] {
			case "call_slow":
				if item["output"] != "slow-output" {
					t.Fatalf("slow output replay = %#v", item)
				}
				positions["output-slow"] = index
			case "call_fast":
				if item["output"] != "fast-output" {
					t.Fatalf("fast output replay = %#v", item)
				}
				positions["output-fast"] = index
			}
		}
		content, _ := item["content"].([]any)
		for _, rawBlock := range content {
			block, _ := rawBlock.(map[string]any)
			if block["type"] == "input_image" && block["image_url"] == "data:image/png;base64,"+pngBase64 {
				foundPNG = true
			}
		}
	}
	for _, key := range []string{"reasoning", "text", "call-slow", "call-fast", "output-slow", "output-fast"} {
		if _, ok := positions[key]; !ok {
			t.Fatalf("missing %s in input: %#v", key, input)
		}
	}
	if !(positions["reasoning"] < positions["text"] &&
		positions["text"] < positions["call-slow"] &&
		positions["call-slow"] < positions["call-fast"] &&
		positions["call-fast"] < positions["output-slow"] &&
		positions["output-slow"] < positions["output-fast"]) {
		t.Fatalf("rich/tool replay order = %#v", positions)
	}
	if !foundPNG || !foundForeignFallback {
		t.Fatalf("PNG/foreign safe fallback = %t/%t; input = %#v", foundPNG, foundForeignFallback, input)
	}
	afterRun, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(afterRun, beforeRun) || !bytes.Contains(afterRun, []byte("foreign-cipher")) {
		t.Fatal("restart append did not preserve the original foreign signature bytes")
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
			reasoningDone := map[string]any{"type": "reasoning", "id": "rs_persist", "summary": []any{map[string]any{"type": "summary_text", "text": "plan"}}}
			reasoningTerminal := map[string]any{"type": "reasoning", "id": "rs_persist", "summary": []any{map[string]any{"type": "summary_text", "text": "plan"}}, "encrypted_content": "cipher"}
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
		if code := app.RunProduction(context.Background(), config, []string{"--model", "openai/gpt-reason", "--session", sessionPath, "-p", prompt}, &stdout, &stderr); code != app.ExitSuccess || stderr.String() != customModelWarning("gpt-reason") {
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
			foundReasoning = wire["id"] == "rs_persist" && wire["encrypted_content"] == "cipher" && reflect.DeepEqual(wire["summary"], []any{map[string]any{"type": "summary_text", "text": "plan"}})
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
	if response, ok := messages[1].(llm.AssistantRichMessage).ResponseMetadata(); !ok || response.RawStopReason != "completed" {
		t.Fatalf("response metadata=%#v", response)
	}
}

func TestRunProductionReplaysDurableImageAfterRestart(t *testing.T) {
	workingDir, agentDir := t.TempDir(), t.TempDir()
	sessionPath := filepath.Join(workingDir, "image-restart.jsonl")
	entryIDs := []string{"seed-image", "bootstrap-thinking", "new-user", "assistant"}
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
	if code := app.RunProduction(context.Background(), config, []string{"--model", "openai/gpt-image", "--session", sessionPath, "-p", "continue"}, &stdout, &stderr); code != app.ExitSuccess || stderr.String() != customModelWarning("gpt-image") {
		t.Fatalf("RunProduction() = code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	input := capture.snapshot().payload["input"].([]any)
	if len(input) < 2 {
		t.Fatalf("resumed input = %#v", input)
	}
	var imageWire map[string]any
	for _, raw := range input {
		message, ok := raw.(map[string]any)
		if !ok || message["role"] != "user" {
			continue
		}
		content, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, rawBlock := range content {
			block, ok := rawBlock.(map[string]any)
			if ok && block["type"] == "input_image" {
				imageWire = block
			}
		}
	}
	if imageWire == nil || imageWire["image_url"] != "data:image/png;base64,"+pngBase64 {
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
			args:          []string{"--model", "openai/gpt-5.5", "-p", "hello"},
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
			wantStderr := ""
			if strings.HasPrefix(testCase.wantModel, "custom-") {
				wantStderr = customModelWarning(testCase.wantModel)
			}
			if exitCode != app.ExitSuccess || stdout.String() != "ok\n" || stderr.String() != wantStderr {
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

func TestRunProductionRefreshesStoredOpenAICodexOAuthBeforeResponsesRequest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("persistent auth remains fail-closed on Windows")
	}
	workingDir, agentDir := t.TempDir(), t.TempDir()
	access := productionOAuthJWT("refreshed-account")
	var tokenCalls atomic.Int32
	var responseAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/oauth/token":
			tokenCalls.Add(1)
			if err := request.ParseForm(); err != nil || request.Form.Get("grant_type") != "refresh_token" || request.Form.Get("refresh_token") != "old-refresh" {
				t.Errorf("refresh form = %#v, %v", request.Form, err)
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"access_token":`+jsonString(access)+`,"refresh_token":"new-refresh","expires_in":3600}`)
		case "/codex/responses":
			if strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
				http.Error(writer, "SSE fixture", http.StatusBadRequest)
				return
			}
			responseAuthorization = request.Header.Get("Authorization")
			_, _ = io.Copy(io.Discard, request.Body)
			writer.Header().Set("Content-Type", "text/event-stream")
			item := map[string]any{"type": "message", "id": "oauth-message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "oauth answer"}}}
			writeProductionSSE(t, writer,
				map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item},
				map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}},
			)
		default:
			t.Errorf("unexpected request %s", request.URL.Path)
		}
	}))
	defer server.Close()
	writeProviderModelsJSON(t, agentDir, map[string]any{"openai-codex": map[string]any{"baseUrl": server.URL}})
	writeAuthJSON(t, agentDir, `{"openai-codex":{"type":"oauth","access":`+jsonString(productionOAuthJWT("old-account"))+`,"refresh":"old-refresh","expires":1,"accountId":"old-account"}}`)
	config := productionTestConfig(workingDir, agentDir, nil)
	config.OpenAIOAuthBaseURL = server.URL
	var stdout, stderr bytes.Buffer
	code := app.RunProduction(context.Background(), config, []string{"--model", "openai-codex/gpt-oauth", "--session", filepath.Join(workingDir, "oauth.jsonl"), "-p", "hello"}, &stdout, &stderr)
	warning := "Warning: Model \"gpt-oauth\" not found for provider \"openai-codex\". Using custom model id.\n"
	if code != app.ExitSuccess || stdout.String() != "oauth answer\n" || stderr.String() != warning || tokenCalls.Load() != 1 || responseAuthorization != "Bearer "+access {
		t.Fatalf("RunProduction OAuth = code %d stdout %q stderr %q tokens %d authorization %q", code, stdout.String(), stderr.String(), tokenCalls.Load(), responseAuthorization)
	}
	data, err := os.ReadFile(filepath.Join(agentDir, "auth.json"))
	if err != nil || !strings.Contains(string(data), `"refresh": "new-refresh"`) || strings.Contains(string(data), "old-refresh") {
		t.Fatalf("rotated auth.json = %q, %v", data, err)
	}
}

func productionOAuthJWT(account string) string {
	payload, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": account}})
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func jsonString(value string) string { encoded, _ := json.Marshal(value); return string(encoded) }

type countingProductionDoer struct {
	calls atomic.Uint32
}

func (d *countingProductionDoer) Do(*http.Request) (*http.Response, error) {
	d.calls.Add(1)
	return nil, fmt.Errorf("network must not be reached")
}

func TestRunProductionSessionFirstFailuresAreSecretSafeAndDoNotPersistResults(t *testing.T) {
	testCases := []struct {
		name        string
		args        []string
		environment []string
		prepare     func(*testing.T, string)
		want        string
		wantCalls   uint32
		secrets     []string
		storedFile  bool
		modelAccess bool
		exactStderr string
	}{
		{
			name:        "stored OAuth owns provider",
			args:        []string{"--model", "openai/gpt-test", "-p", "hello"},
			environment: []string{"OPENAI_API_KEY=ambient-secret"},
			prepare: func(t *testing.T, agentDir string) {
				writeModelsJSON(t, agentDir, "https://fixture.invalid/v1", nil, nil)
				writeAuthJSON(t, agentDir, `{"openai":{"type":"oauth","access":"stored-secret"}}`)
			},
			want:       "auth.json is malformed",
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
			name:        "failed command-backed configured key is explicit",
			args:        []string{"--model", "openai/gpt-test", "-p", "hello"},
			environment: []string{"OPENAI_API_KEY=ambient-secret"},
			prepare: func(t *testing.T, agentDir string) {
				writeModelsJSON(t, agentDir, "https://fixture.invalid/v1", stringPointer("!exit 7 # command-secret"), nil)
			},
			want:    "cannot resolve command-backed configured provider credential",
			secrets: []string{"ambient-secret", "command-secret"},
		},
		{
			name:        "selected provider cached catalog is invalid",
			args:        []string{"--model", "openai/gpt-5.5", "-p", "hello"},
			environment: []string{"OPENAI_API_KEY=ambient-secret"},
			prepare: func(t *testing.T, agentDir string) {
				if err := os.WriteFile(filepath.Join(agentDir, "models-store.json"), []byte(`{"openai":{"models":"invalid"}}`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want:    "selected provider/API is not supported",
			secrets: []string{"ambient-secret"},
		},
		{
			name: "missing all credential sources",
			args: []string{"--model", "openai/gpt-test", "-p", "hello"},
			prepare: func(t *testing.T, agentDir string) {
				writeModelsJSON(t, agentDir, "https://fixture.invalid/v1", nil, nil)
			},
			want:        "No API key found for openai.",
			modelAccess: true,
		},
		{
			name:        "unknown provider route",
			args:        []string{"--model", "unknown/model", "-p", "hello"},
			environment: []string{"OPENAI_API_KEY=ambient-secret"},
			prepare:     func(*testing.T, string) {},
			want:        `Model "unknown/model" not found`,
			secrets:     []string{"ambient-secret"},
		},
		{
			name:        "CLI key requires explicit model",
			args:        []string{"--api-key", "cli-secret", "-p", "hello"},
			environment: []string{"OPENAI_API_KEY=ambient-secret"},
			prepare:     func(*testing.T, string) {},
			want:        "--api-key requires a model to be specified",
			secrets:     []string{"cli-secret", "ambient-secret"},
			exactStderr: "Error: --api-key requires a model to be specified via --model, --provider/--model, or --models\n",
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
			var beforeAgentCalls atomic.Uint32
			var sessionStartCalls atomic.Uint32
			var sessionShutdownCalls atomic.Uint32
			config.Hooks.BeforeAgentStart = func(context.Context, agent.BeforeAgentStartEvent) (agent.BeforeAgentStartResult, error) {
				beforeAgentCalls.Add(1)
				return agent.BeforeAgentStartResult{}, nil
			}
			config.Hooks.SessionStart = func(context.Context, agent.SessionStartHookEvent) error {
				sessionStartCalls.Add(1)
				return nil
			}
			config.Hooks.SessionShutdown = func(context.Context, agent.SessionShutdownHookEvent) error {
				sessionShutdownCalls.Add(1)
				return nil
			}
			if testCase.modelAccess {
				config.DocsDir = filepath.Join(workingDir, "installed-docs")
				if err := os.Mkdir(config.DocsDir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
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
			if testCase.exactStderr != "" && stderr.String() != testCase.exactStderr {
				t.Fatalf("stderr = %q, want %q", stderr.String(), testCase.exactStderr)
			}
			if doer.calls.Load() != testCase.wantCalls {
				t.Fatalf("provider calls = %d, want %d", doer.calls.Load(), testCase.wantCalls)
			}
			if beforeAgentCalls.Load() != 0 {
				t.Fatalf("before_agent_start calls = %d", beforeAgentCalls.Load())
			}
			if testCase.exactStderr != "" && (sessionStartCalls.Load() != 0 || sessionShutdownCalls.Load() != 0) {
				t.Fatalf(
					"blocking diagnostic lifecycle calls: start=%d shutdown=%d",
					sessionStartCalls.Load(), sessionShutdownCalls.Load(),
				)
			}
			if testCase.modelAccess {
				want := customModelWarning("gpt-test") + "No API key found for openai.\n\nUse /login to log into a provider via OAuth or API key. See:\n  " +
					filepath.Join(config.DocsDir, "providers.md") + "\n  " + filepath.Join(config.DocsDir, "models.md") + "\n"
				if stderr.String() != want {
					t.Fatalf("model access stderr = %q, want %q", stderr.String(), want)
				}
			}
			if _, err := os.Stat(sessionPath); !os.IsNotExist(err) {
				t.Fatalf("failed startup persisted prompt/provider result: %v", err)
			}
			if _, statErr := os.Stat(sessionParent); statErr != nil {
				t.Fatalf("session-first startup did not prepare session tree: %v", statErr)
			}
		})
	}
}

func TestRunProductionIgnoresUnknownModelsJSONFields(t *testing.T) {
	tests := []struct {
		name    string
		model   string
		prepare func(*testing.T, string, string)
	}{
		{
			name:  "provider object",
			model: "openai/gpt-test",
			prepare: func(t *testing.T, agentDir, baseURL string) {
				writeModelsJSON(t, agentDir, baseURL, stringPointer("fixture-key"), map[string]any{
					"futureRequestOption": map[string]any{"Authorization": "must-not-become-a-header"},
				})
			},
		},
		{
			name:  "provider scalar",
			model: "openai/gpt-test",
			prepare: func(t *testing.T, agentDir, baseURL string) {
				writeModelsJSON(t, agentDir, baseURL, stringPointer("fixture-key"), map[string]any{
					"futureRequestOption": "must-not-enter-request",
				})
			},
		},
		{
			name:  "case-distinct model override compat",
			model: "openai/gpt-5.5",
			prepare: func(t *testing.T, agentDir, baseURL string) {
				content, err := json.Marshal(map[string]any{"providers": map[string]any{"openai": map[string]any{
					"baseUrl": baseURL, "apiKey": "fixture-key",
					"modelOverrides": map[string]any{"GPT-5.5": map[string]any{"compat": map[string]any{"token": "must-not-enter-request"}}},
				}}})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(agentDir, "models.json"), content, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workingDir, agentDir := t.TempDir(), t.TempDir()
			capture := &capturedProductionRequest{}
			server := newProductionTextServer(t, capture, "ok")
			defer server.Close()
			test.prepare(t, agentDir, server.URL+"/v1")
			var stdout, stderr bytes.Buffer
			code := app.RunProduction(context.Background(), productionTestConfig(workingDir, agentDir, nil), []string{
				"--model", test.model, "-p", "hello", "--session", filepath.Join(workingDir, "session.jsonl"),
			}, &stdout, &stderr)
			if code != app.ExitSuccess || stdout.String() != "ok\n" {
				t.Fatalf("RunProduction() = %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
			}
			request := capture.snapshot()
			capturedPayload, err := json.Marshal(request.payload)
			if err != nil {
				t.Fatal(err)
			}
			captured := string(capturedPayload)
			for _, forbidden := range []string{"must-not-become-a-header", "must-not-enter-request"} {
				if strings.Contains(captured, forbidden) {
					t.Fatalf("unknown models.json value entered payload: %s", captured)
				}
			}
			if request.authorization != "Bearer fixture-key" {
				t.Fatalf("authorization = %q", request.authorization)
			}
		})
	}
}

func TestRunProductionNormalizesInvalidUTF8InGlobalSystemPrompt(t *testing.T) {
	workingDir := t.TempDir()
	agentDir := t.TempDir()
	// Node's UTF-8 file reads replace malformed byte sequences. The Go resource
	// loader must preserve that behavior instead of rejecting the prompt before
	// the provider request.
	if err := os.WriteFile(filepath.Join(agentDir, "SYSTEM.md"), []byte{0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	capture := &capturedProductionRequest{}
	server := newProductionTextServer(t, capture, "ok")
	defer server.Close()
	writeModelsJSON(t, agentDir, server.URL+"/v1", stringPointer("fixture-key"), nil)
	sessionParent := filepath.Join(workingDir, "session-parent")
	config := productionTestConfig(workingDir, agentDir, nil)
	var stdout, stderr bytes.Buffer
	code := app.RunProduction(context.Background(), config, []string{
		"--model", "openai/gpt-test", "-p", "hello", "--session", filepath.Join(sessionParent, "session.jsonl"),
	}, &stdout, &stderr)
	if code != app.ExitSuccess || stdout.String() != "ok\n" || stderr.String() != customModelWarning("gpt-test") {
		t.Fatalf("RunProduction() = %d, %q, %q", code, stdout.String(), stderr.String())
	}
	input := capture.snapshot().payload["input"].([]any)
	system := input[0].(map[string]any)["content"].(string)
	if !strings.Contains(system, "�") {
		t.Fatalf("normalized system prompt = %q", system)
	}
	if _, err := os.Stat(sessionParent); err != nil {
		t.Fatalf("session startup did not prepare session tree: %v", err)
	}
}

func TestRunProductionUsesOnlyExplicitlyTrustedProjectPrompt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("persistent project trust is deliberately fail-closed on Windows")
	}
	workingDir := t.TempDir()
	agentDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workingDir, ".pi-go"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workingDir, ".pi-go", "SYSTEM.md"), []byte("project system prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	resources, err := resource.New(resource.Config{CWD: workingDir, AgentDir: agentDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := resources.Trust().Set(context.Background(), workingDir, true); err != nil {
		t.Fatal(err)
	}
	capture := &capturedProductionRequest{}
	server := newProductionTextServer(t, capture, "ok")
	defer server.Close()
	writeModelsJSON(t, agentDir, server.URL+"/v1", stringPointer("fixture-key"), nil)
	var stdout, stderr bytes.Buffer
	code := app.RunProduction(context.Background(), productionTestConfig(workingDir, agentDir, nil), []string{
		"--model", "openai/gpt-test", "-p", "hello", "--session", filepath.Join(workingDir, "result.jsonl"),
	}, &stdout, &stderr)
	if code != app.ExitSuccess || stderr.String() != customModelWarning("gpt-test") {
		t.Fatalf("RunProduction() = %d, stderr %q", code, stderr.String())
	}
	input := capture.snapshot().payload["input"].([]any)
	system := input[0].(map[string]any)["content"].(string)
	if !strings.Contains(system, "project system prompt") || !strings.Contains(system, "Current working directory:") {
		t.Fatalf("assembled system prompt = %q", system)
	}
}

func TestRunProductionRejectsFutureTrustValueBeforeProviderAccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("existing persistent trust is deliberately fail-closed on Windows")
	}
	root := t.TempDir()
	workingDir := filepath.Join(root, "parent", "project")
	agentDir := filepath.Join(root, "agent")
	if err := os.MkdirAll(filepath.Join(workingDir, ".pi-go"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workingDir, ".pi-go", "SYSTEM.md"), []byte("project must stay unauthorized"), 0o600); err != nil {
		t.Fatal(err)
	}
	trust := fmt.Sprintf("{\n  %q: true,\n  %q: {\"trusted\": false}\n}\n", filepath.Dir(workingDir), workingDir)
	if err := os.WriteFile(filepath.Join(agentDir, "trust.json"), []byte(trust), 0o600); err != nil {
		t.Fatal(err)
	}
	doer := &countingProductionDoer{}
	writeModelsJSON(t, agentDir, "https://example.invalid/v1", stringPointer("fixture-key"), nil)
	config := productionTestConfig(workingDir, agentDir, nil)
	config.OpenAIHTTPClient = doer
	var stdout, stderr bytes.Buffer
	code := app.RunProduction(context.Background(), config, []string{
		"--model", "openai/gpt-test", "-p", "ordinary prompt", "--session", filepath.Join(workingDir, "result.jsonl"),
	}, &stdout, &stderr)
	if code != app.ExitFailure || stdout.Len() != 0 || !strings.Contains(stderr.String(), "must be true, false, or null") {
		t.Fatalf("RunProduction() = %d, stderr %q", code, stderr.String())
	}
	if doer.calls.Load() != 0 {
		t.Fatalf("invalid trust value reached provider")
	}
}

func TestRunProductionExpandsAdmittedPromptTemplateForRequest(t *testing.T) {
	workingDir := t.TempDir()
	agentDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(agentDir, "prompts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompts", "review.md"), []byte("review $1 ${2:-all}"), 0o600); err != nil {
		t.Fatal(err)
	}
	capture := &capturedProductionRequest{}
	server := newProductionTextServer(t, capture, "ok")
	defer server.Close()
	writeModelsJSON(t, agentDir, server.URL+"/v1", stringPointer("fixture-key"), nil)
	path := filepath.Join(workingDir, "expanded.jsonl")
	var stdout, stderr bytes.Buffer
	code := app.RunProduction(context.Background(), productionTestConfig(workingDir, agentDir, nil), []string{
		"--model", "openai/gpt-test", "-p", "/review file.go", "--session", path,
	}, &stdout, &stderr)
	if code != app.ExitSuccess || stderr.String() != customModelWarning("gpt-test") {
		t.Fatalf("RunProduction() = %d, stderr %q", code, stderr.String())
	}
	input := capture.snapshot().payload["input"].([]any)
	user := input[1].(map[string]any)
	content, ok := user["content"].([]any)
	if !ok || len(content) != 1 || content[0].(map[string]any)["text"] != "review file.go all" {
		t.Fatalf("provider user prompt = %#v", user)
	}
	transcript, err := session.Open(path, session.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer transcript.Close()
	messages := transcript.Context().Messages()
	if len(messages) == 0 {
		t.Fatalf("durable expanded prompt = %#v", messages)
	}
	stored, ok := messages[0].(llm.UserContentMessage)
	if !ok || len(stored.Content()) != 1 || stored.Content()[0].(llm.TextBlock).Text() != "review file.go all" {
		t.Fatalf("durable expanded prompt = %#v", messages)
	}
}

func TestRunProductionExpandsSkillInsideAgentSessionAndAdvertisesIt(t *testing.T) {
	workingDir := t.TempDir()
	agentDir := t.TempDir()
	skillPath := filepath.Join(agentDir, "skills", "review", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillPath, []byte("---\nname: review\ndescription: Review a change\n---\nInspect the requested change."), 0o600); err != nil {
		t.Fatal(err)
	}
	capture := &capturedProductionRequest{}
	server := newProductionTextServer(t, capture, "ok")
	defer server.Close()
	writeModelsJSON(t, agentDir, server.URL+"/v1", stringPointer("fixture-key"), nil)
	path := filepath.Join(workingDir, "skill.jsonl")
	var stdout, stderr bytes.Buffer
	code := app.RunProduction(context.Background(), productionTestConfig(workingDir, agentDir, nil), []string{
		"--model", "openai/gpt-test", "-p", "/skill:review file.go", "--session", path,
	}, &stdout, &stderr)
	if code != app.ExitSuccess || stderr.String() != customModelWarning("gpt-test") {
		t.Fatalf("RunProduction() = %d, stderr %q", code, stderr.String())
	}
	input := capture.snapshot().payload["input"].([]any)
	system := input[0].(map[string]any)["content"].(string)
	if !strings.Contains(system, `<available_skills>`) || !strings.Contains(system, "Review a change") {
		t.Fatalf("system prompt skills = %q", system)
	}
	content := input[1].(map[string]any)["content"].([]any)
	expanded := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(expanded, `<skill name="review" location="`+skillPath+`">`) ||
		!strings.Contains(expanded, "Inspect the requested change.\n</skill>\n\nfile.go") {
		t.Fatalf("provider skill prompt = %q", expanded)
	}
	transcript, err := session.Open(path, session.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer transcript.Close()
	messages := transcript.Context().Messages()
	stored, ok := messages[0].(llm.UserContentMessage)
	if !ok || stored.Content()[0].(llm.TextBlock).Text() != expanded {
		t.Fatalf("durable skill prompt = %#v", messages)
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
	if exitCode != app.ExitSuccess || stdout.String() != "resumed\n" || stderr.String() != customModelWarning("gpt-resume") {
		t.Fatalf("RunProduction() = code %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
	}

	transcript, err := session.Open(sessionPath, session.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer transcript.Close()
	entries := transcript.Entries()
	if len(entries) != 4 {
		t.Fatalf("resumed entries = %d, want 4", len(entries))
	}
	for index, entry := range entries {
		want := productionTestTime.Add(time.Duration(index+1) * time.Millisecond)
		if entry.Timestamp() != want {
			t.Fatalf("resumed entry %d timestamp = %v, want %v", index, entry.Timestamp(), want)
		}
	}
}

func TestRunProductionPreservesInvalidExplicitSessionBeforeProviderCall(t *testing.T) {
	workingDir := t.TempDir()
	agentDir := t.TempDir()
	writeModelsJSON(t, agentDir, "https://fixture.invalid/v1", stringPointer("fixture-key"), nil)
	sessionPath := filepath.Join(workingDir, "invalid.jsonl")
	original := []byte(`{"type":"event","data":"not a session"}` + "\n")
	if err := os.WriteFile(sessionPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	exitCode := app.RunProduction(context.Background(), productionTestConfig(workingDir, agentDir, nil), []string{
		"--model", "openai/gpt-invalid", "--session", sessionPath, "-p", "must not call provider",
	}, &stdout, &stderr)
	if exitCode != app.ExitFailure || stdout.Len() != 0 || !strings.Contains(stderr.String(), "open session "+sessionPath) {
		t.Fatalf("RunProduction = code %d stdout %q stderr %q", exitCode, stdout.String(), stderr.String())
	}
	after, err := os.ReadFile(sessionPath)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatalf("production changed invalid session: %v / %q", err, after)
	}
}

func TestRunProductionOpensLegacySessionIntoCurrentContext(t *testing.T) {
	workingDir := t.TempDir()
	agentDir := t.TempDir()
	capture := &capturedProductionRequest{}
	server := newProductionTextServer(t, capture, "legacy production answer")
	defer server.Close()
	writeModelsJSON(t, agentDir, server.URL+"/v1", stringPointer("fixture-key"), nil)
	sessionPath := filepath.Join(workingDir, "legacy-production.jsonl")
	legacy := []byte(fmt.Sprintf(
		`{"type":"session","version":2,"id":"legacy-production","timestamp":"2026-08-01T00:00:00.000Z","cwd":%q}`+"\n"+
			`{"type":"message","id":"legacy-root","parentId":null,"timestamp":"2026-08-01T00:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"legacy production context"}],"timestamp":1785542401000}}`+"\n",
		workingDir,
	))
	if err := os.WriteFile(sessionPath, legacy, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	exitCode := app.RunProduction(
		context.Background(),
		productionTestConfig(workingDir, agentDir, nil),
		[]string{"--model", "openai/gpt-legacy", "--session", sessionPath, "-p", "resume migrated context"},
		&stdout,
		&stderr,
	)
	if exitCode != app.ExitSuccess || stdout.String() != "legacy production answer\n" || stderr.String() != customModelWarning("gpt-legacy") {
		t.Fatalf("RunProduction legacy = code %d stdout %q stderr %q", exitCode, stdout.String(), stderr.String())
	}
	payload, err := json.Marshal(capture.snapshot().payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(payload), "legacy production context") != 1 || strings.Count(string(payload), "resume migrated context") != 1 {
		t.Fatalf("production request lost migrated context: %s", payload)
	}
	data, err := os.ReadFile(sessionPath)
	if err != nil || !bytes.Contains(data, []byte(`"version":3`)) {
		t.Fatalf("production legacy publication = %v / %s", err, data)
	}
	transcript, err := session.Open(sessionPath, session.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer transcript.Close()
	if messages := transcript.BuildContext().Messages(); len(messages) != 3 {
		t.Fatalf("production reopened migrated context = %#v", messages)
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

func mustProductionTextBlock(t *testing.T, text string) llm.TextBlock {
	t.Helper()
	block, err := llm.NewTextBlock(text)
	if err != nil {
		t.Fatal(err)
	}
	return block
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

// simulatedOpenAIResponsesAdmission models the request rules that matter to
// the production tool workflow. The scheduler's execution mode remains local,
// matching pi's Responses wire request, and strict objects may not leave a
// declared property optional.
func simulatedOpenAIResponsesAdmission(payload map[string]any) error {
	if _, sent := payload["parallel_tool_calls"]; sent {
		return fmt.Errorf("parallel_tool_calls must be omitted, got %v", payload["parallel_tool_calls"])
	}
	tools, _ := payload["tools"].([]any)
	for index, raw := range tools {
		definition, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("tool %d must be an object", index)
		}
		strict, ok := definition["strict"].(bool)
		if !ok {
			return fmt.Errorf("tool %d strict must be a boolean", index)
		}
		if !strict {
			continue
		}
		parameters, ok := definition["parameters"].(map[string]any)
		if !ok || parameters["additionalProperties"] != false {
			return fmt.Errorf("strict tool %d must set additionalProperties false", index)
		}
		properties, ok := parameters["properties"].(map[string]any)
		if !ok {
			return fmt.Errorf("strict tool %d properties must be an object", index)
		}
		requiredValues, ok := parameters["required"].([]any)
		if !ok {
			return fmt.Errorf("strict tool %d must require every property", index)
		}
		required := make(map[string]struct{}, len(requiredValues))
		for _, value := range requiredValues {
			name, ok := value.(string)
			if !ok {
				return fmt.Errorf("strict tool %d required names must be strings", index)
			}
			required[name] = struct{}{}
		}
		for name := range properties {
			if _, present := required[name]; !present {
				return fmt.Errorf("strict tool %d leaves property %q optional", index, name)
			}
		}
	}
	return nil
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

func customModelWarning(modelID string) string {
	return `Warning: Model "` + modelID + `" not found for provider "openai". Using custom model id.` + "\n"
}
