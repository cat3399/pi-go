package session

import (
	"bytes"
	stdcontext "context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

const testHeader = `{"type":"session","version":3,"id":"session-1","timestamp":"2026-08-01T00:00:00.000Z","cwd":"/workspace"}`

func TestOpenAppendPreservesUnknownRawPrefixAndProjectsKnownContent(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "unknown.jsonl")
	prefix := []byte(
		` {"type":"session","version":3,"id":"session-1","timestamp":"2026-08-01T00:00:00.000Z","cwd":"/workspace","future":{"keep":true}}` + "\n" +
			`{"type":"message","id":"entry-1","parentId":null,"timestamp":"2026-08-01T00:00:01.000Z","futureEntry":1,"message":{"role":"user","content":[{"type":"text","text":"first"},{"type":"image","data":"opaque","mimeType":"image/png"},{"type":"text","text":"second"}],"timestamp":1785542401000,"futureMessage":{"x":1}}}` + "\n" +
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
	user := messages[0].(llm.UserTextMessage)
	if got := user.Content(); len(got) != 2 || got[0].Text() != "first" || got[1].Text() != "second" {
		t.Fatalf("projected user content = %#v", got)
	}
	if got := messages[1].(llm.AssistantTextMessage).Content(); len(got) != 1 || got[0].Text() != "reply" {
		t.Fatalf("projected assistant content = %#v", got)
	}
	diagnostics := context.Diagnostics()
	if len(diagnostics) != 3 {
		t.Fatalf("diagnostics = %#v, want unknown image/entry/thinking", diagnostics)
	}
	if diagnostics[0].Code != DiagnosticUnknownContentBlock || diagnostics[1].Code != DiagnosticUnknownEntry || diagnostics[2].Code != DiagnosticUnknownContentBlock {
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
		{name: "multiple roots", data: []byte(testHeader + "\n" + validRoot + "\n" + userEntryJSON("root-2", "entry-2", "null", 2) + "\n"), want: ErrUnsupportedTree},
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
	f.Add([]byte("not-json"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _, _, _ = decodeSessionFile("fuzz.jsonl", data)
	})
}

func userEntryJSON(text, id, parent string, second int) string {
	return `{"type":"message","id":"` + id + `","parentId":` + parent + `,"timestamp":"2026-08-01T00:00:0` + string(rune('0'+second)) + `.000Z","message":{"role":"user","content":[{"type":"text","text":"` + text + `"}],"timestamp":178554240` + string(rune('0'+second)) + `000}}`
}

func assistantEntryJSON(text, id, parent string, second int) string {
	return `{"type":"message","id":"` + id + `","parentId":` + parent + `,"timestamp":"2026-08-01T00:00:0` + string(rune('0'+second)) + `.000Z","message":{"role":"assistant","content":[{"type":"text","text":"` + text + `"}],"api":"scripted","provider":"scripted","model":"scripted-1","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"stop","timestamp":178554240` + string(rune('0'+second)) + `000}}`
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
