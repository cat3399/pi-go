package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
	"github.com/cat3399/pi-go/internal/session"
)

type testBackend struct {
	mu       sync.Mutex
	observer application.SessionObserver
	dispatch func(context.Context, application.Command) (application.CommandResult, error)
	disposed bool
}

func (b *testBackend) Dispatch(ctx context.Context, command application.Command) (application.CommandResult, error) {
	return b.dispatch(ctx, command)
}

func (b *testBackend) Subscribe(observer application.SessionObserver) func() {
	b.mu.Lock()
	b.observer = observer
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		b.observer = nil
		b.mu.Unlock()
	}
}

func (b *testBackend) emit(event application.Event) {
	b.mu.Lock()
	observer := b.observer
	b.mu.Unlock()
	if observer != nil {
		observer(context.Background(), event)
	}
}

func (b *testBackend) Dispose(context.Context) error {
	b.mu.Lock()
	b.disposed = true
	b.mu.Unlock()
	return nil
}

func TestServerCorrelatesPromptOperationAndAgentEvents(t *testing.T) {
	backend := &testBackend{}
	backend.dispatch = func(_ context.Context, command application.Command) (application.CommandResult, error) {
		prompt := command.(application.PromptCommand)
		if prompt.Source != agent.InputRPC {
			t.Errorf("prompt source = %q", prompt.Source)
		}
		started := application.PromptStartedResult{OperationID: 7}
		backend.emit(application.Event{Sequence: 1, SessionID: "session", Value: application.AgentSessionEvent{Event: agent.AgentStartEvent{RunID: 1}}})
		return started, nil
	}
	var output bytes.Buffer
	server, err := NewServer(backend, bytes.NewBufferString("{\"id\":\"p1\",\"type\":\"prompt\",\"message\":\"hello\"}\n"), &output)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Serve(context.Background()); err != nil {
		t.Fatal(err)
	}
	records := decodeOutputRecords(t, output.Bytes())
	if len(records) != 2 {
		t.Fatalf("records = %#v", records)
	}
	var response, event map[string]any
	for _, record := range records {
		if record["type"] == "response" {
			response = record
		} else if record["type"] == "agent_start" {
			event = record
		}
	}
	data, _ := response["data"].(map[string]any)
	if response["command"] != "prompt" || response["success"] != true || data["operationId"] != float64(7) {
		t.Fatalf("prompt response = %#v", response)
	}
	if event == nil {
		t.Fatalf("Agent event missing from %#v", records)
	}
}

