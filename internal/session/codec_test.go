package session

import (
	"bytes"
	stdcontext "context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

const testHeader = `{"type":"session","version":3,"id":"session-1","timestamp":"2026-08-01T00:00:00.000Z","cwd":"/workspace"}`

func TestOpenProjectsForeignOpaqueSignaturesAsSafeUnsignedContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		provider   string
		api        string
		model      string
		content    string
		wantKinds  []llm.AssistantBlockKind
		wantTexts  []string
		wantUnsafe []int
	}{
		{
			name:     "anthropic visible and redacted thinking",
			provider: "anthropic",
			api:      "anthropic-messages",
			model:    "claude-test",
			content: `[{"type":"thinking","thinking":"visible plan","thinkingSignature":"anthropic-opaque-secret"},` +
				`{"type":"thinking","thinking":"","thinkingSignature":"anthropic-redacted-secret","redacted":true},` +
				`{"type":"text","text":"answer"}]`,
			wantKinds:  []llm.AssistantBlockKind{llm.AssistantBlockThinking, llm.AssistantBlockText},
			wantTexts:  []string{"visible plan", "answer"},
			wantUnsafe: []int{0, 1},
		},
		{
			name:     "google signed empty and visible parts",
			provider: "google",
			api:      "google-generative-ai",
			model:    "gemini-test",
			content: `[{"type":"thinking","thinking":"","thinkingSignature":"Z29vZ2xlLW9wYXF1ZQ=="},` +
				`{"type":"thinking","thinking":"visible thought","thinkingSignature":"Z29vZ2xlLXZpc2libGU="},` +
				`{"type":"text","text":"","textSignature":"Z29vZ2xlLWVtcHR5LXRleHQ="},` +
				`{"type":"text","text":"answer","textSignature":"Z29vZ2xlLXRleHQ="}]`,
			wantKinds:  []llm.AssistantBlockKind{llm.AssistantBlockThinking, llm.AssistantBlockText},
			wantTexts:  []string{"visible thought", "answer"},
			wantUnsafe: []int{0, 1, 2, 3},
		},
		{
			name:     "foreign provider cannot borrow Responses API provenance",
			provider: "anthropic",
			api:      "openai-responses",
			model:    "claude-test",
			content: `[{"type":"thinking","thinking":"visible plan","thinkingSignature":"{\"type\":\"reasoning\",\"id\":\"rs_foreign\",\"encrypted_content\":\"must-not-project\"}"},` +
				`{"type":"text","text":"answer","textSignature":"{\"v\":1,\"id\":\"msg_foreign\",\"phase\":\"final_answer\"}"}]`,
			wantKinds:  []llm.AssistantBlockKind{llm.AssistantBlockThinking, llm.AssistantBlockText},
			wantTexts:  []string{"visible plan", "answer"},
			wantUnsafe: []int{0, 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			entry := assistantReplayEntryJSON("entry-1", "null", tt.provider, tt.api, tt.model, tt.content, "")
			path := writeSessionFixture(t, testHeader+"\n"+entry+"\n")
			transcript, err := Open(path, OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = transcript.Close() })

			if got := transcript.Entries()[0].RawJSON(); !bytes.Equal(got, []byte(entry)) {
				t.Fatal("foreign opaque signature record was not byte-preserved")
			}
			messages := transcript.Context().Messages()
			if len(messages) != 1 {
				t.Fatalf("messages = %#v", messages)
			}
			metadataCarrier := messages[0].(interface {
				OpenAIResponsesMetadata() (llm.OpenAIResponsesResponse, bool)
			})
			if replay, ok := metadataCarrier.OpenAIResponsesMetadata(); ok {
				t.Fatalf("untrusted response metadata projected: %#v", replay)
			}
			blocks := assistantBlocks(messages[0])
			if len(blocks) != len(tt.wantKinds) {
				t.Fatalf("blocks = %#v, want %d safe blocks", blocks, len(tt.wantKinds))
			}
			for index, block := range blocks {
				if block.Kind() != tt.wantKinds[index] {
					t.Fatalf("block %d kind = %v, want %v", index, block.Kind(), tt.wantKinds[index])
				}
				switch block := block.(type) {
				case llm.ThinkingBlock:
					if block.Thinking() != tt.wantTexts[index] {
						t.Fatalf("thinking %d = %q", index, block.Thinking())
					}
					if replay, ok := block.OpenAIResponsesReplay(); ok {
						t.Fatalf("foreign reasoning projected as Responses replay: %#v", replay)
					}
				case llm.TextBlock:
					if block.Text() != tt.wantTexts[index] {
						t.Fatalf("text %d = %q", index, block.Text())
					}
					if replay, ok := block.TextReplay(); ok {
						t.Fatalf("foreign text projected as Responses replay: %#v", replay)
					}
				}
			}
			diagnostics := transcript.Context().Diagnostics()
			if len(diagnostics) != len(tt.wantUnsafe) {
				t.Fatalf("diagnostics = %#v", diagnostics)
			}
			for index, contentIndex := range tt.wantUnsafe {
				if diagnostics[index] != (Diagnostic{Code: DiagnosticUnsafeContentOmitted, EntryID: "entry-1", ContentIndex: contentIndex}) {
					t.Fatalf("diagnostic %d = %#v", index, diagnostics[index])
				}
			}
		})
	}
}

