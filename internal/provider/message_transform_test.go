package provider

import (
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

func TestTransformConversationMessagesRepairsToolCausalityBeforeDroppingFailure(t *testing.T) {
	t.Parallel()
	now := time.Unix(2_100_000_000, 0)
	firstUser, err := llm.NewUserTextMessage("run both", now)
	if err != nil {
		t.Fatal(err)
	}
	firstCall, err := llm.NewToolCallBlock("call-a", "read", []byte(`{"path":"a"}`))
	if err != nil {
		t.Fatal(err)
	}
	secondCall, err := llm.NewToolCallBlock("call-b", "write", []byte(`{"path":"b"}`))
	if err != nil {
		t.Fatal(err)
	}
	provenance := llm.AssistantProvenance{Provider: OpenAIProviderID, API: OpenAIResponsesAPI, Model: "fixture"}
	toolUse, err := llm.NewAssistantToolUseMessage([]llm.AssistantBlock{firstCall, secondCall}, llm.Usage{}, now, provenance)
	if err != nil {
		t.Fatal(err)
	}
	realText, err := llm.NewTextBlock("written")
	if err != nil {
		t.Fatal(err)
	}
	realResult, err := llm.NewToolResultMessage(secondCall.ID(), secondCall.Name(), []llm.TextBlock{realText}, false, now)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := llm.NewAssistantFailureMessage(nil, llm.FinishError, "retryable failure", llm.Usage{}, now, provenance)
	if err != nil {
		t.Fatal(err)
	}
	nextUser, err := llm.NewUserTextMessage("continue", now)
	if err != nil {
		t.Fatal(err)
	}

	repaired, err := transformConversationMessages([]llm.ConversationMessage{firstUser, toolUse, realResult, failed, nextUser})
	if err != nil {
		t.Fatal(err)
	}
	if len(repaired) != 5 {
		t.Fatalf("repaired length = %d, want 5", len(repaired))
	}
	if _, ok := repaired[0].(llm.UserTextMessage); !ok {
		t.Fatalf("message 0 = %T, want user", repaired[0])
	}
	if _, ok := repaired[1].(llm.AssistantToolUseMessage); !ok {
		t.Fatalf("message 1 = %T, want tool use", repaired[1])
	}
	preservedResult, ok := repaired[2].(llm.ToolResultMessage)
	if !ok || preservedResult.ToolCallID() != secondCall.ID() || preservedResult.IsError() {
		t.Fatalf("message 2 = %#v, want real result for second call", repaired[2])
	}
	if user, ok := repaired[4].(llm.UserTextMessage); !ok || len(user.Content()) != 1 || user.Content()[0].Text() != "continue" {
		t.Fatalf("message 4 = %#v, want next user", repaired[4])
	}
	synthetic, ok := repaired[3].(llm.ToolResultMessage)
	if !ok || synthetic.ToolCallID() != firstCall.ID() || synthetic.ToolName() != firstCall.Name() || !synthetic.IsError() {
		t.Fatalf("synthetic result = %#v", repaired[3])
	}
	if content := synthetic.Content(); len(content) != 1 || content[0].Text() != "No result provided" {
		t.Fatalf("synthetic content = %#v", content)
	}
	for _, message := range repaired {
		if _, ok := message.(llm.AssistantFailureMessage); ok {
			t.Fatal("failed assistant turn was replayed")
		}
	}
}
