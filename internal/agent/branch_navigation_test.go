package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

type recordingBranchSummarizer struct {
	mu      sync.Mutex
	inputs  []session.BranchSummaryInput
	output  session.BranchSummaryOutput
	entered chan struct{}
	block   bool
}

type retryingBranchSummarizer struct{}

func (retryingBranchSummarizer) SummarizeBranch(context.Context, session.BranchSummaryInput) (session.BranchSummaryOutput, error) {
	return session.BranchSummaryOutput{Text: "branch retry complete"}, nil
}

func (retryingBranchSummarizer) SummarizeBranchWithRetryObserver(ctx context.Context, _ session.BranchSummaryInput, observe provider.RetryObserver) (session.BranchSummaryOutput, error) {
	observe(ctx, provider.RetryEvent{Kind: provider.RetryScheduled, Attempt: 2, MaxAttempts: 3, Delay: time.Millisecond})
	observe(ctx, provider.RetryEvent{Kind: provider.RetryAttempt, Attempt: 2, MaxAttempts: 3})
	observe(ctx, provider.RetryEvent{Kind: provider.RetryFinished, Attempt: 2, MaxAttempts: 3, Succeeded: true, FinishReason: provider.RetryFinishSucceeded})
	return session.BranchSummaryOutput{Text: "branch retry complete"}, nil
}

func (s *recordingBranchSummarizer) SummarizeBranch(ctx context.Context, input session.BranchSummaryInput) (session.BranchSummaryOutput, error) {
	s.mu.Lock()
	s.inputs = append(s.inputs, input)
	entered, block := s.entered, s.block
	s.mu.Unlock()
	if entered != nil {
		select {
		case <-entered:
		default:
			close(entered)
		}
	}
	if block {
		<-ctx.Done()
		return session.BranchSummaryOutput{Aborted: true}, nil
	}
	return s.output, nil
}

func branchFixture(t *testing.T, manager *session.SessionManager) (first, common, target, alternate, oldLeaf session.Entry) {
	t.Helper()
	appendUser := func(text string, at int64) session.Entry {
		message, err := llm.NewUserTextMessage(text, time.UnixMilli(at))
		if err != nil {
			t.Fatal(err)
		}
		entry, err := manager.AppendLLMMessage(context.Background(), message)
		if err != nil {
			t.Fatal(err)
		}
		return entry
	}
	appendAssistant := func(text string) session.Entry {
		entry, err := manager.AppendLLMMessage(context.Background(), mustTextTerminal(t, text))
		if err != nil {
			t.Fatal(err)
		}
		return entry
	}
	first = appendUser("first", 1)
	common = appendAssistant("common")
	target = appendUser("edit this", 2)
	if err := manager.Branch(common.ID()); err != nil {
		t.Fatal(err)
	}
	alternate = appendUser("alternate work", 3)
	oldLeaf = appendAssistant("alternate answer")
	return
}

func branchModel(t *testing.T) provider.Model {
	t.Helper()
	model, err := newAgentModel(provider.ModelSpec{Provider: "scripted", API: "scripted", ID: "model", ContextWindow: 128_000, MaxTokens: 8_192})
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func branchUsage(t *testing.T, input, output uint64) *session.CompactionUsage {
	t.Helper()
	usage := mustUsage(t, input, output)
	return &session.CompactionUsage{Usage: usage, Cost: session.UsageCostFromLLM(usage.Cost())}
}

func TestNavigateTreePlacesUserTargetInEditorWithoutAddingEntries(t *testing.T) {
	manager := newSessionManager(t)
	_, common, target, _, _ := branchFixture(t, manager)
	before := len(manager.Entries())
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: newScriptedProvider(t), SessionManager: manager, Model: branchModel(t)})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.NavigateTree(context.Background(), target.ID(), agent.NavigateTreeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.EditorText == nil || *result.EditorText != "edit this" || result.SummaryEntry != nil {
		t.Fatalf("result = %#v", result)
	}
	leaf, ok := manager.LeafID()
	if !ok || leaf != common.ID() || len(manager.Entries()) != before {
		t.Fatalf("leaf=%q ok=%v entries=%d want leaf=%q entries=%d", leaf, ok, len(manager.Entries()), common.ID(), before)
	}
}