func TestOpenResponsesReplayMetadataFailsSafe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		content       string
		extra         string
		wantBlockKind llm.AssistantBlockKind
		wantBlocks    int
	}{
		{name: "future text envelope version", content: `[{"type":"text","text":"answer","textSignature":"{\"v\":2,\"id\":\"msg_future\",\"phase\":\"final_answer\"}"}]`, wantBlockKind: llm.AssistantBlockText, wantBlocks: 1},
		{name: "future signed empty text", content: `[{"type":"text","text":"","textSignature":"{\"v\":2,\"id\":\"msg_future\"}"}]`, wantBlocks: 0},
		{name: "future text envelope field", content: `[{"type":"text","text":"answer","textSignature":"{\"v\":1,\"id\":\"msg_future\",\"future\":true}"}]`, wantBlockKind: llm.AssistantBlockText, wantBlocks: 1},
		{name: "unknown text phase", content: `[{"type":"text","text":"answer","textSignature":"{\"v\":1,\"id\":\"msg_future\",\"phase\":\"future_phase\"}"}]`, wantBlockKind: llm.AssistantBlockText, wantBlocks: 1},
		{name: "malformed text envelope", content: `[{"type":"text","text":"answer","textSignature":"{"}]`, wantBlockKind: llm.AssistantBlockText, wantBlocks: 1},
		{name: "non string text envelope", content: `[{"type":"text","text":"answer","textSignature":{"v":1,"id":"msg_object"}}]`, wantBlockKind: llm.AssistantBlockText, wantBlocks: 1},
		{name: "malformed readable reasoning", content: `[{"type":"thinking","thinking":"visible plan","thinkingSignature":"opaque-not-json"}]`, wantBlockKind: llm.AssistantBlockThinking, wantBlocks: 1},
		{name: "future readable reasoning envelope", content: `[{"type":"thinking","thinking":"visible plan","thinkingSignature":"{\"type\":\"reasoning\",\"id\":\"rs_future\",\"encrypted_content\":\"must-not-project\",\"future\":true}"}]`, wantBlockKind: llm.AssistantBlockThinking, wantBlocks: 1},
		{name: "unsupported opaque only reasoning", content: `[{"type":"thinking","thinking":"","thinkingSignature":"{\"type\":\"future_reasoning\",\"id\":\"rs_future\",\"encrypted_content\":\"must-not-project\"}"}]`, wantBlocks: 0},
		{name: "malformed response metadata", content: `[{"type":"text","text":"answer"}]`, extra: `,"responseId":{"future":true}`, wantBlockKind: llm.AssistantBlockText, wantBlocks: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			entry := assistantReplayEntryJSON("entry-1", "null", "openai", "openai-responses", "gpt-test", tt.content, tt.extra)
			path := writeSessionFixture(t, testHeader+"\n"+entry+"\n")
			transcript, err := Open(path, OpenOptions{})
			if err != nil {
				t.Fatalf("Open() rejected compatible future metadata: %v", err)
			}
			t.Cleanup(func() { _ = transcript.Close() })

			if got := transcript.Entries()[0].RawJSON(); !bytes.Equal(got, []byte(entry)) {
				t.Fatal("untrusted Responses metadata record was not byte-preserved")
			}
			messages := transcript.Context().Messages()
			if len(messages) != 1 {
				t.Fatalf("messages = %#v", messages)
			}
			metadataCarrier := messages[0].(interface {
				OpenAIResponsesMetadata() (llm.OpenAIResponsesResponse, bool)
			})
			if replay, ok := metadataCarrier.OpenAIResponsesMetadata(); ok {
				t.Fatalf("untrusted response metadata projected: %#v", replay)
			}
			blocks := assistantBlocks(messages[0])
			if len(blocks) != tt.wantBlocks {
				t.Fatalf("blocks = %#v, want %d", blocks, tt.wantBlocks)
			}
			if tt.wantBlocks == 1 {
				if blocks[0].Kind() != tt.wantBlockKind {
					t.Fatalf("block kind = %v, want %v", blocks[0].Kind(), tt.wantBlockKind)
				}
				switch block := blocks[0].(type) {
				case llm.TextBlock:
					if replay, ok := block.TextReplay(); ok {
						t.Fatalf("untrusted text replay projected: %#v", replay)
					}
				case llm.ThinkingBlock:
					if replay, ok := block.OpenAIResponsesReplay(); ok {
						t.Fatalf("untrusted reasoning replay projected: %#v", replay)
					}
				}
			}
			if diagnostics := transcript.Context().Diagnostics(); len(diagnostics) != 1 || diagnostics[0].Code != DiagnosticUnsafeContentOmitted {
				t.Fatalf("diagnostics = %#v, want one safe-omission diagnostic", diagnostics)
			}
		})
	}
}

