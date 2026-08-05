package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

var testAssistantProvenance = AssistantProvenance{
	API:      "scripted",
	Provider: "scripted",
	Model:    "scripted-1",
	Cost:     ZeroUsageCost(),
}

func testLLMAssistantProvenance() llm.AssistantProvenance {
	return llm.AssistantProvenance{Provider: testAssistantProvenance.Provider, API: testAssistantProvenance.API, Model: testAssistantProvenance.Model}
}

func newAssistantTextMessage(content []llm.TextBlock, finish llm.FinishReason, usage llm.Usage, timestamp time.Time) (llm.AssistantTextMessage, error) {
	return llm.NewAssistantTextMessage(content, finish, usage, timestamp, testLLMAssistantProvenance())
}

func newAssistantToolUseMessage(content []llm.AssistantBlock, usage llm.Usage, timestamp time.Time) (llm.AssistantToolUseMessage, error) {
	return llm.NewAssistantToolUseMessage(content, usage, timestamp, testLLMAssistantProvenance())
}

func newAssistantRichMessage(content []llm.AssistantBlock, finish llm.FinishReason, usage llm.Usage, timestamp time.Time) (llm.AssistantRichMessage, error) {
	return llm.NewAssistantRichMessage(content, finish, usage, timestamp, testLLMAssistantProvenance())
}

func newAssistantFailureMessage(content []llm.TextBlock, finish llm.FinishReason, message string, usage llm.Usage, timestamp time.Time) (llm.AssistantFailureMessage, error) {
	return llm.NewAssistantFailureMessage(content, finish, message, usage, timestamp, testLLMAssistantProvenance())
}

func newAssistantFailureMessageWithFailure(content []llm.TextBlock, finish llm.FinishReason, failure llm.Failure, usage llm.Usage, timestamp time.Time) (llm.AssistantFailureMessage, error) {
	return llm.NewAssistantFailureMessageWithFailure(content, finish, failure, usage, timestamp, testLLMAssistantProvenance())
}

var testResponsesAssistantProvenance = AssistantProvenance{
	API:      "openai-responses",
	Provider: "openai",
	Model:    "gpt-test",
	Cost:     ZeroUsageCost(),
}