func TestNavigateTreeDefaultSummaryMatchesBranchPlacementAndPersistence(t *testing.T) {
	manager := newSessionManager(t)
	_, common, target, alternate, oldLeaf := branchFixture(t, manager)
	summarizer := &recordingBranchSummarizer{output: session.BranchSummaryOutput{Text: "structured branch", Usage: branchUsage(t, 7, 3)}}
	var before agent.TreePreparation
	var after agent.SessionTreeEvent
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: manager, Model: branchModel(t), BranchSummarizer: summarizer,
		Hooks: agent.Hooks{
			SessionBeforeTree: func(_ context.Context, event agent.SessionBeforeTreeEvent) (agent.SessionBeforeTreeResult, error) {
				before = event.Preparation
				return agent.SessionBeforeTreeResult{}, nil
			},
			SessionTree: func(_ context.Context, event agent.SessionTreeEvent) error { after = event; return nil },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	custom := "preserve commands"
	result, err := runtime.NavigateTree(context.Background(), target.ID(), agent.NavigateTreeOptions{Summarize: true, CustomInstructions: &custom})
	if err != nil {
		t.Fatal(err)
	}
	if result.EditorText == nil || *result.EditorText != "edit this" || result.SummaryEntry == nil {
		t.Fatalf("result = %#v", result)
	}
	if before.CommonAncestorID == nil || *before.CommonAncestorID != common.ID() || before.OldLeafID == nil || *before.OldLeafID != oldLeaf.ID() || len(before.EntriesToSummarize) != 2 || before.EntriesToSummarize[0].ID() != alternate.ID() {
		t.Fatalf("preparation = %#v", before)
	}
	if len(summarizer.inputs) != 1 || !strings.Contains(summarizer.inputs[0].Prompt, "Additional focus: preserve commands") || summarizer.inputs[0].MaxTokens != 2048 {
		t.Fatalf("summary inputs = %#v", summarizer.inputs)
	}
	payload, ok := result.SummaryEntry.Payload().(session.BranchSummaryPayload)
	if !ok || payload.FromID != common.ID() || payload.FromHook || !payload.HasFromHook || !strings.HasPrefix(payload.Summary, session.BranchSummaryPreamble+"structured branch") || payload.Usage == nil {
		t.Fatalf("summary payload = %#v", result.SummaryEntry.Payload())
	}
	var details session.BranchSummaryDetails
	if err := json.Unmarshal(payload.Details, &details); err != nil || details.ReadFiles == nil || details.ModifiedFiles == nil {
		t.Fatalf("details = %s err=%v", payload.Details, err)
	}
	leaf, ok := manager.LeafID()
	if !ok || leaf != result.SummaryEntry.ID() || after.SummaryEntry == nil || after.FromExtension == nil || *after.FromExtension {
		t.Fatalf("leaf=%q after=%#v", leaf, after)
	}
	path, _ := manager.SessionFile()
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := session.OpenSessionManager(path, manager.Cwd(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopenedEntry, ok := reopened.Entry(result.SummaryEntry.ID())
	if !ok {
		t.Fatal("branch summary missing after reopen")
	}
	reopenedPayload, ok := reopenedEntry.Payload().(session.BranchSummaryPayload)
	if !ok || reopenedPayload.Usage == nil || reopenedPayload.FromID != common.ID() {
		t.Fatalf("reopened payload = %#v", reopenedEntry.Payload())
	}
}

func TestNavigateTreeSummaryCanBeRootEntryWhenReeditingFirstUserMessage(t *testing.T) {
	manager := newSessionManager(t)
	firstMessage, err := llm.NewUserTextMessage("first draft", time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.AppendLLMMessage(context.Background(), firstMessage)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), mustTextTerminal(t, "abandoned answer")); err != nil {
		t.Fatal(err)
	}
	summarizer := &recordingBranchSummarizer{output: session.BranchSummaryOutput{Text: "root return"}}
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: newScriptedProvider(t), SessionManager: manager, Model: branchModel(t), BranchSummarizer: summarizer})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.NavigateTree(context.Background(), first.ID(), agent.NavigateTreeOptions{Summarize: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.EditorText == nil || *result.EditorText != "first draft" || result.SummaryEntry == nil {
		t.Fatalf("result=%#v", result)
	}
	if _, hasParent := result.SummaryEntry.ParentID(); hasParent {
		t.Fatalf("root summary unexpectedly has parent: %#v", result.SummaryEntry)
	}
	payload, ok := result.SummaryEntry.Payload().(session.BranchSummaryPayload)
	if !ok || payload.FromID != "root" {
		t.Fatalf("payload=%#v", result.SummaryEntry.Payload())
	}
}

func TestNavigateTreeExtensionSummaryAndCancellation(t *testing.T) {
	t.Run("extension summary", func(t *testing.T) {
		manager := newSessionManager(t)
		_, common, target, _, _ := branchFixture(t, manager)
		fromTree := false
		runtime, err := agent.NewSession(agent.SessionConfig{
			Provider: newScriptedProvider(t), SessionManager: manager, Model: branchModel(t),
			Hooks: agent.Hooks{SessionBeforeTree: func(context.Context, agent.SessionBeforeTreeEvent) (agent.SessionBeforeTreeResult, error) {
				return agent.SessionBeforeTreeResult{Summary: &agent.TreeSummary{Summary: "extension supplied", Details: json.RawMessage(`{"extension":true}`), Usage: branchUsage(t, 2, 1)}}, nil
			}, SessionTree: func(_ context.Context, event agent.SessionTreeEvent) error {
				fromTree = event.FromExtension != nil && *event.FromExtension
				return nil
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		label := "saved branch"
		result, err := runtime.NavigateTree(context.Background(), target.ID(), agent.NavigateTreeOptions{Summarize: true, Label: &label})
		if err != nil {
			t.Fatal(err)
		}
		payload := result.SummaryEntry.Payload().(session.BranchSummaryPayload)
		if payload.Summary != "extension supplied" || payload.FromID != common.ID() || !payload.FromHook || !fromTree || string(payload.Details) != `{"extension":true}` {
			t.Fatalf("payload=%#v fromTree=%v", payload, fromTree)
		}
		leaf, ok := manager.LeafEntry()
		labelPayload, labelOK := leaf.Payload().(session.LabelPayload)
		if !ok || !labelOK || labelPayload.TargetID != result.SummaryEntry.ID() || labelPayload.Label == nil || *labelPayload.Label != label {
			t.Fatalf("summary label leaf=%#v", leaf)
		}
	})

	t.Run("hook cancellation leaves tree unchanged", func(t *testing.T) {
		manager := newSessionManager(t)
		_, _, target, _, oldLeaf := branchFixture(t, manager)
		runtime, err := agent.NewSession(agent.SessionConfig{Provider: newScriptedProvider(t), SessionManager: manager, Model: branchModel(t), Hooks: agent.Hooks{
			SessionBeforeTree: func(context.Context, agent.SessionBeforeTreeEvent) (agent.SessionBeforeTreeResult, error) {
				cancel := true
				return agent.SessionBeforeTreeResult{Cancel: agent.HookCancel{Cancel: &cancel}}, nil
			},
		}})
		if err != nil {
			t.Fatal(err)
		}
		before := len(manager.Entries())
		result, err := runtime.NavigateTree(context.Background(), target.ID(), agent.NavigateTreeOptions{Summarize: true})
		if err != nil || !result.Cancelled || result.Aborted {
			t.Fatalf("result=%#v err=%v", result, err)
		}
		leaf, _ := manager.LeafID()
		if leaf != oldLeaf.ID() || len(manager.Entries()) != before {
			t.Fatalf("leaf=%q entries=%d", leaf, len(manager.Entries()))
		}
	})
}

func TestAbortBranchSummaryDoesNotMutateTree(t *testing.T) {
	manager := newSessionManager(t)
	_, _, target, _, oldLeaf := branchFixture(t, manager)
	summarizer := &recordingBranchSummarizer{entered: make(chan struct{}), block: true}
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: newScriptedProvider(t), SessionManager: manager, Model: branchModel(t), BranchSummarizer: summarizer})
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result agent.NavigateTreeResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := runtime.NavigateTree(context.Background(), target.ID(), agent.NavigateTreeOptions{Summarize: true})
		done <- outcome{result: result, err: err}
	}()
	select {
	case <-summarizer.entered:
	case <-time.After(time.Second):
		t.Fatal("summarizer was not entered")
	}
	if state := runtime.State(); state.Active.Phase() != agent.PhaseCompacting {
		t.Fatalf("branch summary phase = %s, want compacting", state.Active.Phase())
	}
	abortCtx, cancelAbort := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancelAbort()
	if err := runtime.Abort(abortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("agent Abort crossed branch-summary cancellation domain: %v", err)
	}
	select {
	case got := <-done:
		t.Fatalf("agent Abort settled branch summary: %#v", got)
	default:
	}
	runtime.AbortBranchSummary()
	select {
	case got := <-done:
		if got.err != nil || !got.result.Cancelled || !got.result.Aborted {
			t.Fatalf("result=%#v err=%v", got.result, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("navigateTree did not settle after abort")
	}
	leaf, _ := manager.LeafID()
	if leaf != oldLeaf.ID() {
		t.Fatalf("leaf=%q want %q", leaf, oldLeaf.ID())
	}
	if state := runtime.State(); state.Active.Phase() != agent.PhaseIdle {
		t.Fatalf("branch summary settled phase = %s", state.Active.Phase())
	}
}

func TestNavigateTreeRetryEventsCarryBranchSummarySource(t *testing.T) {
	manager := newSessionManager(t)
	_, _, target, _, _ := branchFixture(t, manager)
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: newScriptedProvider(t), SessionManager: manager, Model: branchModel(t), BranchSummarizer: retryingBranchSummarizer{}})
	if err != nil {
		t.Fatal(err)
	}
	var events []agent.SessionEvent
	unsubscribe := runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch event.(type) {
		case agent.SessionSummarizationRetryScheduledEvent, agent.SessionSummarizationRetryAttemptEvent, agent.SessionSummarizationRetryFinishedEvent:
			events = append(events, event)
		}
	})
	defer unsubscribe()
	if _, err := runtime.NavigateTree(context.Background(), target.ID(), agent.NavigateTreeOptions{Summarize: true}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("events=%#v", events)
	}
	scheduled := events[0].(agent.SessionSummarizationRetryScheduledEvent)
	attempt := events[1].(agent.SessionSummarizationRetryAttemptEvent)
	finished := events[2].(agent.SessionSummarizationRetryFinishedEvent)
	if attempt.Source != "branchSummary" || scheduled.Reason != agent.CompactionBranchSummary ||
		attempt.Reason != agent.CompactionBranchSummary || finished.Reason != agent.CompactionBranchSummary {
		t.Fatalf("retry events=%#v", events)
	}
}

func TestNavigateTreeRequiresModelBeforeTargetLookupWhenSummarizing(t *testing.T) {
	manager := newSessionManager(t)
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: newScriptedProvider(t), SessionManager: manager})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.NavigateTree(context.Background(), "missing", agent.NavigateTreeOptions{Summarize: true})
	if !errors.Is(err, agent.ErrNoModelSelected) || err.Error() != "No model available for summarization" {
		t.Fatalf("err=%v", err)
	}
}

