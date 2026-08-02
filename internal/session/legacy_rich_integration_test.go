package session

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

func TestLegacyV2MigrationPreservesRichRawDataAndWriterSafety(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "legacy-rich.jsonl")
	header := `{"type":"session","version":2,"id":"legacy-rich","timestamp":"2026-08-01T00:00:00.000Z","cwd":"/workspace","futureHeader":{"n":900719925474099312345}}`
	content := `[{"type":"thinking","thinking":"visible plan","thinkingSignature":"anthropic-opaque-secret"},` +
		`{"type":"text","text":"future answer","textSignature":"{\"v\":9,\"id\":\"msg_future\",\"phase\":\"future_phase\"}"},` +
		`{"type":"futureRich","opaque":{"keep":true},"signature":"future-rich-secret"}]`
	entry := assistantReplayEntryJSON(
		"legacy-assistant", "null", "anthropic", "anthropic-messages", "claude-test", content,
		`,"futureMessage":{"precise":1.00e2}`,
	)
	legacy := []byte(header + "\n" + entry + "\n")
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}

	transcript, err := Open(path, OpenOptions{
		Now:        func() time.Time { return time.Date(2026, time.August, 1, 0, 0, 2, 0, time.UTC) },
		NewEntryID: sequenceIDs("after-migration"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer transcript.Close()

	migrated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(migrated, legacy) || !bytes.Contains(migrated, []byte(`"version":3`)) {
		t.Fatalf("legacy session was not published as v3: %s", migrated)
	}
	legacyHeader, err := decodeObject([]byte(header))
	if err != nil {
		t.Fatal(err)
	}
	migratedHeader, err := decodeObject(transcript.Header().RawJSON())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(legacyHeader["futureHeader"], migratedHeader["futureHeader"]) {
		t.Fatalf("future header changed: %s", transcript.Header().RawJSON())
	}
	legacyEntry, err := decodeObject([]byte(entry))
	if err != nil {
		t.Fatal(err)
	}
	migratedEntry, err := decodeObject(transcript.Entries()[0].RawJSON())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(legacyEntry["message"], migratedEntry["message"]) {
		t.Fatalf("rich legacy message changed during migration:\nold %s\nnew %s", legacyEntry["message"], migratedEntry["message"])
	}

	projected := transcript.BuildContext()
	messages := projected.Messages()
	if len(messages) != 1 {
		t.Fatalf("migrated context messages = %#v", messages)
	}
	assistant, ok := messages[0].(llm.AssistantRichMessage)
	if !ok {
		t.Fatalf("migrated rich message type = %T", messages[0])
	}
	blocks := assistant.Blocks()
	if len(blocks) != 2 || blocks[0].(llm.ThinkingBlock).Thinking() != "visible plan" || blocks[1].(llm.TextBlock).Text() != "future answer" {
		t.Fatalf("migrated safe rich blocks = %#v", blocks)
	}
	if signature, ok := blocks[0].(llm.ThinkingBlock).ThinkingSignature(); !ok || signature != "anthropic-opaque-secret" {
		t.Fatalf("foreign reasoning signature = (%q, %t)", signature, ok)
	}
	if signature, ok := blocks[1].(llm.TextBlock).TextSignature(); !ok || signature != `{"v":9,"id":"msg_future","phase":"future_phase"}` {
		t.Fatalf("future foreign text signature = (%q, %t)", signature, ok)
	}
	if response, ok := assistant.ResponseMetadata(); ok {
		t.Fatalf("unexpected assistant response metadata: %#v", response)
	}
	var unsafe, unknown int
	for _, diagnostic := range projected.Diagnostics() {
		switch diagnostic.Code {
		case DiagnosticUnsafeContentOmitted:
			unsafe++
		case DiagnosticUnknownContentBlock:
			unknown++
		}
	}
	if unsafe != 0 || unknown != 1 {
		t.Fatalf("migrated rich diagnostics = %#v", projected.Diagnostics())
	}

	blocked, blockedErr := Open(path, OpenOptions{})
	if blocked != nil {
		_ = blocked.Close()
	}
	if !errors.Is(blockedErr, ErrWriterActive) {
		t.Fatalf("second migrated writer = %v, want ErrWriterActive", blockedErr)
	}
	hardLink := filepath.Join(directory, "legacy-rich-alias.jsonl")
	if err := os.Link(path, hardLink); err == nil {
		runWriterClaimHelper(t, hardLink, "active")
	} else {
		t.Logf("hardlink writer oracle unavailable: %v", err)
	}

	beforeAppend := bytes.Clone(migrated)
	if _, err := transcript.Append(
		context.Background(),
		mustUserMessage(t, "after migration", time.UnixMilli(2)),
		AppendOptions{},
	); err != nil {
		t.Fatal(err)
	}
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}
	afterAppend, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(afterAppend, beforeAppend) {
		t.Fatalf("append changed migrated rich prefix: %v\n%s", err, afterAppend)
	}
	reopened, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(reopened.BuildContext().Messages()) != 2 {
		t.Fatalf("reopened migrated context = %#v", reopened.BuildContext().Messages())
	}
}

func TestV3RichOpenAppendNeverEntersLegacyRewrite(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "rich-v3.jsonl")
	content := `[{"type":"thinking","thinking":"plan","thinkingSignature":"{\"type\":\"reasoning\",\"id\":\"rs_current\",\"encrypted_content\":\"cipher\"}"},` +
		`{"type":"text","text":"answer","textSignature":"{\"v\":1,\"id\":\"msg_current\",\"phase\":\"final_answer\"}"}]`
	entry := assistantReplayEntryJSON("rich", "null", "openai", "openai-responses", "gpt-test", content, `,"responseId":"resp_current","rawStopReason":"completed"`)
	original := []byte(testHeader + "\n" + entry + "\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	transcript, err := Open(path, OpenOptions{
		Now:        func() time.Time { return time.Date(2026, time.August, 1, 0, 0, 2, 0, time.UTC) },
		NewEntryID: sequenceIDs("v3-append"),
	})
	if err != nil {
		t.Fatal(err)
	}
	dataAfterOpen, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(dataAfterOpen, original) {
		t.Fatalf("v3 Open entered migration rewrite: %v\n%s", err, dataAfterOpen)
	}
	assistant := transcript.BuildContext().Messages()[0].(llm.AssistantRichMessage)
	if signature, ok := assistant.Blocks()[0].(llm.ThinkingBlock).ThinkingSignature(); !ok || signature != `{"type":"reasoning","id":"rs_current","encrypted_content":"cipher"}` {
		t.Fatalf("v3 reasoning signature = (%q, %t)", signature, ok)
	}
	if signature, ok := assistant.Blocks()[1].(llm.TextBlock).TextSignature(); !ok || signature != `{"v":1,"id":"msg_current","phase":"final_answer"}` {
		t.Fatalf("v3 text signature = (%q, %t)", signature, ok)
	}
	if _, err := transcript.Append(context.Background(), mustUserMessage(t, "continue", time.UnixMilli(2)), AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}
	afterAppend, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(afterAppend, original) {
		t.Fatalf("v3 append changed rich prefix: %v\n%s", err, afterAppend)
	}
}