func TestOpenProjectsCurrentResponsesReplayEnvelope(t *testing.T) {
	t.Parallel()

	content := `[{"type":"thinking","thinking":"plan","thinkingSignature":"{\"type\":\"reasoning\",\"id\":\"rs_current\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"plan\"}],\"status\":\"completed\",\"encrypted_content\":\"cipher\"}"},` +
		`{"type":"text","text":"answer","textSignature":"{\"v\":1,\"id\":\"msg_current\",\"phase\":\"final_answer\"}"}]`
	entry := assistantReplayEntryJSON(
		"entry-1", "null", "openai", "openai-responses", "gpt-test", content,
		`,"responseId":"resp_current","rawStopReason":"completed"`,
	)
	path := writeSessionFixture(t, testHeader+"\n"+entry+"\n")
	transcript, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transcript.Close() })

	message := transcript.Context().Messages()[0].(llm.AssistantRichMessage)
	blocks := message.Blocks()
	reasoning, ok := blocks[0].(llm.ThinkingBlock).OpenAIResponsesReplay()
	if !ok || reasoning.ItemID != "rs_current" || reasoning.EncryptedContent != "cipher" {
		t.Fatalf("reasoning replay = (%#v, %t)", reasoning, ok)
	}
	textReplay, ok := blocks[1].(llm.TextBlock).TextReplay()
	if !ok || textReplay != (llm.TextReplay{MessageID: "msg_current", Phase: "final_answer"}) {
		t.Fatalf("text replay = (%#v, %t)", textReplay, ok)
	}
	response, ok := message.OpenAIResponsesMetadata()
	if !ok || response != (llm.OpenAIResponsesResponse{ResponseID: "resp_current", RawStopReason: "completed"}) {
		t.Fatalf("response replay = (%#v, %t)", response, ok)
	}
	if diagnostics := transcript.Context().Diagnostics(); len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
}

