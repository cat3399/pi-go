package agent_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

func TestReadIdentitySurvivesMessageEndReplacementAndNamesActualEntry(t *testing.T) {
	manager := newSessionManager(t)
	var runtime *agent.AgentSession
	var hookIDs []string
	replacement, _ := agentmsg.NewLLM(mustTextTerminal(t, "replacement"))
	var err error
	runtime, err = agent.NewSession(agent.SessionConfig{
		Provider:       newScriptedProvider(t, mustTextTerminal(t, "same"), mustTextTerminal(t, "same")),
		SessionManager: manager, Model: sessionTestModel(t),
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		Hooks: agent.Hooks{Message: func(_ context.Context, event agent.MessageHookEvent) (agent.MessageHookResult, error) {
			if event.Type != agent.MessageEndHookEvent {
				return agent.MessageHookResult{}, nil
			}
			read, err := runtime.ReadSnapshot()
			if err != nil {
				return agent.MessageHookResult{}, err
			}
			if read.Agent.Streaming != nil || len(read.Agent.Pending) != 1 || read.Agent.Pending[0].ID == "" {
				t.Errorf("message_end hook lost pre-persist identity: %#v", read.Agent)
				return agent.MessageHookResult{}, nil
			}
			hookIDs = append(hookIDs, read.Agent.Pending[0].ID)
			if read.History.Version.Log != uint64(len(hookIDs)-1) {
				t.Errorf("hook observed early persistence: log=%d, hooks=%d", read.History.Version.Log, len(hookIDs))
			}
			if event.Message.Role() == agentmsg.RoleAssistant {
				return agent.MessageHookResult{Message: replacement}, nil
			}
			return agent.MessageHookResult{}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	starts := map[string]bool{}
	ended := map[string]bool{}
	var receipts []agent.MessageReceipt
	runtime.SubscribeObserved(func(_ context.Context, observed agent.SessionObservation) {
		switch observed.Event.(type) {
		case agent.MessageStartEvent:
			if observed.MessageID == "" || starts[observed.MessageID] {
				t.Errorf("message_start reused/missed identity: %#v", observed)
			}
			starts[observed.MessageID] = true
		case agent.MessageUpdateEvent:
			read, readErr := runtime.ReadSnapshot()
			if readErr != nil || read.Agent.Streaming == nil || read.Agent.Streaming.ID != observed.MessageID || read.Agent.Version != observed.Version {
				t.Errorf("streaming read/event mismatch: %#v, %v", read.Agent, readErr)
			}
		case agent.MessageEndEvent:
			if !starts[observed.MessageID] || len(hookIDs) == 0 || hookIDs[len(hookIDs)-1] != observed.MessageID {
				t.Errorf("hook replacement changed identity: %#v", observed)
			}
			ended[observed.MessageID] = true
		}
		if receipt := observed.Receipt; receipt != nil {
			if !ended[receipt.MessageID] || !receipt.Accepted || receipt.Uncertain {
				t.Errorf("receipt preceded end or was not accepted: %#v", receipt)
			}
			entry, exists := manager.Entry(receipt.EntryID)
			if !exists || entry.ID() == "" || receipt.Version.Log != uint64(len(receipts)+1) {
				t.Errorf("receipt does not name actual append: %#v", receipt)
			}
			read, readErr := runtime.ReadSnapshot()
			if readErr != nil || len(read.Agent.Pending) != 0 || read.Agent.Version != observed.Version || read.History.Version != receipt.Version {
				t.Errorf("acknowledged message remained pending: %#v, %v", read, readErr)
			}
			receipts = append(receipts, *receipt)
		}
	})
	for range 2 {
		if result, err := runtime.Run(context.Background(), "same"); err != nil || !result.Succeeded() {
			t.Fatalf("run = %#v, %v", result, err)
		}
	}
	if len(starts) != 4 || len(receipts) != 4 || receipts[0].Durable || !receipts[1].Durable {
		t.Fatalf("identical text/timestamps or delayed flush lost identity: starts=%v receipts=%#v", starts, receipts)
	}
	for i, receipt := range receipts {
		if i%2 == 1 {
			entry, _ := manager.Entry(receipt.EntryID)
			message, _ := entry.Message()
			if got := message.(llm.AssistantTextMessage).Content()[0].Text(); got != "replacement" {
				t.Fatalf("receipt points to unprocessed assistant %q", got)
			}
		}
	}
}

func TestReadReceiptReportsAcceptedButUnflushedAssistant(t *testing.T) {
	manager := newSessionManager(t)
	file, _ := manager.SessionFile()
	if err := os.Mkdir(file, 0o700); err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, mustTextTerminal(t, "cannot flush")), SessionManager: manager,
		Model: sessionTestModel(t), SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	var receipts []agent.MessageReceipt
	runtime.SubscribeObserved(func(_ context.Context, observed agent.SessionObservation) {
		if observed.Receipt != nil {
			receipts = append(receipts, *observed.Receipt)
		}
	})
	_, _ = runtime.Run(context.Background(), "user")
	if len(receipts) < 2 || !receipts[1].Accepted || receipts[1].Durable || receipts[1].EntryID == "" {
		t.Fatalf("flush failure concealed visible entry: %#v", receipts)
	}
	read, err := runtime.ReadSnapshot()
	if err != nil || len(read.Agent.Pending) != 0 || read.History.Version.Log != uint64(len(receipts)) {
		t.Fatalf("flush failure duplicated pending history: %#v, %v", read, err)
	}
}

func TestReadStateRestoresCurrentToolProgressWithoutHistoryCopy(t *testing.T) {
	manager := newSessionManager(t)
	definition, _ := provider.NewToolDefinition("fixture", "fixture", false, []byte(`{"type":"object"}`))
	var runtime *agent.AgentSession
	updateRead := make(chan struct{})
	tool := &fakeTool{name: "fixture", execute: func(ctx context.Context, _ []byte, report func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		report(agent.ToolUpdate{Text: "half", Details: map[string]any{"step": "one"}})
		select {
		case <-updateRead:
		case <-ctx.Done():
			return agent.ToolOutput{}, context.Cause(ctx)
		}
		return agent.ToolOutput{Text: "done"}, nil
	}}
	var err error
	runtime, err = agent.NewSession(agent.SessionConfig{
		Provider:       newScriptedProvider(t, mustToolUseTerminal(t, "call", "fixture", []byte(`{}`)), mustTextTerminal(t, "finished")),
		SessionManager: manager, Model: sessionTestModel(t), Tool: tool, Tools: []provider.ToolDefinition{definition},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime.SubscribeObserved(func(_ context.Context, observed agent.SessionObservation) {
		if _, ok := observed.Event.(agent.ToolExecutionUpdateEvent); !ok {
			return
		}
		defer close(updateRead)
		read, err := runtime.ReadSnapshot()
		if err != nil {
			t.Error(err)
			return
		}
		if len(read.Agent.ToolsRunning) != 1 || read.Agent.ToolsRunning[0].ToolCallID != "call" || read.Agent.ToolsRunning[0].Update == nil || read.Agent.ToolsRunning[0].Update.Text != "half" {
			t.Errorf("lost current tool progress: %#v", read.Agent.ToolsRunning)
		} else {
			read.Agent.ToolsRunning[0].Update.Details.(map[string]any)["step"] = "mutated"
			again, _ := runtime.ReadSnapshot()
			if again.Agent.ToolsRunning[0].Update.Details.(map[string]any)["step"] != "one" {
				t.Error("read snapshot mutated active tool progress")
			}
		}
		if len(read.Agent.Pending) != 0 || read.Agent.Streaming != nil || read.Agent.MessageCount != 2 || read.History.Entries != nil || read.History.Tree != nil {
			t.Errorf("live read copied settled history: %#v", read)
		}
	})
	before, _ := runtime.ReadSnapshot()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if result, err := runtime.Run(ctx, "go"); err != nil || !result.Succeeded() {
		t.Fatalf("run = %#v, %v", result, err)
	}
	after, err := runtime.ReadSnapshot()
	if err != nil || len(after.Agent.ToolsRunning) != 0 || after.Agent.Version.State <= before.Agent.Version.State || after.Agent.Version.Configuration != before.Agent.Version.Configuration || after.Agent.Version.Queue != before.Agent.Version.Queue {
		t.Fatalf("token/tool events invalidated configuration or queue: %#v, %#v, %v", before.Agent.Version, after.Agent.Version, err)
	}
}

func TestReadStateNeverSplitsMessageAcknowledgementFromHistory(t *testing.T) {
	manager, err := session.InMemorySessionManager(t.TempDir(), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	responses := make([]llm.AssistantTerminal, 20)
	for i := range responses {
		responses[i] = mustTextTerminal(t, "same")
	}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, responses...), SessionManager: manager, Model: sessionTestModel(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			read, err := runtime.ReadSnapshot()
			if err != nil {
				t.Error(err)
				return
			}
			if read.History.Version.Log+uint64(len(read.Agent.Pending)) != uint64(read.Agent.MessageCount) {
				t.Errorf("history/ack straddled append: log=%d pending=%d messages=%d", read.History.Version.Log, len(read.Agent.Pending), read.Agent.MessageCount)
				return
			}
		}
	})
	for range responses {
		if result, err := runtime.Run(context.Background(), "same"); err != nil || !result.Succeeded() {
			t.Errorf("run = %#v, %v", result, err)
			break
		}
	}
	close(done)
	wg.Wait()
}

func TestReadStateVersionsSeparateConfigurationQueuesAndConversationReset(t *testing.T) {
	runtime, err := agent.New(agent.Config{Provider: newScriptedProvider(t, mustTextTerminal(t, "same"), mustTextTerminal(t, "same")), Model: sessionTestModel(t)})
	if err != nil {
		t.Fatal(err)
	}
	initial := runtime.ReadSnapshot()
	if initial.Version != runtime.ReadSnapshot().Version {
		t.Fatal("reads changed the resource version")
	}
	if err := runtime.SetSystemPrompt("new prompt"); err != nil {
		t.Fatal(err)
	}
	configured := runtime.ReadSnapshot()
	if configured.Version.Configuration <= initial.Version.Configuration || configured.Version.Queue != initial.Version.Queue {
		t.Fatalf("configuration version = %#v", configured.Version)
	}
	if err := runtime.Steer("queued"); err != nil {
		t.Fatal(err)
	}
	queued := runtime.ReadSnapshot()
	if queued.Version.Configuration != configured.Version.Configuration || queued.Version.Queue <= configured.Version.Queue || len(queued.Steering) != 1 {
		t.Fatalf("queue version = %#v", queued)
	}
	runtime.ClearAllQueues()
	if _, err := runtime.Run(context.Background(), "same"); err != nil {
		t.Fatal(err)
	}
	before := runtime.ReadSnapshot()
	if len(before.Pending) != 2 {
		t.Fatalf("standalone Agent lost unacknowledged identities: %#v", before)
	}
	runtime.Reset()
	if read := runtime.ReadSnapshot(); len(read.Pending) != 0 || read.Streaming != nil || read.MessageCount != 0 {
		t.Fatalf("reset retained stale live messages: %#v", read)
	}
	if _, err := runtime.Run(context.Background(), "same"); err != nil {
		t.Fatal(err)
	}
	after := runtime.ReadSnapshot()
	if len(after.Pending) != 2 || after.Pending[0].ID == before.Pending[0].ID || after.Version.Generation != before.Version.Generation {
		t.Fatalf("reset reused a transient identity: before=%#v after=%#v", before, after)
	}
}
