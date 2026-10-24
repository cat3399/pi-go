package surfacewire

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/session"
)

type resourceReadAPI struct {
	application.API
	manager  *session.SessionManager
	revision uint64
}

func (a *resourceReadAPI) ReadHistory(id string, query session.HistoryQuery) (application.HistorySnapshot, error) {
	page, err := a.manager.ReadHistory(query)
	return application.HistorySnapshot{Revision: a.revision, SessionID: id, Page: page}, err
}
func (a *resourceReadAPI) ReadNodes(id string, query session.NodeQuery) (application.NodesSnapshot, error) {
	page, err := a.manager.ReadNodes(query)
	return application.NodesSnapshot{Revision: a.revision, SessionID: id, Page: page}, err
}

func TestFixedHistoryAndNodeTokensIgnoreUnrelatedRevisions(t *testing.T) {
	manager, err := session.InMemorySessionManager(t.TempDir(), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	appendUser := func(text string) session.Entry {
		t.Helper()
		message, err := llm.NewUserTextMessage(text, time.UnixMilli(1))
		if err != nil {
			t.Fatal(err)
		}
		entry, err := manager.AppendLLMMessage(context.Background(), message)
		if err != nil {
			t.Fatal(err)
		}
		return entry
	}
	first := appendUser("first")
	last := appendUser("last")
	api := &resourceReadAPI{manager: manager}
	id, anchor := manager.SessionID(), last.ID()
	value, _ := manager.ReadSnapshot(session.SnapshotOptions{})
	version := sessionVersion(value.Version)
	query := ReadResourceRequest{LeafID: &anchor, At: &version}
	history, err := ReadSessionResource(context.Background(), api, id, "history", query, "")
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := ReadSessionResource(context.Background(), api, id, "nodes", ReadResourceRequest{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Branch(first.ID()); err != nil {
		t.Fatal(err)
	}
	api.revision = 999 // other sessions and token events are not page versions
	unchangedNodes, err := ReadSessionResource(context.Background(), api, id, "nodes", ReadResourceRequest{}, nodes.Token)
	if err != nil || !unchangedNodes.Unchanged {
		t.Fatalf("leaf invalidated nodes: %+v, %v", unchangedNodes, err)
	}
	appendUser("new branch")
	unchanged, err := ReadSessionResource(context.Background(), api, id, "history", query, history.Token)
	if err != nil || !unchanged.Unchanged {
		t.Fatalf("append invalidated fixed page: %+v, %v", unchanged, err)
	}
	current, err := ReadSessionResource(context.Background(), api, id, "history", ReadResourceRequest{}, history.Token)
	if err != nil || current.Unchanged {
		t.Fatalf("new path reused old token: %v", err)
	}
	var page HistoryPage
	if json.Unmarshal(current.Data, &page) != nil || len(page.Items) != 2 || page.Items[0].Node.ID != first.ID() || page.Items[1].Node.ID == last.ID() {
		t.Fatalf("new path = %s", current.Data)
	}
}

func TestHistoryPageKeepsLargeTextAndThinkingInline(t *testing.T) {
	manager, err := session.InMemorySessionManager(t.TempDir(), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	text, thinking := strings.Repeat("完整正文", 4000), strings.Repeat("完整思考", 4000)
	textBlock, _ := llm.NewTextBlock(text)
	thinkingBlock, _ := llm.NewThinkingBlock(thinking)
	message, err := llm.NewAssistantRichMessage([]llm.AssistantBlock{thinkingBlock, textBlock}, llm.FinishStop, llm.Usage{}, time.UnixMilli(1), llm.AssistantProvenance{Provider: "test", API: "test", Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	value, err := ReadSessionResource(context.Background(), &resourceReadAPI{manager: manager}, manager.SessionID(), "history", ReadResourceRequest{MaxBytes: 4096}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(value.Data, []byte(text)) || !bytes.Contains(value.Data, []byte(thinking)) || bytes.Contains(value.Data, []byte(`"deferred"`)) || bytes.Contains(value.Data, []byte(`"thinkingRef"`)) {
		t.Fatal("large message text or thinking was deferred/truncated")
	}
}
