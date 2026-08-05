package session

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

func TestLengthToolUseSurvivesJSONLCloseAndReopen(t *testing.T) {
	t.Parallel()
	assertToolUseFinishSurvivesJSONL(t, llm.FinishLength)
}

func TestStopToolUseSurvivesJSONLCloseAndReopen(t *testing.T) {
	t.Parallel()
	assertToolUseFinishSurvivesJSONL(t, llm.FinishStop)
}

func TestSyntheticToolErrorDetailsSurviveJSONLCloseAndReopen(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "synthetic-tool-error.jsonl")
	at := time.Date(2026, time.August, 5, 2, 3, 4, 0, time.UTC)
	text, err := llm.NewTextBlock("Tool missing not found")
	if err != nil {
		t.Fatal(err)
	}
	message, err := llm.NewToolResultMessageWithMetadata(
		"call-1", "missing", []llm.TextBlock{text}, true, at,
		llm.ToolResultMetadata{Details: []byte(`{}`)},
	)
	if err != nil {
		t.Fatal(err)
	}
	session, err := Create(path, CreateOptions{ID: "session-1", WorkingDir: directory, Now: func() time.Time { return at }, NewEntryID: sequenceIDs("entry-1")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Append(context.Background(), message, AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"details":{}`)) {
		t.Fatalf("JSONL omitted empty details: %s", encoded)
	}
	reopened, err := Open(path, OpenOptions{Now: func() time.Time { return at }, NewEntryID: sequenceIDs("entry-2")})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	messages := reopened.Context().Messages()
	got, ok := messages[0].(llm.ToolResultMessage)
	if !ok || !got.IsError() || string(got.Details()) != `{}` {
		t.Fatalf("reopened message = %#v details=%s", messages[0], got.Details())
	}
}

func assertToolUseFinishSurvivesJSONL(t *testing.T, finish llm.FinishReason) {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, finish.String()+"-tool.jsonl")
	at := time.Date(2026, time.August, 5, 2, 3, 4, 0, time.UTC)
	call, err := llm.NewToolCallBlockWithThoughtSignature("call-1", "inspect", []byte(`{"path":"README.md"}`), "opaque-signature")
	if err != nil {
		t.Fatal(err)
	}
	cost := llm.Cost{Input: 0.1, Output: 0.2, Total: 0.3}
	usage, err := llm.NewUsage(llm.UsageSpec{Input: 7, Output: 3, Cost: &cost})
	if err != nil {
		t.Fatal(err)
	}
	response := llm.AssistantResponseMetadata{ResponseID: "response-1", ResponseModel: "scripted-1", RawStopReason: "max_tokens"}
	diagnostic, err := llm.NewAssistantDiagnostic(llm.AssistantDiagnosticSpec{Type: "notice", Timestamp: at, Details: []byte(`{"value":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	message, err := llm.NewAssistantToolUseMessageWithFinishAndMetadata(
		[]llm.AssistantBlock{call}, finish, usage, at,
		testLLMAssistantProvenance(), &response, []llm.AssistantDiagnostic{diagnostic},
	)
	if err != nil {
		t.Fatal(err)
	}

	session, err := Create(path, CreateOptions{ID: "session-1", WorkingDir: directory, Now: func() time.Time { return at }, NewEntryID: sequenceIDs("entry-1")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Append(context.Background(), message, AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"stopReason":"`+finish.String()+`"`)) {
		t.Fatalf("JSONL omitted %s stop reason: %s", finish, encoded)
	}

	reopened, err := Open(path, OpenOptions{Now: func() time.Time { return at }, NewEntryID: sequenceIDs("entry-2")})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	messages := reopened.Context().Messages()
	if len(messages) != 1 {
		t.Fatalf("messages = %d", len(messages))
	}
	got, ok := messages[0].(llm.AssistantToolUseMessage)
	if !ok || got.FinishReason() != finish || got.Usage() != usage || !got.Timestamp().Equal(at) {
		t.Fatalf("reopened message = %#v", messages[0])
	}
	gotResponse, ok := got.ResponseMetadata()
	if !ok || gotResponse != response || len(got.Diagnostics()) != 1 {
		t.Fatalf("reopened metadata = (%#v, %t, diagnostics=%d)", gotResponse, ok, len(got.Diagnostics()))
	}
	gotCall := got.Blocks()[0].(llm.ToolCallBlock)
	gotSignature, gotHasSignature := gotCall.ThoughtSignature()
	wantSignature, wantHasSignature := call.ThoughtSignature()
	if gotCall.ID() != call.ID() || gotCall.Name() != call.Name() || gotSignature != wantSignature || gotHasSignature != wantHasSignature || !bytes.Equal(gotCall.ArgumentsJSON(), call.ArgumentsJSON()) {
		t.Fatalf("reopened call = %#v", gotCall)
	}
}
