package agentruntime

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
)

func TestConvertToLLMWithBlockImagesChecksDynamicallyAndPreservesDurableMetadata(t *testing.T) {
	text, err := llm.NewTextBlock("before")
	if err != nil {
		t.Fatal(err)
	}
	placeholder, err := llm.NewTextBlock(blockedImagePlaceholder)
	if err != nil {
		t.Fatal(err)
	}
	image, err := llm.NewImageDataBlock("image/png", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	user, err := llm.NewUserContentMessage(
		[]llm.UserContentBlock{text, placeholder, image, image}, time.UnixMilli(1),
	)
	if err != nil {
		t.Fatal(err)
	}
	usage, err := llm.NewUsage(llm.UsageSpec{Input: 7, Output: 3})
	if err != nil {
		t.Fatal(err)
	}
	toolResult, err := llm.NewToolResultContentMessageWithMetadata(
		"call-1", "probe", []llm.ToolResultContentBlock{text, image, image}, false, time.UnixMilli(2),
		llm.ToolResultMetadata{
			Details: json.RawMessage(`{"value":true}`), Usage: &usage,
			AddedToolNames: []string{"deferred"}, HasAddedToolNames: true,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	original := []llm.ConversationMessage{user, toolResult}
	blocked := false
	baseCalls := 0
	convert := convertToLLMWithBlockImages(
		func(context.Context, []agentmsg.Message) ([]llm.ConversationMessage, error) {
			baseCalls++
			return append([]llm.ConversationMessage(nil), original...), nil
		},
		func() bool { return blocked },
	)
	unblocked, err := convert(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(unblocked, original) {
		t.Fatalf("unblocked conversion = %#v", unblocked)
	}
	blocked = true
	filtered, err := convert(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if baseCalls != 2 {
		t.Fatalf("base converter calls = %d", baseCalls)
	}
	filteredUser, ok := filtered[0].(llm.UserContentMessage)
	if !ok {
		t.Fatalf("filtered user type = %T", filtered[0])
	}
	userContent := filteredUser.Content()
	if len(userContent) != 2 || userContent[0].(llm.TextBlock).Text() != "before" ||
		userContent[1].(llm.TextBlock).Text() != blockedImagePlaceholder {
		t.Fatalf("filtered user content = %#v", userContent)
	}
	filteredTool, ok := filtered[1].(llm.ToolResultContentMessage)
	if !ok {
		t.Fatalf("filtered tool type = %T", filtered[1])
	}
	toolContent := filteredTool.Content()
	filteredUsage, hasUsage := filteredTool.Usage()
	if len(toolContent) != 2 || toolContent[1].(llm.TextBlock).Text() != blockedImagePlaceholder ||
		string(filteredTool.Details()) != `{"value":true}` || !hasUsage || filteredUsage.Input() != 7 ||
		!reflect.DeepEqual(filteredTool.AddedToolNames(), []string{"deferred"}) || !filteredTool.HasAddedToolNames() {
		t.Fatalf("filtered tool result = %#v", filteredTool)
	}
	if _, ok := user.Content()[2].(llm.ImageBlock); !ok {
		t.Fatal("filter mutated original user content")
	}
	if _, ok := toolResult.Content()[1].(llm.ImageBlock); !ok {
		t.Fatal("filter mutated original tool content")
	}
}