func TestOpaqueAndFutureReplayMetadataSurvivesForkAndReopen(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "source.jsonl")
	foreign := assistantReplayEntryJSON(
		"entry-1", "null", "anthropic", "anthropic-messages", "claude-test",
		`[{"type":"thinking","thinking":"visible plan","thinkingSignature":"anthropic-durable-secret"},{"type":"text","text":"answer"}]`, "",
	)
	future := assistantReplayEntryJSON(
		"entry-2", `"entry-1"`, "openai", "openai-responses", "gpt-test",
		`[{"type":"text","text":"future answer","textSignature":"{\"v\":9,\"id\":\"msg_future\",\"phase\":\"future_phase\"}"}]`, "",
	)
	data := testHeader + "\n" + foreign + "\n" + future + "\n"
	if err := os.WriteFile(sourcePath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := Open(sourcePath, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	assertUnsignedReplayProjection(t, source.Context(), 2)

	targetPath := filepath.Join(directory, "fork.jsonl")
	target, err := source.Fork(stdcontext.Background(), ExtractOptions{TargetPath: targetPath, ID: "fork", WorkingDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	wantRaw := [][]byte{[]byte(foreign), []byte(future)}
	for index, entry := range target.Entries() {
		if !bytes.Equal(entry.RawJSON(), wantRaw[index]) {
			t.Fatalf("fork entry %d changed opaque/future metadata", index)
		}
	}
	assertUnsignedReplayProjection(t, target.Context(), 2)
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(targetPath, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	for index, entry := range reopened.Entries() {
		if !bytes.Equal(entry.RawJSON(), wantRaw[index]) {
			t.Fatalf("reopened fork entry %d changed opaque/future metadata", index)
		}
	}
	assertUnsignedReplayProjection(t, reopened.Context(), 2)
}

func TestOpenAppendPreservesUnknownRawPrefixAndProjectsKnownContent(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "unknown.jsonl")
	prefix := []byte(
		` {"type":"session","version":3,"id":"session-1","timestamp":"2026-08-01T00:00:00.000Z","cwd":"/workspace","future":{"keep":true}}` + "\n" +
			`{"type":"message","id":"entry-1","parentId":null,"timestamp":"2026-08-01T00:00:01.000Z","futureEntry":1,"message":{"role":"user","content":[{"type":"text","text":"first"},{"type":"image","data":"AA==","mimeType":"image/png"},{"type":"text","text":"second"}],"timestamp":1785542401000,"futureMessage":{"x":1}}}` + "\n" +
			`{"type":"future_state","id":"entry-2","parentId":"entry-1","timestamp":"2026-08-01T00:00:02.000Z","payload":{"opaque":[1,2,3]}}` + "\n" +
			`{"type":"message","id":"entry-3","parentId":"entry-2","timestamp":"2026-08-01T00:00:03.000Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"opaque"},{"type":"text","text":"reply"}],"api":"scripted","provider":"scripted","model":"scripted-1","usage":{"input":1,"output":2,"cacheRead":0,"cacheWrite":0,"totalTokens":3,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"stop","timestamp":1785542403000}}`,
	)
	if err := os.WriteFile(path, prefix, 0o600); err != nil {
		t.Fatal(err)
	}

	session, err := Open(path, OpenOptions{
		Now:        func() time.Time { return time.Date(2026, time.August, 1, 0, 0, 4, 0, time.UTC) },
		NewEntryID: sequenceIDs("entry-4"),
	})
	if err != nil {
		t.Fatal(err)
	}
	context := session.Context()
	messages := context.Messages()
	if len(messages) != 2 {
		t.Fatalf("context message count = %d, want 2", len(messages))
	}
	user := messages[0].(llm.UserContentMessage)
	if got := user.Content(); len(got) != 3 || got[0].(llm.TextBlock).Text() != "first" || got[2].(llm.TextBlock).Text() != "second" {
		t.Fatalf("projected user content = %#v", got)
	}
	if got := messages[1].(llm.AssistantRichMessage).Blocks(); len(got) != 2 || got[1].(llm.TextBlock).Text() != "reply" {
		t.Fatalf("projected assistant content = %#v", got)
	}
	diagnostics := context.Diagnostics()
	if len(diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v, want unknown entry", diagnostics)
	}
	if diagnostics[0].Code != DiagnosticUnknownEntry {
		t.Fatalf("diagnostic codes = %#v", diagnostics)
	}

	if _, err := session.Append(
		stdcontext.Background(),
		mustUserMessage(t, "after", time.Date(2026, time.August, 1, 0, 0, 4, 0, time.UTC)),
		AppendOptions{},
	); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data[:len(prefix)], prefix) {
		t.Fatal("Open -> Append changed original bytes")
	}
	if data[len(prefix)] != '\n' || !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatalf("append after no-newline prefix has invalid separators: %q", data[len(prefix):])
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Context().Messages(); len(got) != 3 || got[2].(llm.UserTextMessage).Content()[0].Text() != "after" {
		t.Fatalf("reopened context = %#v", got)
	}
}

func TestOpenUsesPhysicalTailAncestryForBranchedSession(t *testing.T) {
	t.Parallel()

	data := testHeader + "\n" +
		userEntryJSON("root", "entry-1", "null", 1) + "\n" +
		assistantEntryJSON("abandoned", "entry-2", `"entry-1"`, 2) + "\n" +
		userEntryJSON("active", "entry-3", `"entry-1"`, 3) + "\n"
	path := writeSessionFixture(t, data)
	session, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(session.Entries()) != 3 {
		t.Fatalf("entry count = %d", len(session.Entries()))
	}
	messages := session.Context().Messages()
	if len(messages) != 2 {
		t.Fatalf("active context count = %d, want 2", len(messages))
	}
	if got := messages[0].(llm.UserTextMessage).Content()[0].Text(); got != "root" {
		t.Fatalf("root text = %q", got)
	}
	if got := messages[1].(llm.UserTextMessage).Content()[0].Text(); got != "active" {
		t.Fatalf("tail text = %q", got)
	}
	if _, ok := session.Context().AssistantProvenance(); ok {
		t.Fatal("identity from abandoned branch entered active context")
	}
}

func TestOpenRejectsUnsafeInputWithoutChangingFile(t *testing.T) {
	t.Parallel()

	validRoot := userEntryJSON("root", "entry-1", "null", 1)
	tests := []struct {
		name string
		data []byte
		want error
	}{
		{name: "empty", data: nil, want: ErrInvalidSession},
		{name: "future version", data: []byte(`{"type":"session","version":4,"id":"s","timestamp":"2026-08-01T00:00:00Z","cwd":"/w"}`), want: ErrUnsupportedVersion},
		{name: "corrupt middle", data: []byte(testHeader + "\n" + validRoot + "\nnot-json\n" + userEntryJSON("later", "entry-2", `"entry-1"`, 2) + "\n"), want: ErrInvalidEntry},
		{name: "trailing partial", data: []byte(testHeader + "\n" + validRoot + "\n{"), want: ErrInvalidEntry},
		{name: "duplicate id", data: []byte(testHeader + "\n" + validRoot + "\n" + userEntryJSON("again", "entry-1", `"entry-1"`, 2) + "\n"), want: ErrInvalidEntry},
		{name: "broken parent", data: []byte(testHeader + "\n" + userEntryJSON("broken", "entry-1", `"missing"`, 1) + "\n"), want: ErrUnsupportedTree},
		{name: "forward parent", data: []byte(testHeader + "\n" + userEntryJSON("forward", "entry-1", `"entry-2"`, 1) + "\n" + userEntryJSON("root", "entry-2", "null", 2) + "\n"), want: ErrUnsupportedTree},
		{name: "cycle", data: []byte(testHeader + "\n" + userEntryJSON("self", "entry-1", `"entry-1"`, 1) + "\n"), want: ErrUnsupportedTree},
		{name: "invalid utf8", data: append([]byte(testHeader+"\n"), 0xff, '\n'), want: ErrInvalidSession},
		{name: "known malformed message", data: []byte(testHeader + "\n" + `{"type":"message","id":"entry-1","parentId":null,"timestamp":"2026-08-01T00:00:01Z","message":{"role":"user","timestamp":1}}` + "\n"), want: ErrInvalidEntry},
		{name: "mismatched preserved arguments", data: []byte(testHeader + "\n" + `{"type":"message","id":"entry-1","parentId":null,"timestamp":"2026-08-01T00:00:01Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"echo","arguments":{"x":1},"_piGoRawArguments":"{\"x\":2}"}],"api":"scripted","provider":"scripted","model":"scripted-1","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"toolUse","timestamp":1}}` + "\n"), want: ErrInvalidEntry},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := writeSessionFixtureBytes(t, tt.data)
			before := append([]byte(nil), tt.data...)
			_, err := Open(path, OpenOptions{})
			if !errors.Is(err, tt.want) {
				t.Fatalf("Open() error = %v, want %v", err, tt.want)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(after, before) {
				t.Fatal("Open changed rejected input")
			}
		})
	}
}

func TestSemanticJSONEqualityPreservesValuesWithoutFloatRounding(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		left  string
		right string
		want  bool
	}{
		{name: "escape key order and decimal spelling", left: `{"escaped":"\u003c","n":1.00e2}`, right: `{"n":100,"escaped":"<"}`, want: true},
		{name: "extreme exponent", left: `{"n":1e999999999999999999999}`, right: `{"n":10e999999999999999999998}`, want: true},
		{name: "adjacent large integers", left: `{"n":9007199254740992}`, right: `{"n":9007199254740993}`, want: false},
		{name: "array order", left: `{"v":[1,2]}`, right: `{"v":[2,1]}`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := semanticJSONEqual([]byte(tt.left), []byte(tt.right))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("semanticJSONEqual() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestOpenAcceptsCompleteFinalRecordWithoutNewline(t *testing.T) {
	t.Parallel()
	path := writeSessionFixture(t, testHeader+"\n"+userEntryJSON("root", "entry-1", "null", 1))
	session, err := Open(path, OpenOptions{NewEntryID: sequenceIDs("entry-2")})
	if err != nil {
		t.Fatal(err)
	}
	if got := session.Context().Messages(); len(got) != 1 {
		t.Fatalf("context count = %d, want 1", len(got))
	}
}

func TestUnknownMessageRoleIsPreservedButNotProjected(t *testing.T) {
	t.Parallel()

	unknown := `{"type":"message","id":"entry-1","parentId":null,"timestamp":"2026-08-01T00:00:01Z","message":{"role":"futureRole","content":{"opaque":true},"timestamp":1}}`
	known := userEntryJSON("known", "entry-2", `"entry-1"`, 2)
	path := writeSessionFixture(t, testHeader+"\n"+unknown+"\n"+known+"\n")
	session, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	context := session.Context()
	if got := context.Messages(); len(got) != 1 || got[0].(llm.UserTextMessage).Content()[0].Text() != "known" {
		t.Fatalf("context messages = %#v", got)
	}
	if got := context.Diagnostics(); len(got) != 1 || got[0].Code != DiagnosticUnknownMessageRole {
		t.Fatalf("context diagnostics = %#v", got)
	}
	if message, ok := session.Entries()[0].Message(); ok || message != nil {
		t.Fatalf("unknown role projected as (%#v, %t)", message, ok)
	}
}

func TestOpenRejectsInvalidAssistantUsageAndCost(t *testing.T) {
	t.Parallel()

	base := `{"type":"message","id":"entry-1","parentId":null,"timestamp":"2026-08-01T00:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"reply"}],"api":"scripted","provider":"scripted","model":"scripted-1","usage":%s,"stopReason":"stop","timestamp":1}}`
	tests := []struct {
		name  string
		usage string
	}{
		{name: "missing cost", usage: `{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2}`},
		{name: "negative cost", usage: `{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":-1,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}`},
		{name: "fractional token", usage: `{"input":1.5,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}`},
		{name: "wrong total", usage: `{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":3,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}`},
		{name: "reasoning exceeds output", usage: `{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"reasoning":2,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := writeSessionFixture(t, testHeader+"\n"+fmt.Sprintf(base, tt.usage)+"\n")
			if _, err := Open(path, OpenOptions{}); !errors.Is(err, ErrInvalidEntry) {
				t.Fatalf("Open() error = %v, want ErrInvalidEntry", err)
			}
		})
	}
}

func FuzzDecodeSessionFileNeverPanics(f *testing.F) {
	f.Add([]byte(testHeader + "\n"))
	f.Add([]byte(testHeader + "\n" + userEntryJSON("seed", "entry-1", "null", 1) + "\n"))
	f.Add([]byte(testHeader + "\n" + assistantReplayEntryJSON(
		"entry-1", "null", "anthropic", "anthropic-messages", "claude-test",
		`[{"type":"thinking","thinking":"visible","thinkingSignature":"opaque-seed"}]`, "",
	) + "\n"))
	f.Add([]byte(testHeader + "\n" + assistantReplayEntryJSON(
		"entry-1", "null", "openai", "openai-responses", "gpt-test",
		`[{"type":"text","text":"answer","textSignature":"{\"v\":99,\"id\":\"future\",\"phase\":\"future\"}"}]`, "",
	) + "\n"))
	f.Add([]byte("not-json"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _, _, _ = decodeSessionFile("fuzz.jsonl", data)
	})
}

func FuzzAppendOnlyForestGraphProperties(f *testing.F) {
	// Multiple roots, a deep path, and each malformed graph family are explicit
	// corpus seeds. Fuzz mutations then vary both topology bytes and fault kind.
	for _, seed := range []struct {
		ops   []byte
		fault uint8
	}{
		{ops: []byte{0, 0, 0}, fault: 0},
		{ops: []byte{1, 1, 1, 1}, fault: 0},
		{ops: []byte{0, 1, 0, 3, 2}, fault: 0},
		{ops: []byte{0, 1}, fault: 1}, // duplicate
		{ops: []byte{0, 1}, fault: 2}, // forward
		{ops: []byte{0, 1}, fault: 3}, // broken
		{ops: []byte{0, 1}, fault: 4}, // self-cycle
	} {
		f.Add(seed.ops, seed.fault)
	}

	f.Fuzz(func(t *testing.T, ops []byte, fault uint8) {
		if len(ops) > 256 {
			ops = ops[:256]
		}
		if len(ops) == 0 {
			ops = []byte{0}
		}
		fault %= 5

		records := make([]string, 0, len(ops)+3)
		rootCount := 0
		for index, operation := range ops {
			parent := "null"
			if index > 0 && operation&3 != 0 {
				parent = fmt.Sprintf("%q", fmt.Sprintf("node-%d", int(operation)%index))
			} else {
				rootCount++
			}
			records = append(records, propertyForestEntry(fmt.Sprintf("node-%d", index), parent))
		}

		var wantError error
		switch fault {
		case 1:
			records = append(records, propertyForestEntry("node-0", "null"))
			wantError = ErrInvalidEntry
		case 2:
			records = append(records,
				propertyForestEntry("forward", `"future"`),
				propertyForestEntry("future", "null"),
			)
			wantError = ErrUnsupportedTree
		case 3:
			records = append(records, propertyForestEntry("broken", `"missing"`))
			wantError = ErrUnsupportedTree
		case 4:
			records = append(records, propertyForestEntry("cycle", `"cycle"`))
			wantError = ErrUnsupportedTree
		}

		data := []byte(testHeader + "\n" + strings.Join(records, "\n") + "\n")
		before := bytes.Clone(data)
		header, entries, byID, _, err := decodeSessionFile("property.jsonl", data)
		if !bytes.Equal(data, before) {
			t.Fatal("decoder mutated source bytes")
		}
		if wantError != nil {
			if !errors.Is(err, wantError) {
				t.Fatalf("fault %d error = %v, want %v", fault, err, wantError)
			}
			return
		}
		if err != nil {
			t.Fatalf("valid append-only forest rejected: %v", err)
		}
		if len(entries) != len(ops) || len(byID) != len(ops) {
			t.Fatalf("decoded graph sizes = entries:%d index:%d, want %d", len(entries), len(byID), len(ops))
		}
		session := &Session{header: header, entries: entries, byID: byID, leaf: len(entries) - 1}
		if roots := session.Tree(); len(roots) != rootCount {
			t.Fatalf("forest roots = %d, want %d", len(roots), rootCount)
		}
		for _, entry := range entries {
			path, err := session.PathTo(entry.ID())
			if err != nil || len(path) == 0 || path[len(path)-1].ID() != entry.ID() {
				t.Fatalf("PathTo(%q) = %v, %v", entry.ID(), entryIDs(path), err)
			}
			if _, hasParent := path[0].ParentID(); hasParent {
				t.Fatalf("PathTo(%q) does not begin at a root", entry.ID())
			}
		}
	})
}

func propertyForestEntry(id, parentJSON string) string {
	return fmt.Sprintf(`{"type":"property","id":%q,"parentId":%s,"timestamp":"2026-08-01T00:00:00.000Z","payload":{"preserve":true}}`, id, parentJSON)
}

func userEntryJSON(text, id, parent string, second int) string {
	return `{"type":"message","id":"` + id + `","parentId":` + parent + `,"timestamp":"2026-08-01T00:00:0` + string(rune('0'+second)) + `.000Z","message":{"role":"user","content":[{"type":"text","text":"` + text + `"}],"timestamp":178554240` + string(rune('0'+second)) + `000}}`
}

func assistantEntryJSON(text, id, parent string, second int) string {
	return `{"type":"message","id":"` + id + `","parentId":` + parent + `,"timestamp":"2026-08-01T00:00:0` + string(rune('0'+second)) + `.000Z","message":{"role":"assistant","content":[{"type":"text","text":"` + text + `"}],"api":"scripted","provider":"scripted","model":"scripted-1","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"stop","timestamp":178554240` + string(rune('0'+second)) + `000}}`
}

func assistantReplayEntryJSON(id, parent, provider, api, model, content, extra string) string {
	return fmt.Sprintf(
		`{"type":"message","id":%q,"parentId":%s,"timestamp":"2026-08-01T00:00:01.000Z","message":{"role":"assistant","content":%s,"api":%q,"provider":%q,"model":%q,"usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"stop"%s,"timestamp":1785542401000}}`,
		id, parent, content, api, provider, model, extra,
	)
}

func assistantBlocks(message llm.ConversationMessage) []llm.AssistantBlock {
	switch message := message.(type) {
	case llm.AssistantTextMessage:
		return message.Blocks()
	case llm.AssistantRichMessage:
		return message.Blocks()
	case llm.AssistantToolUseMessage:
		return message.Blocks()
	default:
		return nil
	}
}

func assertUnsignedReplayProjection(t *testing.T, context Context, wantDiagnostics int) {
	t.Helper()
	messages := context.Messages()
	if len(messages) != 2 {
		t.Fatalf("messages = %#v, want two assistants", messages)
	}
	for messageIndex, message := range messages {
		for blockIndex, block := range assistantBlocks(message) {
			switch block := block.(type) {
			case llm.TextBlock:
				if replay, ok := block.TextReplay(); ok {
					t.Fatalf("message %d block %d projected untrusted text replay %#v", messageIndex, blockIndex, replay)
				}
			case llm.ThinkingBlock:
				if replay, ok := block.OpenAIResponsesReplay(); ok {
					t.Fatalf("message %d block %d projected untrusted reasoning replay %#v", messageIndex, blockIndex, replay)
				}
			}
		}
	}
	if diagnostics := context.Diagnostics(); len(diagnostics) != wantDiagnostics {
		t.Fatalf("diagnostics = %#v, want %d", diagnostics, wantDiagnostics)
	}
}

func writeSessionFixture(t *testing.T, data string) string {
	t.Helper()
	return writeSessionFixtureBytes(t, []byte(data))
}

func writeSessionFixtureBytes(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.jsonl")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
