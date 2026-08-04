package agentmsg_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
)

func TestCustomStringAndRichFormsCannotDiverge(t *testing.T) {
	text := "string"
	if _, err := agentmsg.NewCustom(agentmsg.CustomSpec{CustomType: "fixture", StringContent: &text, Content: []llm.UserContentBlock{mustText(t, "different")}, At: time.UnixMilli(1)}); err == nil {
		t.Fatal("conflicting custom content was accepted")
	}
	value, err := agentmsg.NewCustom(agentmsg.CustomSpec{CustomType: "fixture", StringContent: &text, At: time.UnixMilli(1)})
	if err != nil {
		t.Fatal(err)
	}
	if content := value.Content(); len(content) != 1 || content[0].(llm.TextBlock).Text() != text {
		t.Fatalf("canonical custom content = %#v", content)
	}
	if got, stringForm := value.StringContent(); !stringForm || got != text {
		t.Fatalf("string form = (%q, %t)", got, stringForm)
	}
	details := json.RawMessage(`{"kept":true}`)
	rich := []llm.UserContentBlock{mustText(t, "rich")}
	immutable, err := agentmsg.NewCustom(agentmsg.CustomSpec{CustomType: "fixture", Content: rich, Details: details, At: time.UnixMilli(2)})
	if err != nil {
		t.Fatal(err)
	}
	rich[0] = mustText(t, "mutated source")
	details[2] = 'x'
	returnedContent := immutable.Content()
	returnedContent[0] = mustText(t, "mutated accessor")
	returnedDetails := immutable.Details()
	returnedDetails[2] = 'y'
	if got := immutable.Content()[0].(llm.TextBlock).Text(); got != "rich" || string(immutable.Details()) != `{"kept":true}` {
		t.Fatalf("immutable custom = %q / %s", got, immutable.Details())
	}
}

func TestOpaqueMessageUsesDurableEnvelopeAsSoleIdentity(t *testing.T) {
	raw := json.RawMessage(`{"role":"futureRole","timestamp":7,"value":{"kept":true}}`)
	value, err := agentmsg.NewOpaque(agentmsg.OpaqueSpec{Type: "futureRole", Data: raw})
	if err != nil {
		t.Fatal(err)
	}
	raw[0] = '['
	if value.Role() != agentmsg.Role("futureRole") || value.Timestamp() != time.UnixMilli(7) || !strings.HasPrefix(string(value.Data()), `{"role"`) {
		t.Fatalf("opaque identity/data = %#v", value)
	}
	returned := value.Data()
	returned[0] = '['
	if !strings.HasPrefix(string(value.Data()), `{"role"`) {
		t.Fatal("opaque data accessor leaked mutable storage")
	}
	projected, err := agentmsg.ConvertToLLM([]agentmsg.Message{value})
	if err != nil || len(projected) != 0 {
		t.Fatalf("opaque default projection = %#v, %v", projected, err)
	}
	if _, err := agentmsg.NewOpaque(agentmsg.OpaqueSpec{Type: "other", Data: json.RawMessage(`{"role":"futureRole","timestamp":7}`)}); err == nil {
		t.Fatal("mismatched opaque type/role was accepted")
	}
	if _, err := agentmsg.NewOpaque(agentmsg.OpaqueSpec{Type: "futureRole", Data: json.RawMessage(`{"role":"futureRole"}`)}); err == nil {
		t.Fatal("opaque message without durable timestamp was accepted")
	}
	if _, err := agentmsg.NewOpaque(agentmsg.OpaqueSpec{Type: "futureRole", Data: json.RawMessage(`{"role":"futureRole","timestamp":7}`), At: time.UnixMilli(8)}); err == nil {
		t.Fatal("mismatched opaque timestamp was accepted")
	}
}

func TestConvertToLLMPreservesPiCodingMessageSemantics(t *testing.T) {
	at := time.UnixMilli(123)
	image, err := llm.NewImageDataBlock("image/png", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	custom, err := agentmsg.NewCustom(agentmsg.CustomSpec{CustomType: "artifact", Content: []llm.UserContentBlock{mustText(t, "inspect"), image}, Display: true, Details: json.RawMessage(`{"private":true}`), At: at})
	if err != nil {
		t.Fatal(err)
	}
	bash, err := agentmsg.NewBashExecution(agentmsg.BashExecution{Command: "git status", Output: "clean", At: at})
	if err != nil {
		t.Fatal(err)
	}
	branch, err := agentmsg.NewBranchSummary(agentmsg.BranchSummary{FromID: "abc", Summary: "tried another path", At: at})
	if err != nil {
		t.Fatal(err)
	}
	compact, err := agentmsg.NewCompactionSummary(agentmsg.CompactionSummary{Summary: "history", TokensBefore: 80, At: at})
	if err != nil {
		t.Fatal(err)
	}
	excluded, err := agentmsg.NewBashExecution(agentmsg.BashExecution{Command: "secret", Output: "hidden", ExcludeFromContext: true, At: at})
	if err != nil {
		t.Fatal(err)
	}
	messages, err := agentmsg.ConvertToLLM([]agentmsg.Message{bash, custom, branch, compact, excluded})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 {
		t.Fatalf("messages=%d, want 4", len(messages))
	}
	if got := messages[0].(llm.UserTextMessage).Content()[0].Text(); got != "Ran `git status`\n```\nclean\n```" {
		t.Fatalf("bash projection=%q", got)
	}
	content := messages[1].(llm.UserContentMessage).Content()
	if len(content) != 2 {
		t.Fatalf("custom rich content=%#v", content)
	}
	if got := messages[2].(llm.UserTextMessage).Content()[0].Text(); got != agentmsg.BranchSummaryPrefix+"tried another path"+agentmsg.BranchSummarySuffix {
		t.Fatalf("branch projection=%q", got)
	}
	if got := messages[3].(llm.UserTextMessage).Content()[0].Text(); got != agentmsg.CompactionSummaryPrefix+"history"+agentmsg.CompactionSummarySuffix {
		t.Fatalf("compaction projection=%q", got)
	}
}

func mustText(t *testing.T, text string) llm.TextBlock {
	t.Helper()
	value, err := llm.NewTextBlock(text)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
