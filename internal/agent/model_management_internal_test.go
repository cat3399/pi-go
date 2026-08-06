package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

func internalControlModel(t *testing.T, id string) provider.Model {
	t.Helper()
	value, err := provider.NewModel(provider.ModelSpec{
		Provider: "scripted", API: "scripted", ID: id, Name: id, Reasoning: true,
		Input: []provider.InputKind{provider.InputText}, ContextWindow: 16_000, MaxTokens: 1_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func internalControlSession(t *testing.T, model provider.Model, thinking provider.ThinkingLevel) *AgentSession {
	t.Helper()
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.InMemorySessionManager(t.TempDir(), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewSession(SessionConfig{
		Provider: implementation, SessionManager: manager, Model: model, ThinkingLevel: thinking,
		SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	return runtime
}

type internalRuntimeControlSummarizer struct{}

func (internalRuntimeControlSummarizer) Summarize(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
	return session.SummaryOutput{Text: "summary"}, nil
}

func (internalRuntimeControlSummarizer) SummarizeBranch(context.Context, session.BranchSummaryInput) (session.BranchSummaryOutput, error) {
	return session.BranchSummaryOutput{Text: "branch summary"}, nil
}

func TestSummaryResolversReadCurrentRetrySettingsAtInvocation(t *testing.T) {
	selected := internalControlModel(t, "summary")
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.InMemorySessionManager(t.TempDir(), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	enabled := false
	retry := RetryPolicy{MaxAttempts: 4, InitialDelay: 17 * time.Millisecond, MaxDelay: 2 * time.Second}
	var compactionRequests, branchRequests []SummarizerResolveRequest
	coordinator, err := NewSession(SessionConfig{
		Provider: implementation, SessionManager: manager, Model: selected, ContextWindow: 64_000,
		ResolveRuntimeSettings: func() RuntimeControlSettings {
			return RuntimeControlSettings{
				SteeringMode: QueueOneAtATime, FollowUpMode: QueueOneAtATime,
				AutoCompactionEnabled: true, AutoRetryEnabled: enabled, Retry: retry,
			}
		},
		ResolveSummarizer: func(_ context.Context, request SummarizerResolveRequest) (session.Summarizer, error) {
			compactionRequests = append(compactionRequests, request)
			return internalRuntimeControlSummarizer{}, nil
		},
		ResolveBranchSummarizer: func(_ context.Context, request SummarizerResolveRequest) (session.BranchSummarizer, error) {
			branchRequests = append(branchRequests, request)
			return internalRuntimeControlSummarizer{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(context.Background()) })

	if _, err := coordinator.resolveCompactionSummarizer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinator.resolveTreeSummarizer(context.Background()); err != nil {
		t.Fatal(err)
	}
	enabled = true
	if _, err := coordinator.resolveCompactionSummarizer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinator.resolveTreeSummarizer(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name, requests := range map[string][]SummarizerResolveRequest{"compaction": compactionRequests, "branch": branchRequests} {
		if len(requests) != 2 {
			t.Fatalf("%s requests = %d", name, len(requests))
		}
		if requests[0].Retry.MaxAttempts != 1 || requests[0].Retry.InitialDelay != retry.InitialDelay || requests[0].Retry.MaxDelay != retry.MaxDelay {
			t.Fatalf("%s disabled retry = %#v", name, requests[0].Retry)
		}
		if requests[1].Retry.MaxAttempts != retry.MaxAttempts || requests[1].Retry.InitialDelay != retry.InitialDelay ||
			requests[1].Retry.MaxDelay != retry.MaxDelay {
			t.Fatalf("%s enabled retry = %#v, want %#v", name, requests[1].Retry, retry)
		}
	}
}

func TestRuntimeControlPersistenceCallbacksAreGetterReentrant(t *testing.T) {
	selected := internalControlModel(t, "reentrant")
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.InMemorySessionManager(t.TempDir(), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var coordinator *AgentSession
	coordinator, err = NewSession(SessionConfig{
		Provider: implementation, SessionManager: manager, Model: selected,
		PersistSettings: func(context.Context, SettingsUpdate) (SettingsWriteResult, error) {
			_ = coordinator.SteeringMode()
			_ = coordinator.FollowUpMode()
			_ = coordinator.AutoCompactionEnabled()
			_ = coordinator.AutoRetryEnabled()
			return SettingsWriteResult{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(context.Background()) })

	done := make(chan error, 4)
	go func() { done <- coordinator.SetSteeringMode(QueueAll) }()
	go func() { done <- coordinator.SetFollowUpMode(QueueAll) }()
	go func() { done <- coordinator.SetAutoCompactionEnabled(false) }()
	go func() { done <- coordinator.SetAutoRetryEnabled(false) }()
	for range 4 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("runtime control setter deadlocked in reentrant persistence callback")
		}
	}
}

func TestRuntimeControlSettersUseSharedControlLinearization(t *testing.T) {
	selected := internalControlModel(t, "linearized-controls")
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.InMemorySessionManager(t.TempDir(), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var persistMu sync.Mutex
	var persisted QueueMode
	call := 0
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	coordinator, err := NewSession(SessionConfig{
		Provider: implementation, SessionManager: manager, Model: selected,
		PersistSettings: func(_ context.Context, update SettingsUpdate) (SettingsWriteResult, error) {
			persistMu.Lock()
			call++
			currentCall := call
			if update.SteeringMode != nil {
				persisted = *update.SteeringMode
			}
			persistMu.Unlock()
			switch currentCall {
			case 1:
				close(firstEntered)
				<-releaseFirst
			case 2:
				close(secondEntered)
			}
			return SettingsWriteResult{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(context.Background()) })

	firstDone := make(chan error, 1)
	go func() { firstDone <- coordinator.SetSteeringMode(QueueAll) }()
	<-firstEntered
	secondStarted := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondStarted)
		secondDone <- coordinator.SetSteeringMode(QueueOneAtATime)
	}()
	<-secondStarted
	for range 100 {
		runtime.Gosched()
	}
	select {
	case <-secondEntered:
		close(releaseFirst)
		t.Fatal("second settings write entered before the first persist+publish operation completed")
	default:
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	<-secondEntered
	persistMu.Lock()
	persistedFinal := persisted
	persistMu.Unlock()
	if persistedFinal != QueueOneAtATime || coordinator.SteeringMode() != QueueOneAtATime {
		t.Fatalf("linearized final state = persisted %s, runtime %s", persistedFinal, coordinator.SteeringMode())
	}
}

func TestEveryRuntimeControlSetterOwnsControlMuDuringPersistence(t *testing.T) {
	tests := []struct {
		name string
		set  func(*AgentSession) error
	}{
		{name: "steering", set: func(s *AgentSession) error { return s.SetSteeringMode(QueueAll) }},
		{name: "follow-up", set: func(s *AgentSession) error { return s.SetFollowUpMode(QueueAll) }},
		{name: "compaction", set: func(s *AgentSession) error { return s.SetAutoCompactionEnabled(false) }},
		{name: "retry", set: func(s *AgentSession) error { return s.SetAutoRetryEnabled(false) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selected := internalControlModel(t, test.name)
			implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{})
			if err != nil {
				t.Fatal(err)
			}
			manager, err := session.InMemorySessionManager(t.TempDir(), session.NewSessionOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var coordinator *AgentSession
			coordinator, err = NewSession(SessionConfig{
				Provider: implementation, SessionManager: manager, Model: selected,
				PersistSettings: func(context.Context, SettingsUpdate) (SettingsWriteResult, error) {
					if coordinator.controlMu.TryLock() {
						coordinator.controlMu.Unlock()
						t.Error("persistence ran outside controlMu")
					}
					return SettingsWriteResult{}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = coordinator.Close(context.Background()) })
			if err := test.set(coordinator); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPrepareCompactionSnapshotsCurrentAutoCompactionSetting(t *testing.T) {
	selected := internalControlModel(t, "dynamic-compaction")
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.InMemorySessionManager(t.TempDir(), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"old", "recent"} {
		message, messageErr := llm.NewUserTextMessage(text, time.UnixMilli(1))
		if messageErr != nil {
			t.Fatal(messageErr)
		}
		if _, appendErr := manager.AppendLLMMessage(context.Background(), message); appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	enabled := true
	coordinator, err := NewSession(SessionConfig{
		Provider: implementation, SessionManager: manager, Model: selected,
		KeepRecentTokens: 1, KeepRecentTokensSet: true,
		Summarizer: internalRuntimeControlSummarizer{},
		ResolveRuntimeSettings: func() RuntimeControlSettings {
			return RuntimeControlSettings{
				SteeringMode: QueueOneAtATime, FollowUpMode: QueueOneAtATime,
				AutoCompactionEnabled: enabled, AutoRetryEnabled: true,
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(context.Background()) })

	input, _, err := coordinator.prepareCompaction(context.Background(), "enabled snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if !input.Settings.Enabled {
		t.Fatal("prepared compaction did not capture enabled runtime setting")
	}
	enabled = false
	input, _, err = coordinator.prepareCompaction(context.Background(), "disabled snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if input.Settings.Enabled {
		t.Fatal("prepared compaction retained construction-time enabled value")
	}
}

func waitForSelectionWriter(t *testing.T, coordinator *AgentSession) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !coordinator.selectionMu.TryRLock() {
			return
		}
		coordinator.selectionMu.RUnlock()
		runtime.Gosched()
	}
	t.Fatal("selection publisher did not wait for the write lock")
}

func TestSelectionReadersWaitForCoupledAgentAndSessionSnapshot(t *testing.T) {
	a, b := internalControlModel(t, "a"), internalControlModel(t, "b")
	coordinator := internalControlSession(t, a, provider.ThinkingHigh)

	runCtx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	coordinator.lifecycleMu.Lock()
	coordinator.run = &sessionRun{ctx: runCtx, cancel: cancel, done: make(chan struct{}), phase: PhaseProvider}
	coordinator.lifecycleMu.Unlock()
	coordinator.loop.mu.Lock()
	coordinator.loop.active = &activeRun{id: 1, ctx: runCtx, cancel: cancel, done: make(chan struct{}), turn: 1, phase: PhaseProvider}
	coordinator.loop.mu.Unlock()
	t.Cleanup(func() {
		coordinator.lifecycleMu.Lock()
		coordinator.run = nil
		coordinator.lifecycleMu.Unlock()
		coordinator.loop.mu.Lock()
		coordinator.loop.active = nil
		coordinator.loop.mu.Unlock()
	})

	// Reproduce the exact old publication window: the low Agent already has
	// the new pair while AgentSession still has the old pair.
	coordinator.selectionMu.Lock()
	if err := coordinator.loop.SetModelAndThinking(b, provider.ThinkingLow); err != nil {
		coordinator.selectionMu.Unlock()
		t.Fatal(err)
	}

	type result struct {
		name     string
		state    SessionState
		turn     TurnSnapshot
		model    provider.Model
		thinking provider.ThinkingLevel
		err      error
	}
	started := make(chan struct{}, 4)
	results := make(chan result, 4)
	go func() {
		started <- struct{}{}
		results <- result{name: "state", state: coordinator.State()}
	}()
	go func() {
		started <- struct{}{}
		turn, err := coordinator.prepareTurn(context.Background(), TurnContext{})
		results <- result{name: "prepare", turn: turn, err: err}
	}()
	go func() {
		started <- struct{}{}
		results <- result{name: "model", model: coordinator.Model()}
	}()
	go func() {
		started <- struct{}{}
		results <- result{name: "thinking", thinking: coordinator.ThinkingLevel()}
	}()
	for range 4 {
		<-started
	}
	for range 100 {
		runtime.Gosched()
	}
	select {
	case early := <-results:
		coordinator.selectionMu.Unlock()
		t.Fatalf("%s reader escaped split publication: %#v", early.name, early)
	default:
	}

	coordinator.mu.Lock()
	coordinator.model, coordinator.hasModel, coordinator.thinkingLevel = b, true, provider.ThinkingLow
	coordinator.mu.Unlock()
	coordinator.selectionMu.Unlock()

	for range 4 {
		got := <-results
		if got.err != nil {
			t.Fatalf("%s reader error = %v", got.name, got.err)
		}
		switch got.name {
		case "state":
			if !got.state.Model.Equal(b) || got.state.ThinkingLevel != provider.ThinkingLow ||
				!got.state.Active.Model().Equal(b) || got.state.Active.ThinkingLevel() != provider.ThinkingLow {
				t.Fatalf("state mixed selection = %#v / %s/%s", got.state, got.state.Active.Model().ID(), got.state.Active.ThinkingLevel())
			}
		case "prepare":
			if !got.turn.Model.Equal(b) || got.turn.ThinkingLevel != provider.ThinkingLow {
				t.Fatalf("turn mixed selection = %s/%s", got.turn.Model.ID(), got.turn.ThinkingLevel)
			}
		case "model":
			if !got.model.Equal(b) {
				t.Fatalf("model getter = %s", got.model.ID())
			}
		case "thinking":
			if got.thinking != provider.ThinkingLow {
				t.Fatalf("thinking getter = %s", got.thinking)
			}
		}
	}
}

func TestSelectionPublishersSerializeSuccessAndCommitUnknown(t *testing.T) {
	a, b := internalControlModel(t, "a"), internalControlModel(t, "b")
	coordinator := internalControlSession(t, a, provider.ThinkingHigh)

	// Selection getters must not route through State(), which takes lifecycleMu
	// and is therefore not reentrant for lifecycle-owned internal paths.
	coordinator.lifecycleMu.Lock()
	gettersDone := make(chan struct{})
	go func() {
		selected, ok := coordinator.SelectedModel()
		if !ok || !selected.Equal(a) || coordinator.ThinkingLevel() != provider.ThinkingHigh {
			t.Errorf("selection getters = %s/%t/%s", selected.ID(), ok, coordinator.ThinkingLevel())
		}
		close(gettersDone)
	}()
	select {
	case <-gettersDone:
	case <-time.After(time.Second):
		coordinator.lifecycleMu.Unlock()
		t.Fatal("selection getters attempted to re-enter lifecycleMu")
	}
	coordinator.lifecycleMu.Unlock()

	modelAppended := make(chan struct{})
	coordinator.appendModelControl = func(context.Context, string, string, *string) ([]session.Entry, error) {
		close(modelAppended)
		return nil, nil
	}
	coordinator.selectionMu.RLock()
	modelDone := make(chan error, 1)
	go func() { modelDone <- coordinator.SetModel(b) }()
	<-modelAppended
	waitForSelectionWriter(t, coordinator)
	if state := coordinator.loop.State(); !state.Model().Equal(a) || state.ThinkingLevel() != provider.ThinkingHigh {
		coordinator.selectionMu.RUnlock()
		t.Fatalf("model publisher changed low Agent before lock admission: %s/%s", state.Model().ID(), state.ThinkingLevel())
	}
	coordinator.selectionMu.RUnlock()
	if err := <-modelDone; err != nil {
		t.Fatal(err)
	}
	if selected, ok := coordinator.SelectedModel(); !ok || !selected.Equal(b) || coordinator.ThinkingLevel() != provider.ThinkingHigh {
		t.Fatalf("model success publication = %s/%t/%s", selected.ID(), ok, coordinator.ThinkingLevel())
	}

	thinkingAppended := make(chan struct{})
	coordinator.appendThinkingControl = func(context.Context, string) (session.Entry, error) {
		close(thinkingAppended)
		return session.Entry{}, nil
	}
	coordinator.selectionMu.RLock()
	thinkingDone := make(chan error, 1)
	go func() { thinkingDone <- coordinator.SetThinkingLevel(provider.ThinkingLow) }()
	<-thinkingAppended
	waitForSelectionWriter(t, coordinator)
	if state := coordinator.loop.State(); state.ThinkingLevel() != provider.ThinkingHigh {
		coordinator.selectionMu.RUnlock()
		t.Fatalf("thinking publisher changed low Agent before lock admission: %s", state.ThinkingLevel())
	}
	coordinator.selectionMu.RUnlock()
	if err := <-thinkingDone; err != nil {
		t.Fatal(err)
	}
	if coordinator.ThinkingLevel() != provider.ThinkingLow || coordinator.loop.State().ThinkingLevel() != provider.ThinkingLow {
		t.Fatalf("thinking success publication = %s/%s", coordinator.ThinkingLevel(), coordinator.loop.State().ThinkingLevel())
	}

	unknownAppended := make(chan struct{})
	uncertain := errors.New("durability acknowledgement lost")
	coordinator.appendThinkingControl = func(context.Context, string) (session.Entry, error) {
		close(unknownAppended)
		return session.Entry{}, fmt.Errorf("%w: %w", session.ErrCommitUnknown, uncertain)
	}
	coordinator.selectionMu.RLock()
	unknownDone := make(chan error, 1)
	go func() { unknownDone <- coordinator.SetThinkingLevel(provider.ThinkingHigh) }()
	<-unknownAppended
	waitForSelectionWriter(t, coordinator)
	if state := coordinator.loop.State(); state.ThinkingLevel() != provider.ThinkingLow {
		coordinator.selectionMu.RUnlock()
		t.Fatalf("commit-unknown publisher changed low Agent before lock admission: %s", state.ThinkingLevel())
	}
	coordinator.selectionMu.RUnlock()
	err := <-unknownDone
	if !errors.Is(err, ErrTranscriptCommit) || !errors.Is(err, session.ErrCommitUnknown) || !errors.Is(err, uncertain) {
		t.Fatalf("commit-unknown error = %v", err)
	}
	if coordinator.ThinkingLevel() != provider.ThinkingHigh || coordinator.loop.State().ThinkingLevel() != provider.ThinkingHigh {
		t.Fatalf("thinking commit-unknown publication = %s/%s", coordinator.ThinkingLevel(), coordinator.loop.State().ThinkingLevel())
	}
}

func TestConcurrentForwardModelCyclesApplyTwoSerializedSteps(t *testing.T) {
	a, b, c := internalControlModel(t, "a"), internalControlModel(t, "b"), internalControlModel(t, "c")
	coordinator := internalControlSession(t, a, provider.ThinkingHigh)
	coordinator.SetScopedModels([]ScopedModel{{Model: a}, {Model: b}, {Model: c}})

	started := make(chan struct{}, 6)
	release := make(chan struct{})
	coordinator.modelAvailable = func(context.Context, provider.Model) (bool, error) {
		started <- struct{}{}
		<-release
		return true, nil
	}
	results := make(chan *ModelCycleResult, 2)
	errorsFound := make(chan error, 2)
	for range 2 {
		go func() {
			result, err := coordinator.CycleModel(context.Background(), CycleForward)
			results <- result
			errorsFound <- err
		}()
	}
	for range 6 {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("scoped availability checks did not all start concurrently")
		}
	}
	close(release)
	seen := map[string]bool{}
	for range 2 {
		if err := <-errorsFound; err != nil {
			t.Fatal(err)
		}
		result := <-results
		if result == nil {
			t.Fatal("concurrent cycle returned nil")
		}
		seen[result.Model.ID()] = true
	}
	selected, ok := coordinator.SelectedModel()
	if !ok || !selected.Equal(c) || !seen["b"] || !seen["c"] {
		t.Fatalf("serialized cycles = final %s/%t results %#v", selected.ID(), ok, seen)
	}
}

func TestConcurrentThinkingCyclesApplyTwoSerializedSteps(t *testing.T) {
	model := internalControlModel(t, "reasoning")
	coordinator := internalControlSession(t, model, provider.ThinkingOff)

	firstEntered := make(chan struct{})
	secondEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var callsMu sync.Mutex
	calls := 0
	coordinator.appendThinkingControl = func(context.Context, string) (session.Entry, error) {
		callsMu.Lock()
		calls++
		call := calls
		callsMu.Unlock()
		if call == 1 {
			close(firstEntered)
			<-releaseFirst
		} else if call == 2 {
			close(secondEntered)
		}
		return session.Entry{}, nil
	}
	results := make(chan *provider.ThinkingLevel, 2)
	errorsFound := make(chan error, 2)
	go func() {
		result, err := coordinator.CycleThinkingLevel()
		results <- result
		errorsFound <- err
	}()
	<-firstEntered
	go func() {
		result, err := coordinator.CycleThinkingLevel()
		results <- result
		errorsFound <- err
	}()
	select {
	case <-secondEntered:
		close(releaseFirst)
		t.Fatal("second thinking cycle entered persistence before the first mutation published")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	seen := map[provider.ThinkingLevel]bool{}
	for range 2 {
		if err := <-errorsFound; err != nil {
			t.Fatal(err)
		}
		result := <-results
		if result == nil {
			t.Fatal("thinking cycle returned nil")
		}
		seen[*result] = true
	}
	if got := coordinator.ThinkingLevel(); got != provider.ThinkingLow || !seen[provider.ThinkingMinimal] || !seen[provider.ThinkingLow] {
		t.Fatalf("serialized thinking cycles = final %s results %#v", got, seen)
	}
}

func TestModelSelectionHookRunsOutsideControlMutationLock(t *testing.T) {
	a, b := internalControlModel(t, "a"), internalControlModel(t, "b")
	coordinator := internalControlSession(t, a, provider.ThinkingHigh)
	coordinator.appendModelControl = func(context.Context, string, string, *string) ([]session.Entry, error) {
		return nil, nil
	}
	coordinator.appendThinkingControl = func(context.Context, string) (session.Entry, error) {
		return session.Entry{}, nil
	}
	hookDone := make(chan error, 1)
	coordinator.hooks.ModelSelect = func(context.Context, ModelSelectEvent) error {
		err := coordinator.SetThinkingLevel(provider.ThinkingLow)
		hookDone <- err
		return err
	}
	selectionDone := make(chan error, 1)
	go func() { selectionDone <- coordinator.SetModel(b) }()
	select {
	case err := <-selectionDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("model selection hook re-entered control mutation lock")
	}
	if err := <-hookDone; err != nil {
		t.Fatal(err)
	}
	selected, ok := coordinator.SelectedModel()
	if !ok || !selected.Equal(b) || coordinator.ThinkingLevel() != provider.ThinkingLow {
		t.Fatalf("reentrant hook selection = %s/%t/%s", selected.ID(), ok, coordinator.ThinkingLevel())
	}
}

func TestThinkingObserverEventsUseCapturedMutationLevel(t *testing.T) {
	model := internalControlModel(t, "reasoning")
	coordinator := internalControlSession(t, model, provider.ThinkingOff)
	coordinator.appendThinkingControl = func(context.Context, string) (session.Entry, error) {
		return session.Entry{}, nil
	}

	var levelsMu sync.Mutex
	var levels []provider.ThinkingLevel
	lowEntered := make(chan struct{})
	releaseLow := make(chan struct{})
	var blockLowOnce sync.Once
	unsubscribe := coordinator.Subscribe(func(_ context.Context, event SessionEvent) {
		changed, ok := event.(ThinkingLevelChangedEvent)
		if !ok {
			return
		}
		levelsMu.Lock()
		levels = append(levels, changed.Level)
		levelsMu.Unlock()
		if changed.Level == provider.ThinkingLow {
			blockLowOnce.Do(func() {
				close(lowEntered)
				<-releaseLow
			})
		}
	})
	defer unsubscribe()

	type delayedEmission struct {
		event thinkingSelectionEvent
		err   error
	}
	firstPublished := make(chan delayedEmission, 1)
	emitFirst := make(chan struct{})
	firstDone := make(chan struct{})
	go func() {
		coordinator.controlMu.Lock()
		event, err := coordinator.setThinkingLevelLocked(provider.ThinkingMinimal)
		coordinator.controlMu.Unlock()
		firstPublished <- delayedEmission{event: event, err: err}
		if err == nil {
			<-emitFirst
			coordinator.emitThinkingSelection(event)
		}
		close(firstDone)
	}()
	first := <-firstPublished
	if first.err != nil {
		t.Fatal(first.err)
	}

	secondDone := make(chan error, 1)
	go func() { secondDone <- coordinator.SetThinkingLevel(provider.ThinkingLow) }()
	select {
	case <-lowEntered:
	case <-time.After(time.Second):
		close(emitFirst)
		close(releaseLow)
		t.Fatal("second thinking observer did not block")
	}
	close(emitFirst)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		close(releaseLow)
		t.Fatal("captured first event was blocked as the current low level")
	}
	close(releaseLow)
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}

	levelsMu.Lock()
	defer levelsMu.Unlock()
	if len(levels) != 2 || levels[0] != provider.ThinkingLow || levels[1] != provider.ThinkingMinimal {
		t.Fatalf("thinking observer levels = %v, want [low minimal]", levels)
	}
}

func TestCycleModelReturnsThinkingAfterReentrantModelSelectHook(t *testing.T) {
	a, b := internalControlModel(t, "a"), internalControlModel(t, "b")
	coordinator := internalControlSession(t, a, provider.ThinkingHigh)
	coordinator.SetScopedModels([]ScopedModel{{Model: a}, {Model: b}})
	coordinator.modelAvailable = func(context.Context, provider.Model) (bool, error) { return true, nil }
	coordinator.appendModelControl = func(context.Context, string, string, *string) ([]session.Entry, error) {
		return nil, nil
	}
	coordinator.appendThinkingControl = func(context.Context, string) (session.Entry, error) {
		return session.Entry{}, nil
	}
	coordinator.hooks.ModelSelect = func(_ context.Context, event ModelSelectEvent) error {
		if event.Source != ModelSelectCycle {
			t.Fatalf("model select source = %s", event.Source)
		}
		return coordinator.SetThinkingLevel(provider.ThinkingLow)
	}

	result, err := coordinator.CycleModel(context.Background(), CycleForward)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || !result.Model.Equal(b) || result.ThinkingLevel != provider.ThinkingLow || !result.IsScoped {
		t.Fatalf("cycle result after reentrant hook = %#v", result)
	}
}

func TestScopedAvailabilityChecksConcurrentlyPreserveOrderAndFirstError(t *testing.T) {
	a, b, c := internalControlModel(t, "a"), internalControlModel(t, "b"), internalControlModel(t, "c")
	coordinator := internalControlSession(t, a, provider.ThinkingHigh)
	scoped := []ScopedModel{{Model: a}, {Model: b}, {Model: c}}

	t.Run("ordered filter", func(t *testing.T) {
		started := make(chan string, len(scoped))
		releases := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{}), "c": make(chan struct{})}
		coordinator.modelAvailable = func(_ context.Context, candidate provider.Model) (bool, error) {
			started <- candidate.ID()
			<-releases[candidate.ID()]
			return candidate.ID() != "a", nil
		}
		type availabilityOutput struct {
			models []ScopedModel
			err    error
		}
		done := make(chan availabilityOutput, 1)
		go func() {
			models, err := coordinator.availableScopedModels(context.Background(), scoped)
			done <- availabilityOutput{models: models, err: err}
		}()
		for range len(scoped) {
			select {
			case <-started:
			case <-time.After(time.Second):
				for _, release := range releases {
					close(release)
				}
				t.Fatal("scoped availability was evaluated serially")
			}
		}
		close(releases["c"])
		close(releases["b"])
		close(releases["a"])
		output := <-done
		if output.err != nil {
			t.Fatal(output.err)
		}
		if len(output.models) != 2 || output.models[0].Model.ID() != "b" || output.models[1].Model.ID() != "c" {
			t.Fatalf("ordered availability = %#v", output.models)
		}
	})

	t.Run("first indexed error after convergence", func(t *testing.T) {
		firstErr, secondErr := errors.New("first indexed error"), errors.New("second indexed error")
		started := make(chan string, len(scoped))
		releases := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{}), "c": make(chan struct{})}
		coordinator.modelAvailable = func(_ context.Context, candidate provider.Model) (bool, error) {
			started <- candidate.ID()
			<-releases[candidate.ID()]
			switch candidate.ID() {
			case "a":
				return false, firstErr
			case "b":
				return false, secondErr
			default:
				return true, nil
			}
		}
		done := make(chan error, 1)
		go func() {
			_, err := coordinator.availableScopedModels(context.Background(), scoped)
			done <- err
		}()
		for range len(scoped) {
			select {
			case <-started:
			case <-time.After(time.Second):
				for _, release := range releases {
					close(release)
				}
				t.Fatal("erroring availability checks did not start concurrently")
			}
		}
		close(releases["b"])
		close(releases["c"])
		select {
		case err := <-done:
			close(releases["a"])
			t.Fatalf("availability returned before all checks converged: %v", err)
		case <-time.After(25 * time.Millisecond):
		}
		close(releases["a"])
		err := <-done
		if !errors.Is(err, firstErr) || errors.Is(err, secondErr) {
			t.Fatalf("availability error = %v", err)
		}
	})

	t.Run("panic becomes invariant error", func(t *testing.T) {
		coordinator.modelAvailable = func(_ context.Context, candidate provider.Model) (bool, error) {
			if candidate.ID() == "b" {
				panic("availability panic")
			}
			return true, nil
		}
		_, err := coordinator.availableScopedModels(context.Background(), scoped)
		if !errors.Is(err, ErrInvariant) || !strings.Contains(err.Error(), "availability panic") {
			t.Fatalf("panic policy error = %v", err)
		}
	})
}

func TestModelCommitUnknownKeepsForwardSelectionAndDoesNotUndoSettings(t *testing.T) {
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.InMemorySessionManager(t.TempDir(), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	newModel := func(id string) provider.Model {
		value, err := provider.NewModel(provider.ModelSpec{
			Provider: "scripted", API: "scripted", ID: id, Name: id, Reasoning: true,
			Input: []provider.InputKind{provider.InputText}, ContextWindow: 16_000, MaxTokens: 1_000,
		})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	a, b := newModel("a"), newModel("b")
	settingsModel := "a"
	settingsThinking := provider.ThinkingHigh
	undoCalls := 0
	runtime, err := NewSession(SessionConfig{
		Provider: implementation, SessionManager: manager, Model: a, ThinkingLevel: provider.ThinkingHigh,
		PersistSettings: func(_ context.Context, update SettingsUpdate) (SettingsWriteResult, error) {
			beforeModel, beforeThinking := settingsModel, settingsThinking
			if update.DefaultModel != nil {
				settingsModel = *update.DefaultModel
			}
			if update.DefaultThinkingLevel != nil {
				settingsThinking = *update.DefaultThinkingLevel
			}
			undo := func(context.Context) error {
				undoCalls++
				settingsModel, settingsThinking = beforeModel, beforeThinking
				return nil
			}
			return SettingsWriteResult{Undo: undo}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	uncertain := errors.New("durability acknowledgement lost")
	runtime.appendModelControl = func(context.Context, string, string, *string) ([]session.Entry, error) {
		return nil, fmt.Errorf("%w: %w", session.ErrCommitUnknown, uncertain)
	}
	err = runtime.SetModel(b)
	if !errors.Is(err, ErrTranscriptCommit) || !errors.Is(err, session.ErrCommitUnknown) || !errors.Is(err, uncertain) {
		t.Fatalf("SetModel error = %v", err)
	}
	selected, ok := runtime.SelectedModel()
	if !ok || !selected.Equal(b) || settingsModel != "b" || undoCalls != 0 {
		t.Fatalf("unknown outcome policy: model=%s present=%t settings=%q undo=%d", selected.ID(), ok, settingsModel, undoCalls)
	}
	runtime.appendThinkingControl = func(context.Context, string) (session.Entry, error) {
		return session.Entry{}, fmt.Errorf("%w: %w", session.ErrCommitUnknown, uncertain)
	}
	err = runtime.SetThinkingLevel(provider.ThinkingLow)
	if !errors.Is(err, ErrTranscriptCommit) || !errors.Is(err, session.ErrCommitUnknown) {
		t.Fatalf("SetThinkingLevel error = %v", err)
	}
	if runtime.ThinkingLevel() != provider.ThinkingLow || settingsThinking != provider.ThinkingLow || undoCalls != 0 {
		t.Fatalf("unknown thinking policy: state=%q settings=%q undo=%d", runtime.ThinkingLevel(), settingsThinking, undoCalls)
	}
	if len(manager.Entries()) != 0 {
		t.Fatalf("injected uncertain append changed real manager: %d", len(manager.Entries()))
	}
}
