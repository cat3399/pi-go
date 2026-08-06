package session

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
)

func parityMessageEntry(t *testing.T, id string, message llm.ConversationMessage) Entry {
	t.Helper()
	wrapper, err := agentmsg.NewLLM(message)
	if err != nil {
		t.Fatal(err)
	}
	return Entry{id: id, typeName: "message", message: message, payload: MessagePayload{Message: wrapper}}
}

func parityAssistantText(t *testing.T, text string, at time.Time) llm.AssistantTextMessage {
	t.Helper()
	message, err := newAssistantTextMessage([]llm.TextBlock{mustTextBlock(t, text)}, llm.FinishStop, llm.Usage{}, at)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func TestPrepareCompactionSplitsAssistantTurnExactlyLikePi(t *testing.T) {
	at := time.UnixMilli(1)
	entries := []Entry{
		parityMessageEntry(t, "u1", mustUserMessage(t, "u111", at)),
		parityMessageEntry(t, "a1", parityAssistantText(t, "a111", at)),
		parityMessageEntry(t, "u2", mustUserMessage(t, "u222", at)),
		parityMessageEntry(t, "a2", parityAssistantText(t, "a222", at)),
		parityMessageEntry(t, "a3", parityAssistantText(t, "a333", at)),
	}
	settings := CompactionSettings{Enabled: false, ReserveTokens: 101, KeepRecentTokens: 2}
	preparation, err := prepareCompaction(entries, settings)
	if err != nil {
		t.Fatal(err)
	}
	if preparation.firstKeptID != "a2" || !preparation.isSplitTurn || preparation.settings != settings {
		t.Fatalf("preparation cut/settings = %#v", preparation)
	}
	history, err := agentmsg.ConvertToLLM(preparation.messages)
	if err != nil {
		t.Fatal(err)
	}
	prefix, err := agentmsg.ConvertToLLM(preparation.turnPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if got := messageTexts(history); !reflect.DeepEqual(got, []string{"u111", "a111"}) {
		t.Fatalf("history = %q", got)
	}
	if got := messageTexts(prefix); !reflect.DeepEqual(got, []string{"u222"}) {
		t.Fatalf("turn prefix = %q", got)
	}
	if got := messageTexts(preparation.retained); !reflect.DeepEqual(got, []string{"a222", "a333"}) {
		t.Fatalf("retained = %q", got)
	}
	prompt, err := summarizePrompt(preparation.messages, "prior checkpoint", "focus")
	if err != nil {
		t.Fatal(err)
	}
	for _, exact := range []string{
		"<conversation>\n[User]: u111\n\n[Assistant]: a111\n</conversation>",
		"<previous-summary>\nprior checkpoint\n</previous-summary>",
		"Additional focus: focus",
	} {
		if !strings.Contains(prompt, exact) {
			t.Fatalf("summary prompt missing %q:\n%s", exact, prompt)
		}
	}
}

func TestFindCompactionCutNeverStartsAtToolResultAndKeepsAdjacentMetadata(t *testing.T) {
	at := time.UnixMilli(1)
	call, err := llm.NewToolCallBlock("call", "read", []byte(`{"path":"large.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := newAssistantToolUseMessage([]llm.AssistantBlock{call}, llm.Usage{}, at)
	if err != nil {
		t.Fatal(err)
	}
	result, err := llm.NewToolResultMessage("call", "read", []llm.TextBlock{mustTextBlock(t, strings.Repeat("x", 40))}, false, at)
	if err != nil {
		t.Fatal(err)
	}
	toolEntries := []Entry{
		parityMessageEntry(t, "user", mustUserMessage(t, "request", at)),
		parityMessageEntry(t, "assistant", assistant),
		parityMessageEntry(t, "result", result),
		parityMessageEntry(t, "recent", mustUserMessage(t, "z", at)),
	}
	cut, err := findCompactionCut(toolEntries, 0, len(toolEntries), 2)
	if err != nil {
		t.Fatal(err)
	}
	if cut.firstKeptEntryIndex != 3 || cut.isSplitTurn {
		t.Fatalf("tool-result cut = %#v", cut)
	}

	metadataEntries := []Entry{
		parityMessageEntry(t, "old", mustUserMessage(t, "old", at)),
		{id: "metadata", typeName: "thinking_level_change", payload: ThinkingLevelChangePayload{ThinkingLevel: "high"}},
		parityMessageEntry(t, "recent", mustUserMessage(t, "z", at)),
	}
	cut, err = findCompactionCut(metadataEntries, 0, len(metadataEntries), 1)
	if err != nil {
		t.Fatal(err)
	}
	if cut.firstKeptEntryIndex != 1 || !cut.isSplitTurn || cut.turnStartIndex != 0 {
		t.Fatalf("metadata-adjacent cut = %#v", cut)
	}
}

func TestFindCompactionCutCountsPreviousCheckpointSummaryLikePi(t *testing.T) {
	at := time.UnixMilli(1)
	entries := []Entry{
		parityMessageEntry(t, "old", mustUserMessage(t, "old", at)),
		{id: "checkpoint", typeName: "compaction", timestamp: at, compaction: &CompactionRecord{
			Summary: strings.Repeat("s", 40), FirstKeptEntryID: "old",
		}},
		parityMessageEntry(t, "recent", mustUserMessage(t, "z", at)),
	}
	cut, err := findCompactionCut(entries, 0, len(entries), 5)
	if err != nil {
		t.Fatal(err)
	}
	if cut.firstKeptEntryIndex != 2 || cut.isSplitTurn {
		t.Fatalf("checkpoint-budget cut = %#v", cut)
	}
}

func TestFindCompactionCutBudgetsContextVisibleCustomEntryLikePi(t *testing.T) {
	at := time.UnixMilli(1)
	custom, err := agentmsg.NewCustomText("fixture", strings.Repeat("x", 4000), true, nil, at)
	if err != nil {
		t.Fatal(err)
	}
	entries := []Entry{
		parityMessageEntry(t, "user", mustUserMessage(t, "hi", at)),
		parityMessageEntry(t, "first-assistant", parityAssistantText(t, "hello", at)),
		{id: "custom", typeName: "custom_message", timestamp: at, payload: CustomMessagePayload{Message: custom}},
		parityMessageEntry(t, "last-assistant", parityAssistantText(t, "ok", at)),
	}
	tiny, err := findCompactionCut(entries, 0, len(entries), 1)
	if err != nil {
		t.Fatal(err)
	}
	if tiny.firstKeptEntryIndex != 3 || !tiny.isSplitTurn || tiny.turnStartIndex != 2 {
		t.Fatalf("tiny custom cut = %#v", tiny)
	}
	fits, err := findCompactionCut(entries, 0, len(entries), 2)
	if err != nil {
		t.Fatal(err)
	}
	if fits.firstKeptEntryIndex != 2 || fits.isSplitTurn || fits.turnStartIndex != -1 {
		t.Fatalf("fitting custom cut = %#v", fits)
	}
}

func TestCompactionAssistantEstimateAndSerializationIncludeAllOriginalBlocks(t *testing.T) {
	at := time.UnixMilli(1)
	thinking, err := llm.NewThinkingBlock("12345678")
	if err != nil {
		t.Fatal(err)
	}
	first := mustTextBlock(t, "first")
	second := mustTextBlock(t, "second")
	call, err := llm.NewToolCallBlock("call", "r", []byte(`{ "path" : "x" }`))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := newAssistantToolUseMessage([]llm.AssistantBlock{thinking, first, second, call}, llm.Usage{}, at)
	if err != nil {
		t.Fatal(err)
	}
	// thinking(8) + text(11) + name(1) + compact arguments(12) = 32 chars.
	if tokens, estimateErr := estimateMessageTokens(assistant); estimateErr != nil || tokens != 8 {
		t.Fatalf("assistant estimate = (%d, %v), want 8", tokens, estimateErr)
	}
	serialized := SerializeConversation([]llm.ConversationMessage{assistant})
	if !strings.Contains(serialized, "[Assistant thinking]: 12345678") ||
		!strings.Contains(serialized, "[Assistant]: first\nsecond") ||
		!strings.Contains(serialized, `[Assistant tool calls]: r(path="x")`) {
		t.Fatalf("assistant serialization = %q", serialized)
	}
}

func TestToolResultTruncationUsesJavaScriptUTF16Units(t *testing.T) {
	text := strings.Repeat("a", 1998) + "🙂" + "z"
	truncated := truncateToolResult(text, 2000)
	if !strings.HasPrefix(truncated, strings.Repeat("a", 1998)+"🙂") ||
		!strings.HasSuffix(truncated, "[... 1 more characters truncated]") {
		t.Fatalf("UTF-16 truncation = %q", truncated[len(truncated)-80:])
	}
}

func TestCompactionFileOperationsMatchPiSetsAndFinalDetails(t *testing.T) {
	at := time.UnixMilli(1)
	blocks := make([]llm.AssistantBlock, 0, 5)
	for index, spec := range []struct {
		name string
		path string
	}{
		{name: "read", path: "z.go"},
		{name: "read", path: "shared.go"},
		{name: "write", path: "shared.go"},
		{name: "edit", path: "m.go"},
		{name: "read", path: "a.go"},
	} {
		raw, err := json.Marshal(map[string]string{"path": spec.path})
		if err != nil {
			t.Fatal(err)
		}
		call, err := llm.NewToolCallBlock(string(rune('a'+index)), spec.name, raw)
		if err != nil {
			t.Fatal(err)
		}
		blocks = append(blocks, call)
	}
	assistant, err := newAssistantToolUseMessage(blocks, llm.Usage{}, at)
	if err != nil {
		t.Fatal(err)
	}
	entries := []Entry{
		parityMessageEntry(t, "old", mustUserMessage(t, "old", at)),
		parityMessageEntry(t, "tools", assistant),
		parityMessageEntry(t, "recent", mustUserMessage(t, "z", at)),
	}
	preparation, err := prepareCompaction(entries, CompactionSettings{Enabled: true, ReserveTokens: 100, KeepRecentTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(preparation.fileOperations.Read, []string{"z.go", "shared.go", "a.go"}) ||
		!reflect.DeepEqual(preparation.fileOperations.Written, []string{"shared.go"}) ||
		!reflect.DeepEqual(preparation.fileOperations.Edited, []string{"m.go"}) {
		t.Fatalf("file operations = %#v", preparation.fileOperations)
	}
	output, err := finalizeCompactionOutput(SummaryInput{FileOperations: preparation.fileOperations}, SummaryOutput{Text: "checkpoint"})
	if err != nil {
		t.Fatal(err)
	}
	wantText := "checkpoint\n\n<read-files>\na.go\nz.go\n</read-files>\n\n<modified-files>\nm.go\nshared.go\n</modified-files>"
	if output.Text != wantText || string(output.Details) != `{"readFiles":["a.go","z.go"],"modifiedFiles":["m.go","shared.go"]}` {
		t.Fatalf("final output = %#v", output)
	}
	extension := SummaryOutput{Text: "owned", Details: json.RawMessage(`{"owner":"extension"}`), FromExtension: true}
	unchanged, err := finalizeCompactionOutput(SummaryInput{FileOperations: preparation.fileOperations}, extension)
	if err != nil || unchanged.Text != extension.Text || string(unchanged.Details) != string(extension.Details) {
		t.Fatalf("extension output was finalized: %#v, %v", unchanged, err)
	}
}

func TestCompactionEstimatesJavaScriptUTF16AndInheritsPiDetails(t *testing.T) {
	message := mustUserMessage(t, "你好🙂", time.UnixMilli(1))
	if tokens, err := estimateMessageTokens(message); err != nil || tokens != 1 {
		t.Fatalf("UTF-16 token estimate = (%d, %v), want 1", tokens, err)
	}

	previous := Entry{payload: CompactionPayload{
		Details: json.RawMessage(`{"readFiles":["old-read","shared"],"modifiedFiles":["old-mod","shared"]}`),
	}}
	readCall, err := llm.NewToolCallBlock("read", "read", []byte(`{"path":"new-read"}`))
	if err != nil {
		t.Fatal(err)
	}
	writeCall, err := llm.NewToolCallBlock("write", "write", []byte(`{"path":"old-read"}`))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := newAssistantToolUseMessage([]llm.AssistantBlock{readCall, writeCall}, llm.Usage{}, time.UnixMilli(2))
	if err != nil {
		t.Fatal(err)
	}
	wrapper, err := agentmsg.NewLLM(assistant)
	if err != nil {
		t.Fatal(err)
	}
	operations := extractFileOperations([]agentmsg.Message{wrapper}, []Entry{previous}, 0)
	if !reflect.DeepEqual(operations.Read, []string{"old-read", "shared", "new-read"}) ||
		!reflect.DeepEqual(operations.Edited, []string{"old-mod", "shared"}) ||
		!reflect.DeepEqual(operations.Written, []string{"old-read"}) {
		t.Fatalf("inherited operations = %#v", operations)
	}
	if details := ComputeFileLists(operations); !reflect.DeepEqual(details, CompactionDetails{
		ReadFiles: []string{"new-read"}, ModifiedFiles: []string{"old-mod", "old-read", "shared"},
	}) {
		t.Fatalf("inherited details = %#v", details)
	}

	previous.payload = CompactionPayload{Details: previous.payload.(CompactionPayload).Details, FromHook: true}
	ignored := extractFileOperations(nil, []Entry{previous}, 0)
	if len(ignored.Read) != 0 || len(ignored.Edited) != 0 || len(ignored.Written) != 0 {
		t.Fatalf("extension details were inherited: %#v", ignored)
	}
}