func TestServerStreamsRealAgentSessionProtocol(t *testing.T) {
	timestamp := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{ChunkRunes: 2, Clock: func() time.Time { return timestamp }})
	if err != nil {
		t.Fatal(err)
	}
	model, err := provider.NewModel(provider.ModelSpec{
		Provider: "scripted", API: "scripted", ID: "rpc-model", Name: "RPC Model",
		Input: []provider.InputKind{provider.InputText}, ContextWindow: 16_000, MaxTokens: 1_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	text, err := llm.NewTextBlock("answer")
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := llm.NewAssistantTextMessage(
		[]llm.TextBlock{text}, llm.FinishStop, llm.Usage{}, timestamp,
		llm.AssistantProvenance{Provider: "scripted", API: "scripted", Model: "rpc-model"},
	)
	if err != nil {
		t.Fatal(err)
	}
	step, err := provider.FixedResponseStep(terminal)
	if err != nil {
		t.Fatal(err)
	}
	if err := implementation.SetResponses([]provider.ScriptStep{step}); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	manager, err := session.InMemorySessionManager(cwd, session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	factory := func(_ context.Context, options agentruntime.CreateOptions) (agentruntime.CreateResult, error) {
		productSession, err := agent.NewSession(agent.SessionConfig{
			Provider: implementation, SessionManager: options.SessionManager, Model: model,
			AllModels: []provider.Model{model}, ThinkingLevel: provider.ThinkingOff,
			Now: func() time.Time { return timestamp }, SettlementTimeout: time.Second,
		})
		if err != nil {
			return agentruntime.CreateResult{}, err
		}
		return agentruntime.CreateResult{
			Session:  productSession,
			Services: &agentruntime.Services{CWD: cwd, AgentDir: cwd, Provider: implementation},
		}, nil
	}
	runtime, err := agentruntime.Create(context.Background(), factory, agentruntime.InitialOptions{CWD: cwd, AgentDir: cwd, SessionManager: manager})
	if err != nil {
		t.Fatal(err)
	}
	productSession, err := application.NewApplicationSession(context.Background(), runtime)
	if err != nil {
		t.Fatal(err)
	}
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	server, err := NewServer(productSession, inputReader, outputWriter)
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(context.Background())
		_ = outputWriter.Close()
	}()
	_, _ = io.WriteString(inputWriter, "{\"id\":\"real\",\"type\":\"prompt\",\"message\":\"hello\"}\n")

	scanner := bufio.NewScanner(outputReader)
	var records []map[string]any
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
		if record["type"] == "operation" && record["status"] == "completed" {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	_ = inputWriter.Close()
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
	responseIndex, agentStartIndex, updateIndex, operationIndex := -1, -1, -1, -1
	for index, record := range records {
		switch record["type"] {
		case "response":
			if record["command"] == "prompt" {
				responseIndex = index
			}
		case "agent_start":
			agentStartIndex = index
		case "message_update":
			updateIndex = index
		case "operation":
			if record["command"] == "prompt" && record["status"] == "completed" {
				operationIndex = index
			}
		}
	}
	if responseIndex < 0 || agentStartIndex < 0 || updateIndex <= agentStartIndex || operationIndex <= updateIndex {
		t.Fatalf("real prompt ordering = %#v", records)
	}
	message, ok := records[updateIndex]["message"].(map[string]any)
	if !ok || message["role"] != "assistant" || message["stopReason"] != "pending" {
		t.Fatalf("streaming AgentMessage = %#v", records[updateIndex]["message"])
	}
	assistantEvent, ok := records[updateIndex]["assistantMessageEvent"].(map[string]any)
	if !ok || assistantEvent["partial"] == nil {
		t.Fatalf("assistantMessageEvent = %#v", records[updateIndex]["assistantMessageEvent"])
	}
}

func TestServerDispatchesAbortWhileBashCommandIsBlocked(t *testing.T) {
	bashStarted := make(chan struct{})
	aborted := make(chan struct{})
	bashDone := make(chan struct{})
	backend := &testBackend{}
	backend.dispatch = func(_ context.Context, command application.Command) (application.CommandResult, error) {
		switch command.(type) {
		case application.BashCommand:
			close(bashStarted)
			<-aborted
			close(bashDone)
			return application.BashResult{Result: agent.BashResult{Output: "partial", Cancelled: true}}, nil
		case application.AbortBashCommand:
			close(aborted)
			return application.AbortBashResult{}, nil
		default:
			t.Fatalf("unexpected command %T", command)
			return nil, nil
		}
	}
	inputReader, inputWriter := io.Pipe()
	var output bytes.Buffer
	server, err := NewServer(backend, inputReader, &output)
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(context.Background()) }()

	_, _ = io.WriteString(inputWriter, "{\"id\":\"bash\",\"type\":\"bash\",\"command\":\"sleep\"}\n")
	select {
	case <-bashStarted:
	case <-time.After(time.Second):
		t.Fatal("bash command did not start")
	}
	_, _ = io.WriteString(inputWriter, "{\"id\":\"abort\",\"type\":\"abort_bash\"}\n")
	select {
	case <-bashDone:
	case <-time.After(time.Second):
		t.Fatal("abort_bash was serialized behind bash")
	}
	_ = inputWriter.Close()
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
	records := decodeOutputRecords(t, output.Bytes())
	commands := make(map[string]bool)
	for _, record := range records {
		if command, ok := record["command"].(string); ok && record["success"] == true {
			commands[command] = true
		}
	}
	if !reflect.DeepEqual(commands, map[string]bool{"abort_bash": true, "bash": true}) {
		t.Fatalf("responses = %#v", records)
	}
}

func TestServerReturnsCorrelatedDecodeErrorAndDisposes(t *testing.T) {
	backend := &testBackend{dispatch: func(context.Context, application.Command) (application.CommandResult, error) {
		t.Fatal("invalid command reached backend")
		return nil, nil
	}}
	var output bytes.Buffer
	server, err := NewServer(backend, bytes.NewBufferString("{\"id\":\"bad\",\"type\":\"set_auto_retry\"}\n"), &output)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Serve(context.Background()); err != nil {
		t.Fatal(err)
	}
	records := decodeOutputRecords(t, output.Bytes())
	if len(records) != 1 || records[0]["id"] != "bad" || records[0]["command"] != "set_auto_retry" || records[0]["success"] != false {
		t.Fatalf("error response = %#v", records)
	}
	backend.mu.Lock()
	disposed := backend.disposed
	backend.mu.Unlock()
	if !disposed {
		t.Fatal("backend was not disposed at EOF")
	}
}

func TestDecodePromptPreservesRichImage(t *testing.T) {
	decoded, err := decodeCommand([]byte(`{"type":"prompt","message":"look","streamingBehavior":"followUp","images":[{"type":"image","data":"AQID","mimeType":"image/png"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	prompt := decoded.command.(application.PromptCommand)
	if prompt.StreamingBehavior != agent.StreamingFollowUp || len(prompt.Images) != 1 || !reflect.DeepEqual(prompt.Images[0].Data(), []byte{1, 2, 3}) {
		t.Fatalf("decoded prompt = %#v", prompt)
	}
}

func decodeOutputRecords(t *testing.T, value []byte) []map[string]any {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(value), []byte{'\n'})
	records := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("decode output %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}
