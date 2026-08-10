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

func TestDeferHistoryMediaMatchesPiWebPlaceholder(t *testing.T) {
	message := json.RawMessage(`{
		"role":"toolResult",
		"content":[
			{"type":"text","text":"kept"},
			{"type":"image","data":"YWJjZA==","mimeType":"image/png"},
			{"type":"image","source":{"type":"base64","data":"YWJj","media_type":"image/jpeg"}},
			{"type":"image","source":{"type":"url","url":"https://example.invalid/image.png"}}
		]
	}`)
	deferred, err := deferHistoryMedia(message, false, true)
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		Content []struct {
			Type   string         `json:"type"`
			Text   string         `json:"text"`
			Source map[string]any `json:"source"`
		} `json:"content"`
	}
	if err := json.Unmarshal(deferred, &value); err != nil {
		t.Fatal(err)
	}
	if len(value.Content) != 3 || value.Content[0].Text != "kept" || value.Content[1].Source["type"] != "url" {
		t.Fatalf("deferred content = %#v", value.Content)
	}
	const want = "[2 tool result images omitted from initial history payload: image/png, image/jpeg, ~7 bytes]"
	if value.Content[2].Text != want {
		t.Fatalf("placeholder = %q, want %q", value.Content[2].Text, want)
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