func TestRichContentSessionRoundTripCopiesImageAndReasoningReplay(t *testing.T) {
	directory := t.TempDir()
	transcript, err := Create(filepath.Join(directory, "rich.jsonl"), CreateOptions{ID: "rich", WorkingDir: directory, Now: func() time.Time { return time.Date(2026, time.August, 2, 0, 0, 0, 0, time.UTC) }, NewEntryID: sequenceIDs("u", "a", "r")})
	if err != nil {
		t.Fatal(err)
	}
	imageBytes, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	wantImage := bytes.Clone(imageBytes)
	image, err := llm.NewImageDataBlock("image/png", imageBytes)
	if err != nil {
		t.Fatal(err)
	}
	imageBytes[0] = 9
	text := mustTextBlock(t, "look")
	user, err := llm.NewUserContentMessage([]llm.UserContentBlock{text, image}, time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	reasoningSignature := `{"type":"reasoning","id":"rs_1","encrypted_content":"cipher"}`
	thinking, err := llm.NewThinkingBlockWithSignature("plan", reasoningSignature, false)
	if err != nil {
		t.Fatal(err)
	}
	textSignature := `{"v":1,"id":"msg_1","phase":"final_answer"}`
	answer, err := llm.NewTextBlockWithSignature("done", textSignature)
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := llm.NewAssistantRichMessageWithMetadata(
		[]llm.AssistantBlock{thinking, answer},
		llm.FinishStop,
		llm.Usage{},
		time.UnixMilli(2),
		llm.AssistantProvenance{Provider: testResponsesAssistantProvenance.Provider, API: testResponsesAssistantProvenance.API, Model: testResponsesAssistantProvenance.Model},
		&llm.AssistantResponseMetadata{ResponseID: "resp_1", ResponseModel: "resolved-model", RawStopReason: "completed"},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := llm.NewToolResultContentMessage("call_1", "bash", []llm.ToolResultContentBlock{image}, false, time.UnixMilli(3))
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []llm.ConversationMessage{user, assistant, result} {
		if _, err := transcript.Append(context.Background(), message, AppendOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(filepath.Join(directory, "rich.jsonl"), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	messages := reopened.Context().Messages()
	if len(messages) != 3 {
		t.Fatalf("messages=%#v", messages)
	}
	storedUser := messages[0].(llm.UserContentMessage).Content()
	if got := storedUser[1].(llm.ImageBlock).Data(); !bytes.Equal(got, wantImage) {
		t.Fatalf("image=%v", got)
	}
	storedAssistant := messages[1].(llm.AssistantRichMessage).Blocks()
	replay, ok := storedAssistant[0].(llm.ThinkingBlock).ThinkingSignature()
	if !ok || replay != reasoningSignature {
		t.Fatalf("reasoning=%#v", storedAssistant[0])
	}
	signature, ok := storedAssistant[1].(llm.TextBlock).TextSignature()
	if !ok || signature != textSignature {
		t.Fatalf("text=%#v", storedAssistant[1])
	}
	provenance := messages[1].(llm.AssistantRichMessage).AssistantProvenance()
	if provenance != (llm.AssistantProvenance{Provider: testResponsesAssistantProvenance.Provider, API: testResponsesAssistantProvenance.API, Model: testResponsesAssistantProvenance.Model}) {
		t.Fatalf("assistant provenance = %#v", provenance)
	}
	if response, ok := messages[1].(llm.AssistantRichMessage).ResponseMetadata(); !ok || response != (llm.AssistantResponseMetadata{ResponseID: "resp_1", ResponseModel: "resolved-model", RawStopReason: "completed"}) {
		t.Fatalf("assistant response metadata = (%#v, %t)", response, ok)
	}
	raw, err := os.ReadFile(filepath.Join(directory, "rich.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`\"v\":1,\"id\":\"msg_1\",\"phase\":\"final_answer\"`)) ||
		!bytes.Contains(raw, []byte(`\"type\":\"reasoning\",\"id\":\"rs_1\",\"encrypted_content\":\"cipher\"`)) ||
		!bytes.Contains(raw, []byte(`"responseModel":"resolved-model"`)) {
		t.Fatalf("session did not use typed v3 replay envelopes: %s", raw)
	}
}

func TestUserContentWireShapeUsesArrayAndRoundTripsConcreteType(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "user-shapes.jsonl")
	transcript, err := Create(path, CreateOptions{
		ID: "user-shapes", WorkingDir: directory,
		Now:        func() time.Time { return time.Date(2026, time.August, 2, 0, 0, 0, 0, time.UTC) },
		NewEntryID: sequenceIDs("content", "text"),
	})
	if err != nil {
		t.Fatal(err)
	}
	block := mustTextBlock(t, "content prompt")
	content, err := llm.NewUserContentMessage([]llm.UserContentBlock{block}, time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.Append(context.Background(), content, AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	text, err := llm.NewUserTextMessage("legacy text", time.UnixMilli(2))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.Append(context.Background(), text, AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"content":[{"type":"text","text":"content prompt"}]`)) ||
		!bytes.Contains(data, []byte(`"content":"legacy text"`)) || bytes.Contains(data, []byte(`"contentType"`)) {
		t.Fatalf("user wire shapes = %s", data)
	}
	reopened, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	messages := reopened.Context().Messages()
	if len(messages) != 2 {
		t.Fatalf("messages = %#v", messages)
	}
	if _, ok := messages[0].(llm.UserContentMessage); !ok {
		t.Fatalf("array user decoded as %T", messages[0])
	}
	if _, ok := messages[1].(llm.UserTextMessage); !ok {
		t.Fatalf("string user decoded as %T", messages[1])
	}
}

func TestAppendDerivesAssistantProvenanceFromMessage(t *testing.T) {
	directory := t.TempDir()
	transcript, err := Create(filepath.Join(directory, "provenance.jsonl"), CreateOptions{
		ID:         "provenance",
		WorkingDir: directory,
		NewEntryID: sequenceIDs("assistant"),
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := llm.NewAssistantTextMessageWithMetadata(
		[]llm.TextBlock{mustTextBlock(t, "answer")},
		llm.FinishStop,
		llm.Usage{},
		time.UnixMilli(1),
		llm.AssistantProvenance{Provider: "openai", API: "openai-responses", Model: "gpt-test"},
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := transcript.Append(context.Background(), message, AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := entry.AssistantProvenance()
	want := AssistantProvenance{Provider: "openai", API: "openai-responses", Model: "gpt-test", Cost: ZeroUsageCost()}
	if !ok || got != want {
		t.Fatalf("AssistantProvenance() = (%#v, %t), want %#v", got, ok, want)
	}
}

func TestAppendPreservesReplayMetadataOnFailedAssistant(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name   string
		finish llm.FinishReason
	}{
		{name: "error", finish: llm.FinishError},
		{name: "aborted", finish: llm.FinishAborted},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			directory := t.TempDir()
			transcript, err := Create(filepath.Join(directory, "failure.jsonl"), CreateOptions{
				ID:         "failure",
				WorkingDir: directory,
				NewEntryID: sequenceIDs("assistant"),
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = transcript.Close() })
			partial, err := llm.NewTextBlockWithSignature("partial", `{"v":1,"id":"msg_partial","phase":"commentary"}`)
			if err != nil {
				t.Fatal(err)
			}
			failure, err := newAssistantFailureMessage([]llm.TextBlock{partial}, tt.finish, "request failed", llm.Usage{}, time.UnixMilli(1))
			if err != nil {
				t.Fatal(err)
			}
			entry, err := transcript.Append(context.Background(), failure, AppendOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(entry.RawJSON(), []byte("textSignature")) {
				t.Fatalf("failed assistant lost replay metadata: %s", entry.RawJSON())
			}
			stored := transcript.Context().Messages()[0].(llm.AssistantFailureMessage)
			if signature, ok := stored.Blocks()[0].(llm.TextBlock).TextSignature(); !ok || signature != `{"v":1,"id":"msg_partial","phase":"commentary"}` {
				t.Fatalf("failed assistant projected signature = (%q, %t)", signature, ok)
			}
			if diagnostics := transcript.Context().Diagnostics(); len(diagnostics) != 0 {
				t.Fatalf("locally encoded failure diagnostics = %#v", diagnostics)
			}
		})
	}
}

func TestFailedAssistantMetadataAndDiagnosticsSurviveReopen(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "failure-metadata.jsonl")
	transcript, err := Create(path, CreateOptions{
		ID:         "failure-metadata",
		WorkingDir: directory,
		NewEntryID: sequenceIDs("assistant"),
	})
	if err != nil {
		t.Fatal(err)
	}

	at := time.UnixMilli(1_785_542_401_123).UTC()
	diagnostic, err := llm.NewAssistantDiagnostic(llm.AssistantDiagnosticSpec{
		Type:      "provider-recovery",
		Timestamp: at,
		Error: &llm.AssistantDiagnosticError{
			Name: "UpstreamError", Message: "redacted upstream failure",
			Stack: "redacted stack", Code: json.RawMessage(`"E_RETRY"`),
		},
		Details: json.RawMessage(`{"attempt":2,"recovered":false}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	failure, err := llm.NewFailure("request failed", errors.New("local cause must not be serialized"))
	if err != nil {
		t.Fatal(err)
	}
	response := llm.AssistantResponseMetadata{
		ResponseID: "resp_failure", ResponseModel: "upstream-model", RawStopReason: "upstream_error",
	}
	thinking, err := llm.NewThinkingBlock("completed reasoning before failure")
	if err != nil {
		t.Fatal(err)
	}
	call, err := llm.NewToolCallBlock("call-before-failure", "inspect", []byte(`{"path":"README.md"}`))
	if err != nil {
		t.Fatal(err)
	}
	message, err := llm.NewAssistantFailureMessageWithBlocksAndMetadata(
		[]llm.AssistantBlock{thinking, call}, llm.FinishError, failure, llm.Usage{}, at,
		llm.AssistantProvenance{Provider: "fixture-provider", API: "fixture-api", Model: "fixture-model"},
		&response, []llm.AssistantDiagnostic{diagnostic},
	)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := transcript.Append(context.Background(), message, AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range [][]byte{
		[]byte(`"provider":"fixture-provider"`), []byte(`"api":"fixture-api"`),
		[]byte(`"model":"fixture-model"`), []byte(`"responseId":"resp_failure"`),
		[]byte(`"responseModel":"upstream-model"`), []byte(`"rawStopReason":"upstream_error"`),
		[]byte(`"diagnostics":[`),
	} {
		if !bytes.Contains(entry.RawJSON(), field) {
			t.Fatalf("encoded failure is missing %s: %s", field, entry.RawJSON())
		}
	}
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, ok := reopened.BuildContext().Messages()[0].(llm.AssistantFailureMessage)
	if !ok {
		t.Fatalf("reopened failure = %T", reopened.BuildContext().Messages()[0])
	}
	if provenance := got.AssistantProvenance(); provenance != (llm.AssistantProvenance{Provider: "fixture-provider", API: "fixture-api", Model: "fixture-model"}) {
		t.Fatalf("reopened provenance = %#v", provenance)
	}
	if metadata, ok := got.ResponseMetadata(); !ok || metadata != response {
		t.Fatalf("reopened response metadata = (%#v, %t)", metadata, ok)
	}
	diagnostics := got.Diagnostics()
	if len(diagnostics) != 1 || diagnostics[0].Type() != "provider-recovery" || !diagnostics[0].Timestamp().Equal(at) || string(diagnostics[0].Details()) != `{"attempt":2,"recovered":false}` {
		t.Fatalf("reopened diagnostics = %#v", diagnostics)
	}
	errorInfo, ok := diagnostics[0].ErrorInfo()
	if !ok || errorInfo.Name != "UpstreamError" || errorInfo.Message != "redacted upstream failure" || errorInfo.Stack != "redacted stack" || string(errorInfo.Code) != `"E_RETRY"` {
		t.Fatalf("reopened diagnostic error = (%#v, %t)", errorInfo, ok)
	}
	blocks := got.Blocks()
	if len(blocks) != 2 || blocks[0].(llm.ThinkingBlock).Thinking() != "completed reasoning before failure" || blocks[1].(llm.ToolCallBlock).ID() != "call-before-failure" {
		t.Fatalf("reopened failure blocks = %#v", blocks)
	}
	// Accessors must not expose the durable diagnostic's mutable JSON buffers.
	errorInfo.Code[0] = '0'
	details := diagnostics[0].Details()
	details[0] = '['
	again := got.Diagnostics()[0]
	againError, _ := again.ErrorInfo()
	if string(againError.Code) != `"E_RETRY"` || string(again.Details()) != `{"attempt":2,"recovered":false}` {
		t.Fatalf("diagnostic accessors shared storage: %#v / %s", againError, again.Details())
	}
}

func TestCreateAppendCloseAndReopenToolTurn(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "session.jsonl")
	clock := sequenceClock(time.Date(2026, time.August, 1, 1, 2, 3, 0, time.UTC))
	session, err := Create(path, CreateOptions{
		ID:         "session-1",
		WorkingDir: directory,
		Now:        clock,
		NewEntryID: sequenceIDs("entry-1", "entry-2", "entry-3", "entry-4"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := session.Path(); got != path {
		t.Fatalf("Path() = %q, want %q", got, path)
	}
	if got := session.Header(); got.ID() != "session-1" || got.WorkingDir() != directory || got.Version() != 3 {
		t.Fatalf("Header() = %#v", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
		t.Fatalf("session mode = %#o, want 0600", got)
	}

	user := mustUserMessage(t, "run it", clock())
	call := mustToolCall(t, "call-1", "bash", []byte(`{"command":"printf ok"}`))
	toolUse, err := newAssistantToolUseMessage(
		[]llm.AssistantBlock{mustTextBlock(t, "running"), call},
		mustUsage(t, 4, 2),
		clock(),
	)
	if err != nil {
		t.Fatal(err)
	}
	toolResult, err := llm.NewToolResultMessage(
		"call-1",
		"bash",
		[]llm.TextBlock{mustTextBlock(t, "ok")},
		false,
		clock(),
	)
	if err != nil {
		t.Fatal(err)
	}
	final, err := newAssistantTextMessage(
		[]llm.TextBlock{mustTextBlock(t, "done")},
		llm.FinishStop,
		mustUsage(t, 6, 1),
		clock(),
	)
	if err != nil {
		t.Fatal(err)
	}

	messages := []llm.ConversationMessage{user, toolUse, toolResult, final}
	for index, message := range messages {
		entry, err := session.Append(context.Background(), message, AppendOptions{})
		if err != nil {
			t.Fatalf("Append(%d) error = %v", index, err)
		}
		if entry.ID() != fmt.Sprintf("entry-%d", index+1) {
			t.Fatalf("entry %d id = %q", index, entry.ID())
		}
		parent, hasParent := entry.ParentID()
		if index == 0 && hasParent {
			t.Fatalf("root parent = %q", parent)
		}
		if index > 0 && (!hasParent || parent != fmt.Sprintf("entry-%d", index)) {
			t.Fatalf("entry %d parent = (%q, %t)", index, parent, hasParent)
		}
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if _, err := session.Append(context.Background(), user, AppendOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Append after close error = %v, want ErrClosed", err)
	}

	reopened, err := Open(path, OpenOptions{
		Now:        clock,
		NewEntryID: sequenceIDs("entry-5"),
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := reopened.Entries()
	if len(entries) != 4 {
		t.Fatalf("len(Entries()) = %d, want 4", len(entries))
	}
	for index, entry := range entries {
		parent, hasParent := entry.ParentID()
		if index == 0 && hasParent {
			t.Fatalf("reopened root parent = %q", parent)
		}
		if index > 0 && (!hasParent || parent != entries[index-1].ID()) {
			t.Fatalf("reopened entry %d parent = (%q, %t)", index, parent, hasParent)
		}
	}
	assertToolTurnContext(t, reopened.Context())
	identity, ok := reopened.Context().AssistantProvenance()
	if !ok || identity != testAssistantProvenance {
		t.Fatalf("AssistantProvenance() = (%#v, %t), want scripted provenance", identity, ok)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatal("session file does not end with newline")
	}
	lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	if len(lines) != 5 {
		t.Fatalf("physical record count = %d, want 5", len(lines))
	}
	var toolUseRecord struct {
		Message struct {
			Content []map[string]json.RawMessage `json:"content"`
			Usage   map[string]json.RawMessage   `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(lines[2], &toolUseRecord); err != nil {
		t.Fatal(err)
	}
	arguments := toolUseRecord.Message.Content[1]["arguments"]
	if len(arguments) == 0 || arguments[0] != '{' {
		t.Fatalf("tool arguments encoded as %s, want embedded object", arguments)
	}
	if _, ok := toolUseRecord.Message.Usage["cost"]; !ok {
		t.Fatal("assistant usage is missing coding-agent cost object")
	}
}

func TestCommittedStateMatchesReopenAndPreservesRawToolArguments(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "canonical.jsonl")
	headerTime := time.Date(2026, time.August, 1, 1, 2, 3, 123456789, time.FixedZone("east", 8*60*60))
	entryTime := headerTime.Add(time.Second + 864197532*time.Nanosecond)
	times := []time.Time{headerTime, entryTime}
	timeIndex := 0
	session, err := Create(path, CreateOptions{
		ID:         "canonical",
		WorkingDir: directory,
		Now: func() time.Time {
			value := times[timeIndex]
			timeIndex++
			return value
		},
		NewEntryID: sequenceIDs("entry-1"),
	})
	if err != nil {
		t.Fatal(err)
	}

	arguments := []byte(" {\n  \"html\": \"<tag>\", \"escaped\": \"\\u003ctag\\u003e\",\n  \"count\": 1.00e2\n} ")
	messageTime := time.Date(2026, time.August, 1, 5, 6, 7, 987654321, time.FixedZone("west", -7*60*60))
	message, err := newAssistantToolUseMessage(
		[]llm.AssistantBlock{mustToolCall(t, "call-1", "echo", arguments)},
		mustUsage(t, 2, 3),
		messageTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := session.Append(context.Background(), message, AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := session.Header().Timestamp(), canonicalTime(headerTime); !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("header timestamp = %v, want canonical %v", got, want)
	}
	if got, want := entry.Timestamp(), canonicalTime(entryTime); !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("entry timestamp = %v, want canonical %v", got, want)
	}
	immediate := session.Context().Messages()[0].(llm.AssistantToolUseMessage)
	if got, want := immediate.Timestamp(), time.UnixMilli(messageTime.UnixMilli()).UTC(); !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("message timestamp = %v, want canonical %v", got, want)
	}
	if got := immediate.Content()[0].(llm.ToolCallBlock).ArgumentsJSON(); !bytes.Equal(got, arguments) {
		t.Fatalf("immediate arguments = %q, want exact %q", got, arguments)
	}
	if bytes.Contains(entry.RawJSON(), []byte{'\n'}) {
		t.Fatalf("entry record contains a physical newline: %q", entry.RawJSON())
	}
	committedRaw := entry.RawJSON()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Entries()[0].RawJSON(); !bytes.Equal(got, committedRaw) {
		t.Fatalf("reopened raw entry = %q, want %q", got, committedRaw)
	}
	restored := reopened.Context().Messages()[0].(llm.AssistantToolUseMessage)
	if got := restored.Content()[0].(llm.ToolCallBlock).ArgumentsJSON(); !bytes.Equal(got, arguments) {
		t.Fatalf("reopened arguments = %q, want exact %q", got, arguments)
	}
	if !restored.Timestamp().Equal(immediate.Timestamp()) || !reopened.Entries()[0].Timestamp().Equal(entry.Timestamp()) {
		t.Fatalf("reopened timestamps differ: message=%v entry=%v", restored.Timestamp(), reopened.Entries()[0].Timestamp())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(data, []byte{'\n'}); got != 2 {
		t.Fatalf("physical line count = %d, want header plus one entry", got)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate upstream JSON.parse -> JSON.stringify during fork/rewrite. String
	// escapes, object order, and number spelling may change without changing the
	// argument value; the exact Go copy must remain recoverable.
	lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	var genericEntry any
	if err := json.Unmarshal(lines[1], &genericEntry); err != nil {
		t.Fatal(err)
	}
	rewrittenEntry, err := json.Marshal(genericEntry)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(rewrittenEntry, committedRaw) {
		t.Fatal("rewrite simulation did not change lexical JSON")
	}
	rewrittenFile := append(append(append([]byte(nil), lines[0]...), '\n'), rewrittenEntry...)
	rewrittenFile = append(rewrittenFile, '\n')
	if err := os.WriteFile(path, rewrittenFile, 0o600); err != nil {
		t.Fatal(err)
	}
	afterRewrite, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatalf("Open() after upstream-style rewrite error = %v", err)
	}
	defer afterRewrite.Close()
	rewrittenMessage := afterRewrite.Context().Messages()[0].(llm.AssistantToolUseMessage)
	if got := rewrittenMessage.Content()[0].(llm.ToolCallBlock).ArgumentsJSON(); !bytes.Equal(got, arguments) {
		t.Fatalf("arguments after rewrite = %q, want exact %q", got, arguments)
	}
}

func TestToolResultDetailsRoundTripThroughDurableSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "details.jsonl")
	session, err := Create(path, CreateOptions{
		ID: "details", WorkingDir: "/workspace", Now: func() time.Time { return time.Date(2026, time.August, 2, 0, 0, 0, 0, time.UTC) },
		NewEntryID: sequenceIDs("details-entry"),
	})
	if err != nil {
		t.Fatal(err)
	}
	block, err := llm.NewTextBlock("result")
	if err != nil {
		t.Fatal(err)
	}
	message, err := llm.NewToolResultMessageWithDetails("call", "tool", []llm.TextBlock{block}, false, time.UnixMilli(1), json.RawMessage(`{"trace":{"id":"kept"}}`))
	if err != nil {
		t.Fatal(err)
	}
	entry, err := session.Append(context.Background(), message, AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(entry.RawJSON(), []byte(`"details":{"trace":{"id":"kept"}}`)) {
		t.Fatalf("details absent from raw entry: %s", entry.RawJSON())
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, ok := reopened.Context().Messages()[0].(llm.ToolResultMessage)
	if !ok || string(restored.Details()) != `{"trace":{"id":"kept"}}` {
		t.Fatalf("restored details = %#v", reopened.Context().Messages())
	}
}

func TestConcurrentAppendFormsOneDurableParentChain(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "concurrent.jsonl")
	clock := func() time.Time { return time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC) }
	index := 0
	session, err := Create(path, CreateOptions{
		ID:         "concurrent",
		WorkingDir: directory,
		Now:        clock,
		NewEntryID: func() (string, error) {
			index++
			return fmt.Sprintf("entry-%03d", index), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	const count = 32
	var group sync.WaitGroup
	errorsByCall := make(chan error, count)
	for call := 0; call < count; call++ {
		call := call
		group.Add(1)
		go func() {
			defer group.Done()
			message := mustUserMessage(t, fmt.Sprintf("message-%d", call), clock())
			_, err := session.Append(context.Background(), message, AppendOptions{})
			errorsByCall <- err
		}()
	}
	group.Wait()
	close(errorsByCall)
	for err := range errorsByCall {
		if err != nil {
			t.Fatal(err)
		}
	}

	entries := session.Entries()
	if len(entries) != count {
		t.Fatalf("len(Entries()) = %d, want %d", len(entries), count)
	}
	assertLinearChain(t, entries)
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertLinearChain(t, reopened.Entries())
}

func TestEntryAndHeaderSnapshotsOwnRawBytes(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "snapshots.jsonl")
	session, err := Create(path, CreateOptions{
		ID: "snapshots", WorkingDir: directory,
		NewEntryID: sequenceIDs("entry-1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Append(context.Background(), mustUserMessage(t, "hello", time.Now()), AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	header := session.Header()
	headerRaw := header.RawJSON()
	headerRaw[0] = '!'
	if session.Header().RawJSON()[0] != '{' {
		t.Fatal("header raw bytes mutated through snapshot")
	}
	entries := session.Entries()
	raw := entries[0].RawJSON()
	raw[0] = '!'
	if session.Entries()[0].RawJSON()[0] != '{' {
		t.Fatal("entry raw bytes mutated through snapshot")
	}
}

func TestMessageCodecRoundTripsFailureAndErrorToolResult(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "failures.jsonl")
	session, err := Create(path, CreateOptions{
		ID: "failures", WorkingDir: directory,
		NewEntryID: sequenceIDs("entry-1", "entry-2"),
	})
	if err != nil {
		t.Fatal(err)
	}
	failure, err := newAssistantFailureMessage(
		[]llm.TextBlock{mustTextBlock(t, "partial")},
		llm.FinishAborted,
		"cancelled",
		mustUsage(t, 2, 1),
		time.UnixMilli(10),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := llm.NewToolResultMessage(
		"call-1", "bash", []llm.TextBlock{mustTextBlock(t, "failed")}, true, time.UnixMilli(11),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Append(context.Background(), failure, AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Append(context.Background(), result, AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	messages := reopened.Context().Messages()
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want 2", len(messages))
	}
	gotFailure := messages[0].(llm.AssistantFailureMessage)
	if gotFailure.FinishReason() != llm.FinishAborted || gotFailure.ErrorMessage() != "cancelled" || gotFailure.Content()[0].Text() != "partial" {
		t.Fatalf("failure = %#v", gotFailure)
	}
	gotResult := messages[1].(llm.ToolResultMessage)
	if !gotResult.IsError() || gotResult.Content()[0].Text() != "failed" {
		t.Fatalf("tool result = %#v", gotResult)
	}
	entries := reopened.Entries()
	if message, ok := entries[0].Message(); !ok || message.Role() != llm.RoleAssistant {
		t.Fatalf("Entry.Message() = (%#v, %t)", message, ok)
	}
	identity, ok := entries[0].AssistantProvenance()
	if !ok || identity != testAssistantProvenance {
		t.Fatalf("Entry.AssistantProvenance() = (%#v, %t)", identity, ok)
	}
	if entries[0].Type() != "message" || entries[0].Timestamp().IsZero() || len(entries[0].Diagnostics()) != 0 {
		t.Fatalf("entry snapshot = %#v", entries[0])
	}
	if reopened.Header().Timestamp().IsZero() {
		t.Fatal("header timestamp is zero")
	}
}

func TestCreateDoesNotOverwriteExistingSessionPath(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "existing.jsonl")
	original := []byte("not a session\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Create(path, CreateOptions{ID: "new", WorkingDir: directory})
	if !errors.Is(err, ErrStorage) {
		t.Fatalf("Create() error = %v, want ErrStorage", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("existing path changed: %q", after)
	}
	temporary, globErr := filepath.Glob(filepath.Join(directory, ".pi-go-session-*"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(temporary) != 0 {
		t.Fatalf("temporary files left behind: %v", temporary)
	}
}

func TestCreateRequiresPreexistingParentDirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	parent := filepath.Join(root, "missing", "nested")
	path := filepath.Join(parent, "session.jsonl")
	_, err := Create(path, CreateOptions{ID: "missing-parent", WorkingDir: root})
	if !errors.Is(err, ErrStorage) {
		t.Fatalf("Create() error = %v, want ErrStorage", err)
	}
	if _, statErr := os.Stat(parent); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("missing parent was created or has unexpected error: %v", statErr)
	}
}

func TestCreateAndAppendRejectUnreopenableISOTimestamps(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	invalid := time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(directory, "invalid-header-time.jsonl")
	_, err := Create(path, CreateOptions{
		ID: "invalid-time", WorkingDir: directory, Now: func() time.Time { return invalid },
	})
	if !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("Create() error = %v, want ErrInvalidSession", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("invalid header timestamp created a file: %v", statErr)
	}

	valid := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	times := []time.Time{valid, invalid, valid.Add(time.Second)}
	index := 0
	session, err := Create(filepath.Join(directory, "invalid-entry-time.jsonl"), CreateOptions{
		ID: "entry-time", WorkingDir: directory,
		Now:        func() time.Time { value := times[index]; index++; return value },
		NewEntryID: sequenceIDs("entry-1", "entry-2"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	message := mustUserMessage(t, "hello", valid)
	if _, err := session.Append(context.Background(), message, AppendOptions{}); !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("Append() error = %v, want ErrInvalidEntry", err)
	}
	if len(session.Entries()) != 0 || session.Poisoned() {
		t.Fatalf("invalid entry timestamp changed state: entries=%d poisoned=%t", len(session.Entries()), session.Poisoned())
	}
	entry, err := session.Append(context.Background(), message, AppendOptions{})
	if err != nil || entry.ID() != "entry-2" {
		t.Fatalf("retry after invalid timestamp = (%q, %v), want entry-2", entry.ID(), err)
	}
}

func TestDefaultSessionAndEntryIDsAreValid(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	session, err := Create(filepath.Join(directory, "generated.jsonl"), CreateOptions{WorkingDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateOpaqueID(session.Header().ID(), "session id"); err != nil {
		t.Fatal(err)
	}
	if id := session.Header().ID(); len(id) != 36 || id[14] != '7' {
		t.Fatalf("generated session id = %q, want UUIDv7", id)
	}
	entry, err := session.Append(context.Background(), mustUserMessage(t, "hello", time.Now()), AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entry.ID()) != 8 {
		t.Fatalf("generated entry id length = %d, want 8", len(entry.ID()))
	}
	if leaf, ok := session.LeafID(); !ok || leaf != entry.ID() {
		t.Fatalf("LeafID() = (%q, %t), want %q", leaf, ok, entry.ID())
	}
}

func TestSessionPathHasOneInProcessWriterUntilClose(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "exclusive.jsonl")
	first, err := Create(path, CreateOptions{ID: "exclusive", WorkingDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, OpenOptions{}); !errors.Is(err, ErrWriterActive) {
		t.Fatalf("concurrent Open() error = %v, want ErrWriterActive", err)
	}
	if _, err := Create(path, CreateOptions{ID: "replacement", WorkingDir: directory}); !errors.Is(err, ErrWriterActive) {
		t.Fatalf("concurrent Create() error = %v, want ErrWriterActive", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatalf("Open() after Close error = %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionWriterRejectsSymlinkAndHardLinkAliases(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "original.jsonl")
	first, err := Create(path, CreateOptions{ID: "aliases", WorkingDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	hardLink := filepath.Join(directory, "hard-link.jsonl")
	if err := os.Link(path, hardLink); err != nil {
		t.Fatalf("create hard-link alias: %v", err)
	}
	if _, err := Open(hardLink, OpenOptions{}); !errors.Is(err, ErrWriterActive) {
		t.Fatalf("Open(hard link) error = %v, want ErrWriterActive", err)
	}

	symlink := filepath.Join(directory, "symlink.jsonl")
	if err := os.Symlink(path, symlink); err != nil {
		t.Logf("symlink alias check unavailable: %v", err)
		return
	}
	if _, err := Open(symlink, OpenOptions{}); !errors.Is(err, ErrWriterActive) {
		t.Fatalf("Open(symlink) error = %v, want ErrWriterActive", err)
	}
}

func assertLinearChain(t *testing.T, entries []Entry) {
	t.Helper()
	for index, entry := range entries {
		parent, hasParent := entry.ParentID()
		if index == 0 {
			if hasParent {
				t.Fatalf("root parent = %q", parent)
			}
			continue
		}
		if !hasParent || parent != entries[index-1].ID() {
			t.Fatalf("entry %d parent = (%q, %t), want %q", index, parent, hasParent, entries[index-1].ID())
		}
	}
}

func assertToolTurnContext(t *testing.T, context Context) {
	t.Helper()
	messages := context.Messages()
	if len(messages) != 4 {
		t.Fatalf("context message count = %d, want 4", len(messages))
	}
	if got := messages[0].(llm.UserTextMessage).Content()[0].Text(); got != "run it" {
		t.Fatalf("user text = %q", got)
	}
	toolUse := messages[1].(llm.AssistantToolUseMessage)
	if got := toolUse.Content()[1].(llm.ToolCallBlock).ArgumentsJSON(); !bytes.Equal(got, []byte(`{"command":"printf ok"}`)) {
		t.Fatalf("tool arguments = %s", got)
	}
	result := messages[2].(llm.ToolResultMessage)
	if result.ToolCallID() != "call-1" || result.ToolName() != "bash" || result.IsError() {
		t.Fatalf("tool result = %#v", result)
	}
	if got := messages[3].(llm.AssistantTextMessage).Content()[0].Text(); got != "done" {
		t.Fatalf("final text = %q", got)
	}
}

func mustUserMessage(t *testing.T, text string, timestamp time.Time) llm.UserTextMessage {
	t.Helper()
	message, err := llm.NewUserTextMessage(text, timestamp)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func mustTextBlock(t *testing.T, text string) llm.TextBlock {
	t.Helper()
	block, err := llm.NewTextBlock(text)
	if err != nil {
		t.Fatal(err)
	}
	return block
}

func mustToolCall(t *testing.T, id, name string, arguments []byte) llm.ToolCallBlock {
	t.Helper()
	call, err := llm.NewToolCallBlock(id, name, arguments)
	if err != nil {
		t.Fatal(err)
	}
	return call
}

func mustUsage(t *testing.T, input, output uint64) llm.Usage {
	t.Helper()
	usage, err := llm.NewUsage(llm.UsageSpec{Input: input, Output: output})
	if err != nil {
		t.Fatal(err)
	}
	return usage
}

func sequenceClock(start time.Time) Clock {
	var mu sync.Mutex
	next := start
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		result := next
		next = next.Add(time.Millisecond)
		return result
	}
}

func sequenceIDs(ids ...string) IDGenerator {
	index := 0
	return func() (string, error) {
		if index >= len(ids) {
			return "", errors.New("id sequence exhausted")
		}
		id := ids[index]
		index++
		return id, nil
	}
}
