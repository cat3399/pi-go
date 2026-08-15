package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/session"
)

type countingItemRenderer struct {
	calls int
}

func (r *countingItemRenderer) CacheKey() string { return "counting" }
func (r *countingItemRenderer) Render(item contentItem, _ int) []string {
	r.calls++
	return []string{item.ID + ":0", item.ID + ":1", item.ID + ":2"}
}

func TestTranscriptVirtualizesAndCachesOffscreenItems(t *testing.T) {
	items := make([]contentItem, 1000)
	for index := range items {
		items[index] = contentItem{ID: fmt.Sprintf("item-%04d", index), Revision: 1, Title: "item"}
	}
	model := newTranscriptModel()
	model.SetItems(items)
	renderer := &countingItemRenderer{}

	first := model.View(80, 12, renderer)
	if !strings.Contains(first, "item-0999") {
		t.Fatalf("tail view does not include newest item:\n%s", first)
	}
	if renderer.calls > 5 {
		t.Fatalf("initial render touched %d items, want only the visible tail", renderer.calls)
	}
	calls := renderer.calls
	if second := model.View(80, 12, renderer); second != first {
		t.Fatalf("cached view changed:\nfirst=%s\nsecond=%s", first, second)
	}
	if renderer.calls != calls {
		t.Fatalf("cached view rendered %d additional items", renderer.calls-calls)
	}

	model.View(60, 12, renderer)
	if renderer.calls == calls {
		t.Fatal("width change did not invalidate visible render cache")
	}
}

func TestTranscriptAnchorSurvivesLiveAppend(t *testing.T) {
	items := make([]contentItem, 12)
	for index := range items {
		items[index] = contentItem{ID: fmt.Sprintf("item-%02d", index), Revision: 1, Title: "item"}
	}
	model := newTranscriptModel()
	model.SetItems(items)
	renderer := &countingItemRenderer{}
	_ = model.View(80, 8, renderer)
	model.ScrollUp(7)
	before := model.View(80, 8, renderer)
	if model.Following() {
		t.Fatal("ScrollUp left transcript following the tail")
	}

	model.Upsert(contentItem{ID: "item-12", Revision: 1, Title: "new"})
	after := model.View(80, 8, renderer)
	if after != before {
		t.Fatalf("append moved anchored viewport:\nbefore=%s\nafter=%s", before, after)
	}
	model.ScrollToBottom()
	if bottom := model.View(80, 8, renderer); !strings.Contains(bottom, "item-12") {
		t.Fatalf("bottom view does not include appended item:\n%s", bottom)
	}
}

func TestContentProjectionPreservesRichMessageSemantics(t *testing.T) {
	now := time.Unix(1700000000, 0)
	text, err := llm.NewTextBlock("hello")
	if err != nil {
		t.Fatal(err)
	}
	image, err := llm.NewImageDataBlock("image/png", []byte{1, 2, 3, 4})
	if err != nil {
		t.Fatal(err)
	}
	user, err := llm.NewUserContentMessage([]llm.UserContentBlock{text, image}, now)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := agentmsg.NewLLM(user)
	if err != nil {
		t.Fatal(err)
	}
	item, ok := contentItemFromAgentMessage("entry", 1, wrapped, false)
	if !ok || item.Role != contentRoleUser || len(item.Blocks) != 2 {
		t.Fatalf("item = %#v, ok=%t", item, ok)
	}
	if item.Blocks[0].Kind != contentBlockText || item.Blocks[0].Text != "hello" {
		t.Fatalf("text block = %#v", item.Blocks[0])
	}
	if item.Blocks[1].Kind != contentBlockImage || item.Blocks[1].MediaType != "image/png" || item.Blocks[1].ByteSize != 4 {
		t.Fatalf("image block = %#v", item.Blocks[1])
	}
}

func TestContentProjectionDoesNotDisplayHiddenCustomMessages(t *testing.T) {
	now := time.Unix(1700000000, 0)
	hidden, err := agentmsg.NewCustomText("internal", "secret", false, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := contentItemFromAgentMessage("hidden", 1, hidden, false); ok {
		t.Fatal("hidden custom message was projected")
	}
	visible, err := agentmsg.NewCustomText("notice", "shown", true, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	item, ok := contentItemFromAgentMessage("visible", 1, visible, false)
	if !ok || item.Title != "notice" || len(item.Blocks) != 1 || item.Blocks[0].Text != "shown" {
		t.Fatalf("visible item = %#v, ok=%t", item, ok)
	}
}

func TestSnapshotProjectionJoinsDurableToolCallAndResult(t *testing.T) {
	manager, err := session.InMemorySessionManager(t.TempDir(), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	call, err := llm.NewToolCallBlock("durable-call", "read", []byte(`{"path":"a.go"}`))
	if err != nil {
		t.Fatal(err)
	}
	usage, err := llm.NewUsage(llm.UsageSpec{})
	if err != nil {
		t.Fatal(err)
	}
	provenance := llm.AssistantProvenance{Provider: "test", API: "test", Model: "test"}
	assistant, err := llm.NewAssistantToolUseMessage([]llm.AssistantBlock{call}, usage, time.UnixMilli(1), provenance)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), assistant); err != nil {
		t.Fatal(err)
	}
	output, err := llm.NewTextBlock("durable output")
	if err != nil {
		t.Fatal(err)
	}
	result, err := llm.NewToolResultMessageWithDetails(
		"durable-call", "read", []llm.TextBlock{output}, false, time.UnixMilli(2), json.RawMessage(`{"kept":true}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), result); err != nil {
		t.Fatal(err)
	}

	items := contentItemsFromSnapshot(application.SessionSnapshot{Entries: manager.Entries()})
	if len(items) != 1 || len(items[0].Blocks) != 2 {
		t.Fatalf("projected items = %#v", items)
	}
	if items[0].Blocks[0].Kind != contentBlockToolCall || items[0].Blocks[1].Kind != contentBlockToolResult || items[0].Blocks[1].Text != "durable output" {
		t.Fatalf("projected transaction = %#v", items[0])
	}
	if string(items[0].Blocks[1].ToolDetails) != `{"kept":true}` {
		t.Fatalf("projected details = %s", items[0].Blocks[1].ToolDetails)
	}
}
