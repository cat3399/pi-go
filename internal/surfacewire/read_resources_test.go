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

func (a *resourceReadAPI) ReadRuns(id string, query session.RunsQuery) (application.RunsSnapshot, error) {
	page, err := a.manager.ReadRuns(query)
	return application.RunsSnapshot{Revision: a.revision, SessionID: id, Page: page}, err
}

func (a *resourceReadAPI) ReadSessionMetadata(id string) (application.SessionSnapshot, error) {
	value, err := a.manager.ReadSnapshot(session.SnapshotOptions{})
	return application.SessionSnapshot{SessionID: id, Version: value.Version, LeafID: value.LeafID, LatestUsage: value.LatestUsage}, err
}

func TestMetadataCarriesUsageOfAnAssistantHiddenInsideTheProcess(t *testing.T) {
	manager, err := session.InMemorySessionManager(t.TempDir(), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	user, _ := llm.NewUserTextMessage("question", time.UnixMilli(1))
	if _, err := manager.AppendLLMMessage(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	call, _ := llm.NewToolCallBlock("call", "read", []byte(`{}`))
	usage, _ := llm.NewUsage(llm.UsageSpec{Input: 30, Output: 10, CacheRead: 90})
	assistant, _ := llm.NewAssistantToolUseMessage([]llm.AssistantBlock{call}, usage, time.UnixMilli(2), llm.AssistantProvenance{Provider: "fixture", API: "scripted", Model: "test"})
	entry, err := manager.AppendLLMMessage(context.Background(), assistant)
	if err != nil {
		t.Fatal(err)
	}
	api := &resourceReadAPI{manager: manager}
	response, err := ReadSessionResource(context.Background(), api, manager.SessionID(), "metadata", ReadResourceRequest{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		LatestUsage struct {
			EntryID   string                                                 `json:"entryId"`
			Timestamp int64                                                  `json:"timestamp"`
			Provider  string                                                 `json:"provider"`
			Usage     struct{ Input, Output, CacheRead, TotalTokens uint64 } `json:"usage"`
		} `json:"latestUsage"`
	}
	if err := json.Unmarshal(response.Data, &value); err != nil {
		t.Fatal(err)
	}
	got := value.LatestUsage
	if got.EntryID != entry.ID() || got.Timestamp != 2 || got.Provider != "fixture" || got.Usage.Input != 30 || got.Usage.Output != 10 || got.Usage.CacheRead != 90 || got.Usage.TotalTokens != 130 {
		t.Fatalf("metadata lost response usage or identity: %s", response.Data)
	}
	if bytes.Contains(response.Data, []byte(`"content"`)) || bytes.Contains(response.Data, []byte(`"toolCall"`)) {
		t.Fatal("usage metadata copied process content")
	}
}

func TestRunOverviewDefersProcessAndDetailKeepsCompleteEntryAndGroupHeader(t *testing.T) {
	manager, err := session.InMemorySessionManager(t.TempDir(), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	user, _ := llm.NewUserTextMessage("original question", time.UnixMilli(1))
	header, err := manager.AppendLLMMessage(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	thinking, _ := llm.NewThinkingBlock("thinking-start\n" + strings.Repeat("完整思考\n", 60000) + "thinking-end")
	text, _ := llm.NewTextBlock("complete final answer")
	assistant, err := llm.NewAssistantRichMessage([]llm.AssistantBlock{thinking, text}, llm.FinishStop, llm.Usage{}, time.UnixMilli(2), llm.AssistantProvenance{API: "scripted", Provider: "scripted", Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	final, err := manager.AppendLLMMessage(context.Background(), assistant)
	if err != nil {
		t.Fatal(err)
	}
	api := &resourceReadAPI{manager: manager}
	overview, err := ReadSessionResource(context.Background(), api, manager.SessionID(), "runs", ReadResourceRequest{AroundID: header.ID()}, "")
	if err != nil {
		t.Fatal(err)
	}
	var page RunsPage
	if err := json.Unmarshal(overview.Data, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Runs) != 1 || len(page.Items) != 2 || page.Runs[0].Header.ID != header.ID() || page.Runs[0].Result.ID != final.ID() || !page.Runs[0].ResultHasProcess {
		t.Fatalf("overview lost group context: %s", overview.Data)
	}
	if len(overview.Data) > 10_000 || bytes.Contains(overview.Data, []byte("thinking-start")) || !bytes.Contains(page.Items[1].Message, []byte("complete final answer")) || page.Items[1].Projection != "answer" {
		t.Fatal("overview eagerly fetched hidden process or truncated its answer")
	}
	detail, err := ReadSessionResource(context.Background(), api, manager.SessionID(), "history", ReadResourceRequest{RunID: header.ID(), Limit: 1, MaxBytes: 512 << 10}, "")
	if err != nil {
		t.Fatal(err)
	}
	var full HistoryPage
	if err := json.Unmarshal(detail.Data, &full); err != nil {
		t.Fatal(err)
	}
	if full.Run == nil || full.Run.Header.ID != header.ID() || full.Run.Result.ID != final.ID() || len(full.Items) != 1 || full.Items[0].Node.ID != final.ID() || full.Newer != "" {
		t.Fatal("partial process page lost its group header or crossed the boundary")
	}
	if !bytes.Contains(full.Items[0].Message, []byte("thinking-start")) || !bytes.Contains(full.Items[0].Message, []byte("thinking-end")) || full.Items[0].Projection != "" || full.Items[0].Node.ContentToken != page.Items[1].Node.ContentToken {
		t.Fatal("detail projection did not retain the complete original entry identity")
	}
	index, err := ReadSessionResource(context.Background(), api, manager.SessionID(), "runs", ReadResourceRequest{MetadataOnly: true}, "")
	if err != nil || bytes.Contains(index.Data, []byte(`"message":`)) {
		t.Fatal("run navigation downloaded a message body", err)
	}
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
