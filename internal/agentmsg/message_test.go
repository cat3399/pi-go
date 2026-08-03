package agentmsg_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
)

func TestConvertToLLMPreservesPiCodingMessageSemantics(t *testing.T) {
	at := time.UnixMilli(123)
	image, err := llm.NewImageDataBlock("image/png", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	custom, err := agentmsg.NewCustom(agentmsg.Custom{CustomType: "artifact", Content: []llm.UserContentBlock{mustText(t, "inspect"), image}, Display: true, Details: json.RawMessage(`{"private":true}`), At: at})
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
