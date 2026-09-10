package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/session"
)

func TestDeferHistoryMediaPreservesImageReferences(t *testing.T) {
	message := json.RawMessage(`{
		"role":"toolResult",
		"content":[
			{"type":"text","text":"kept"},
			{"type":"image","data":"YWJjZA==","mimeType":"image/png"},
			{"type":"image","source":{"type":"base64","data":"YWJj","media_type":"image/jpeg"}},
			{"type":"image","source":{"type":"url","url":"https://example.invalid/image.png"}}
		]
	}`)
	deferred, err := deferHistoryMedia(message, false, true, "entry-1")
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(deferred, &value); err != nil {
		t.Fatal(err)
	}
	if len(value.Content) != 4 || value.Content[0]["text"] != "kept" {
		t.Fatalf("content = %s", deferred)
	}
	for index, size := range []float64{4, 3} {
		block := value.Content[index+1]
		ref, _ := block["imageRef"].(map[string]any)
		if block["type"] != "image" || block["byteSize"] != size || ref["entryId"] != "entry-1" || ref["blockIndex"] != float64(index+1) {
			t.Fatalf("reference = %#v", block)
		}
		if block["data"] != nil || block["source"] != nil {
			t.Fatalf("image bytes leaked: %s", deferred)
		}
	}
	if value.Content[3]["source"].(map[string]any)["type"] != "url" {
		t.Fatalf("URL image changed: %s", deferred)
	}

}

func TestNormalizeHistoryToolCallsMatchesPiWeb(t *testing.T) {
	message := json.RawMessage(`{
		"role":"assistant",
		"content":[
			{"type":"text","text":"before"},
			{"type":"toolCall","id":"call-1","name":"read","arguments":{"path":"go.mod","limit":1},"thoughtSignature":"private"},
			{"type":"toolCall","toolCallId":"call-2","toolName":"edit","input":{"path":"README.md"}}
		]
	}`)
	normalized, err := normalizeHistoryToolCalls(message)
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(normalized, &value); err != nil {
		t.Fatal(err)
	}
	if len(value.Content) != 3 {
		t.Fatalf("content = %#v", value.Content)
	}
	first := value.Content[1]
	input, _ := first["input"].(map[string]any)
	if first["toolCallId"] != "call-1" || first["toolName"] != "read" || input["path"] != "go.mod" {
		t.Fatalf("normalized legacy tool call = %#v", first)
	}
	if _, exists := first["name"]; exists {
		t.Fatalf("raw tool call fields leaked into Web view: %#v", first)
	}
	second := value.Content[2]
	if second["toolCallId"] != "call-2" || second["toolName"] != "edit" {
		t.Fatalf("normalized Web tool call = %#v", second)
	}
}

func TestEntryToUIMessageProjectsDurableToolCallToWebContract(t *testing.T) {
	manager, err := session.InMemorySessionManager(t.TempDir(), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	call, err := llm.NewToolCallBlock("call-read", "read", []byte(`{"path":"go.mod","limit":1}`))
	if err != nil {
		t.Fatal(err)
	}
	message, err := llm.NewAssistantToolUseMessage(
		[]llm.AssistantBlock{call}, llm.Usage{}, time.UnixMilli(1),
		llm.AssistantProvenance{Provider: "deepseek", API: "openai-completions", Model: "deepseek-v4-flash"},
	)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := manager.AppendLLMMessage(context.Background(), message)
	if err != nil {
		t.Fatal(err)
	}
	projected, ok, err := entryToUIMessage(entry, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("durable assistant message was omitted")
	}
	var value struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(projected, &value); err != nil {
		t.Fatal(err)
	}
	if len(value.Content) != 1 {
		t.Fatalf("content = %#v", value.Content)
	}
	block := value.Content[0]
	input, _ := block["input"].(map[string]any)
	if block["toolCallId"] != "call-read" || block["toolName"] != "read" || input["path"] != "go.mod" {
		t.Fatalf("projected tool call = %#v", block)
	}
}

func TestProjectedSessionTreeFlattensBeyondClientDepthLimit(t *testing.T) {
	manager, err := session.InMemorySessionManager(t.TempDir(), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	appendMessage := func(label string, timestamp int64) session.Entry {
		message, messageErr := llm.NewUserTextMessage(label, time.UnixMilli(timestamp))
		if messageErr != nil {
			t.Fatal(messageErr)
		}
		entry, appendErr := manager.AppendLLMMessage(context.Background(), message)
		if appendErr != nil {
			t.Fatal(appendErr)
		}
		return entry
	}

	main := appendMessage("root", 1)
	for depth := 2; depth <= maxProjectedTreeDepth+5; depth++ {
		if err := manager.Branch(main.ID()); err != nil {
			t.Fatal(err)
		}
		next := appendMessage(fmt.Sprintf("main-%d", depth), int64(depth*2))
		if err := manager.Branch(main.ID()); err != nil {
			t.Fatal(err)
		}
		_ = appendMessage(fmt.Sprintf("side-%d", depth), int64(depth*2+1))
		if err := manager.Branch(next.ID()); err != nil {
			t.Fatal(err)
		}
		main = next
	}

	projected, err := projectSessionTree(manager.Tree())
	if err != nil {
		t.Fatal(err)
	}
	if len(projected) != 1 {
		t.Fatalf("projected roots = %d", len(projected))
	}
	node := projected[0]
	for depth := 1; depth < maxProjectedTreeDepth; depth++ {
		if len(node.Children) < 1 {
			t.Fatalf("tree ended at depth %d", depth)
		}
		node = node.Children[0]
	}
	if len(node.Children) < 2 {
		t.Fatalf("depth-capped node has %d children", len(node.Children))
	}
	for _, child := range node.Children {
		if len(child.Children) != 0 {
			t.Fatal("descendant beyond the depth cap was nested instead of flattened")
		}
		if len(child.Entry) == 0 || !strings.Contains(string(child.Entry), `"type":"message"`) {
			t.Fatalf("invalid flattened entry: %s", child.Entry)
		}
	}
}