func TestRecoverTrailingPartialKeepsRichSelectedBranch(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "rich-partial.jsonl")
	content := `[{"type":"thinking","thinking":"selected plan","thinkingSignature":"{\"type\":\"reasoning\",\"id\":\"rs_selected\",\"encrypted_content\":\"cipher\"}"},` +
		`{"type":"text","text":"selected answer","textSignature":"{\"v\":1,\"id\":\"msg_selected\",\"phase\":\"final_answer\"}"}]`
	rich := assistantReplayEntryJSON("rich-tail", `"root"`, "openai", "openai-responses", "gpt-test", content, "")
	prefix := []byte(testHeader + "\n" +
		userEntryJSON("root message", "root", "null", 1) + "\n" +
		userEntryJSON("unselected sibling", "sibling", `"root"`, 2) + "\n" +
		rich + "\n")
	original := append(bytes.Clone(prefix), '{')
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := RecoverTrailingPartial(path)
	if err != nil {
		t.Fatal(err)
	}
	if result.TruncatedBytes != 1 {
		t.Fatalf("truncated bytes = %d", result.TruncatedBytes)
	}
	recovered, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(recovered, prefix) {
		t.Fatalf("recovered rich prefix = %q, %v", recovered, err)
	}
	backup, err := os.ReadFile(result.BackupPath)
	if err != nil || !bytes.Equal(backup, original) {
		t.Fatalf("rich recovery backup = %q, %v", backup, err)
	}

	transcript, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer transcript.Close()
	if leaf, ok := transcript.LeafID(); !ok || leaf != "rich-tail" {
		t.Fatalf("recovered selected leaf = (%q, %t)", leaf, ok)
	}
	entries := transcript.Entries()
	parent, ok := entries[2].ParentID()
	if !ok || parent != "root" {
		t.Fatalf("recovered rich parent = (%q, %t)", parent, ok)
	}
	messages := transcript.BuildContext().Messages()
	if len(messages) != 2 || messages[0].(llm.UserTextMessage).Content()[0].Text() != "root message" {
		t.Fatalf("recovered selected context = %#v", messages)
	}
	assistant := messages[1].(llm.AssistantRichMessage)
	if got := assistant.Blocks()[1].(llm.TextBlock).Text(); got != "selected answer" {
		t.Fatalf("recovered rich tail text = %q", got)
	}
	if signature, ok := assistant.Blocks()[0].(llm.ThinkingBlock).ThinkingSignature(); !ok || !strings.Contains(signature, `"id":"rs_selected"`) {
		t.Fatalf("recovered reasoning signature = (%q, %t)", signature, ok)
	}
}