func TestNavigateTreeResolvesAccessBeforeNoContentSummary(t *testing.T) {
	manager := newSessionManager(t)
	_, common, target, _, _ := branchFixture(t, manager)
	if err := manager.Branch(common.ID()); err != nil {
		t.Fatal(err)
	}
	label := "metadata-only abandoned branch"
	metadata, err := manager.AppendLabelChange(context.Background(), common.ID(), &label)
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("summary credentials unavailable")
	summarizer := &recordingBranchSummarizer{output: session.BranchSummaryOutput{Text: "must not run"}}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: manager, Model: branchModel(t), BranchSummarizer: summarizer,
		ValidateModelAccess: func(context.Context, provider.Model) error { return want },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.NavigateTree(context.Background(), target.ID(), agent.NavigateTreeOptions{Summarize: true})
	if !errors.Is(err, want) {
		t.Fatalf("NavigateTree() error=%v, want %v", err, want)
	}
	if len(summarizer.inputs) != 0 {
		t.Fatalf("summarizer ran before access failure: %#v", summarizer.inputs)
	}
	leaf, ok := manager.LeafID()
	if !ok || leaf != metadata.ID() {
		t.Fatalf("leaf=%q present=%t, want unchanged %q", leaf, ok, metadata.ID())
	}
}

func TestNavigateTreePrefersExplicitBranchSummarizerOverCompactionResolver(t *testing.T) {
	manager := newSessionManager(t)
	_, _, target, _, _ := branchFixture(t, manager)
	summarizer := &recordingBranchSummarizer{output: session.BranchSummaryOutput{Text: "branch-specific"}}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: manager, Model: branchModel(t), BranchSummarizer: summarizer,
		ResolveSummarizer: func(context.Context, agent.SummarizerResolveRequest) (session.Summarizer, error) {
			return nil, errors.New("compaction resolver must not handle explicit branch summarizer")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.NavigateTree(context.Background(), target.ID(), agent.NavigateTreeOptions{Summarize: true})
	if err != nil || result.SummaryEntry == nil || len(summarizer.inputs) != 1 {
		t.Fatalf("NavigateTree()=%#v, inputs=%d, err=%v", result, len(summarizer.inputs), err)
	}
}
