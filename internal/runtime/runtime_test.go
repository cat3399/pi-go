package agentruntime_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
	"github.com/cat3399/pi-go/internal/session"
)

type runtimeHarness struct {
	t          *testing.T
	provider   *provider.ScriptedProvider
	model      provider.Model
	hooks      agent.Hooks
	factory    agentruntime.Factory
	starts     []agent.SessionStartHookEvent
	shutdowns  []agent.SessionShutdownHookEvent
	created    []*session.SessionManager
	createCall int
	factoryErr func(int) error
	mu         sync.Mutex
}

func newRuntimeHarness(t *testing.T, hooks agent.Hooks) *runtimeHarness {
	t.Helper()
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{})
	if err != nil {
		t.Fatal(err)
	}
	model, err := provider.NewModel(provider.ModelSpec{
		Provider: "scripted", API: "scripted", ID: "runtime-test", Name: "Runtime Test", Input: []provider.InputKind{provider.InputText},
		ContextWindow: 16_000, MaxTokens: 1_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &runtimeHarness{t: t, provider: implementation, model: model, hooks: hooks}
	originalStart := hooks.SessionStart
	originalShutdown := hooks.SessionShutdown
	h.hooks.SessionStart = func(ctx context.Context, event agent.SessionStartHookEvent) error {
		h.mu.Lock()
		h.starts = append(h.starts, event)
		h.mu.Unlock()
		if originalStart != nil {
			return originalStart(ctx, event)
		}
		return nil
	}
	h.hooks.SessionShutdown = func(ctx context.Context, event agent.SessionShutdownHookEvent) error {
		h.mu.Lock()
		h.shutdowns = append(h.shutdowns, event)
		h.mu.Unlock()
		if originalShutdown != nil {
			return originalShutdown(ctx, event)
		}
		return nil
	}
	h.factory = func(_ context.Context, options agentruntime.CreateOptions) (agentruntime.CreateResult, error) {
		h.mu.Lock()
		h.createCall++
		call := h.createCall
		h.created = append(h.created, options.SessionManager)
		h.mu.Unlock()
		if h.factoryErr != nil {
			if err := h.factoryErr(call); err != nil {
				return agentruntime.CreateResult{}, err
			}
		}
		created, err := agent.NewSession(agent.SessionConfig{
			Provider: h.provider, SessionManager: options.SessionManager, Model: h.model,
			ThinkingLevel: provider.ThinkingOff, Hooks: h.hooks, SessionStartEvent: options.SessionStartEvent,
			SettlementTimeout: time.Second,
		})
		if err != nil {
			return agentruntime.CreateResult{}, err
		}
		fallback := "fallback:" + options.CWD
		return agentruntime.CreateResult{
			Session: created, Services: &agentruntime.Services{CWD: options.CWD, AgentDir: options.AgentDir},
			Diagnostics:          []agentruntime.Diagnostic{{Kind: agentruntime.DiagnosticWarning, Message: options.CWD}},
			ModelFallbackMessage: &fallback,
		}, nil
	}
	return h
}

func createRuntime(t *testing.T, h *runtimeHarness, manager *session.SessionManager, cwd string) *agentruntime.Runtime {
	t.Helper()
	runtime, err := agentruntime.Create(context.Background(), h.factory, agentruntime.InitialOptions{
		CWD: cwd, AgentDir: t.TempDir(), SessionManager: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runtime.Dispose(context.Background()); err != nil {
			t.Errorf("dispose runtime: %v", err)
		}
	})
	return runtime
}

func TestRuntimeNewSessionLifecycleOrderingAndSetup(t *testing.T) {
	cwd := t.TempDir()
	manager, err := session.CreateSessionManager(cwd, t.TempDir(), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sourceFile, ok := manager.SessionFile()
	if !ok {
		t.Fatal("persistent source has no session file")
	}
	var order []string
	h := newRuntimeHarness(t, agent.Hooks{
		SessionBeforeSwitch: func(_ context.Context, event agent.SessionBeforeSwitchEvent) (agent.SessionBeforeSwitchResult, error) {
			if event.Reason != agent.SessionSwitchNew || event.TargetSessionFile != nil {
				t.Fatalf("before switch event = %#v", event)
			}
			order = append(order, "before:new")
			return agent.SessionBeforeSwitchResult{}, nil
		},
		SessionShutdown: func(_ context.Context, event agent.SessionShutdownHookEvent) error {
			order = append(order, "shutdown:"+string(event.Reason))
			return nil
		},
		SessionStart: func(_ context.Context, event agent.SessionStartHookEvent) error {
			order = append(order, "start:"+string(event.Reason))
			return nil
		},
	})
	runtime := createRuntime(t, h, manager, cwd)
	order = nil
	initial := runtime.Session()
	runtime.SetBeforeSessionInvalidate(func() { order = append(order, "invalidate") })
	runtime.SetRebindSession(func(_ context.Context, replacement *agent.AgentSession) error {
		if replacement != runtime.Session() {
			t.Fatal("rebind did not receive current replacement")
		}
		order = append(order, "rebind")
		return nil
	})
	result, err := runtime.NewSession(context.Background(), agentruntime.NewOptions{
		Setup: func(ctx context.Context, manager *session.SessionManager) error {
			message, err := llm.NewUserTextMessage("restored", time.Now())
			if err != nil {
				return err
			}
			_, err = manager.AppendLLMMessage(ctx, message)
			return err
		},
		WithSession: func(_ context.Context, replacement *agent.AgentSession) error {
			if replacement != runtime.Session() {
				t.Fatal("withSession did not receive current replacement")
			}
			order = append(order, "with")
			return nil
		},
	})
	if err != nil || result.Cancelled {
		t.Fatalf("NewSession() = (%#v, %v)", result, err)
	}
	if reflect.DeepEqual(initial, runtime.Session()) {
		t.Fatal("session was not replaced")
	}
	if got := runtime.Session().SessionManager().BuildContext().AgentMessages(); len(got) != 1 || got[0].Role() != agentmsg.RoleUser {
		t.Fatalf("setup messages were not retained: %#v", got)
	}
	if want := []string{"before:new", "shutdown:new", "invalidate", "start:new", "rebind", "with"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("replacement order = %#v, want %#v", order, want)
	}
	if len(h.starts) != 2 || h.starts[0].Reason != agent.SessionStartup || h.starts[1].Reason != agent.SessionNew {
		t.Fatalf("start events = %#v", h.starts)
	}
	targetFile, ok := runtime.Session().SessionManager().SessionFile()
	if !ok {
		t.Fatal("replacement has no session file")
	}
	if len(h.shutdowns) != 1 || h.shutdowns[0].TargetSessionFile == nil || *h.shutdowns[0].TargetSessionFile != targetFile {
		t.Fatalf("shutdown events = %#v, target=%q", h.shutdowns, targetFile)
	}
	if h.starts[1].PreviousSessionFile == nil || *h.starts[1].PreviousSessionFile != sourceFile {
		t.Fatalf("new start previous = %#v, want %q", h.starts[1].PreviousSessionFile, sourceFile)
	}
	fallback := runtime.ModelFallbackMessage()
	if got := runtime.Diagnostics(); len(got) != 1 || got[0].Message != cwd || fallback == nil || *fallback != "fallback:"+cwd {
		t.Fatalf("runtime metadata not replaced: diagnostics=%#v fallback=%v", got, fallback)
	}
}

func TestRuntimeCancellationRunsBeforeForkValidationAndAllocation(t *testing.T) {
	cwd := t.TempDir()
	manager, err := session.InMemorySessionManager(cwd, session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cancel := true
	h := newRuntimeHarness(t, agent.Hooks{
		SessionBeforeSwitch: func(context.Context, agent.SessionBeforeSwitchEvent) (agent.SessionBeforeSwitchResult, error) {
			return agent.SessionBeforeSwitchResult{Cancel: agent.HookCancel{Cancel: &cancel}}, nil
		},
		SessionBeforeFork: func(context.Context, agent.SessionBeforeForkEvent) (agent.SessionBeforeForkResult, error) {
			return agent.SessionBeforeForkResult{Cancel: agent.HookCancel{Cancel: &cancel}}, nil
		},
	})
	runtime := createRuntime(t, h, manager, cwd)
	initial := runtime.Session()
	newResult, err := runtime.NewSession(context.Background(), agentruntime.NewOptions{})
	if err != nil || !newResult.Cancelled {
		t.Fatalf("cancelled NewSession() = (%#v, %v)", newResult, err)
	}
	forkResult, err := runtime.Fork(context.Background(), "missing-entry", agentruntime.ForkOptions{})
	if err != nil || !forkResult.Cancelled {
		t.Fatalf("cancelled Fork() = (%#v, %v)", forkResult, err)
	}
	if runtime.Session() != initial || h.createCall != 1 || len(h.shutdowns) != 0 {
		t.Fatalf("cancelled replacement mutated runtime: calls=%d shutdowns=%#v", h.createCall, h.shutdowns)
	}
}

func TestRuntimeValidatesInitialPersistedCwdBeforeFactory(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "stored")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := session.CreateSessionManager(cwd, filepath.Join(root, "sessions"), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close initial manager: %v", err)
		}
	})
	moved := cwd + "-moved"
	if err := os.Rename(cwd, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Rename(moved, cwd); err != nil {
			t.Errorf("restore stored cwd: %v", err)
		}
	})
	called := false
	_, err = agentruntime.Create(context.Background(), func(context.Context, agentruntime.CreateOptions) (agentruntime.CreateResult, error) {
		called = true
		return agentruntime.CreateResult{}, nil
	}, agentruntime.InitialOptions{CWD: root, AgentDir: root, SessionManager: manager})
	var missing *agentruntime.MissingSessionCwdError
	if !errors.As(err, &missing) || called {
		t.Fatalf("Create() = missing %#v, called=%v, error=%v", missing, called, err)
	}
	if _, appendErr := appendUser(context.Background(), manager, "still owned by caller"); appendErr != nil {
		t.Fatalf("initial validation closed caller manager: %v", appendErr)
	}
}

func TestRuntimeForkBeforeAndAtUseIndependentMemoryManagers(t *testing.T) {
	cwd := t.TempDir()
	manager, err := session.InMemorySessionManager(cwd, session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := appendUser(context.Background(), manager, "first", " message")
	if err != nil {
		t.Fatal(err)
	}
	second, err := appendUser(context.Background(), manager, "second")
	if err != nil {
		t.Fatal(err)
	}
	h := newRuntimeHarness(t, agent.Hooks{})
	runtime := createRuntime(t, h, manager, cwd)
	result, err := runtime.Fork(context.Background(), second.ID(), agentruntime.ForkOptions{Position: agent.ForkBefore})
	if err != nil || result.Cancelled || result.SelectedText == nil || *result.SelectedText != "second" {
		t.Fatalf("Fork(before) = (%#v, %v)", result, err)
	}
	if runtime.Session().SessionManager() == manager {
		t.Fatal("fork reused the disposed source manager")
	}
	entries := runtime.Session().SessionManager().Entries()
	if len(entries) != 1 || entries[0].ID() != first.ID() {
		t.Fatalf("before branch entries = %#v", entryIDs(entries))
	}
	result, err = runtime.Fork(context.Background(), first.ID(), agentruntime.ForkOptions{Position: agent.ForkAt})
	if err != nil || result.Cancelled || result.SelectedText != nil {
		t.Fatalf("Fork(at) = (%#v, %v)", result, err)
	}
	entries = runtime.Session().SessionManager().Entries()
	if len(entries) != 1 || entries[0].ID() != first.ID() {
		t.Fatalf("at branch entries = %#v", entryIDs(entries))
	}
	result, err = runtime.Fork(context.Background(), first.ID(), agentruntime.ForkOptions{Position: agent.ForkBefore})
	if err != nil || result.SelectedText == nil || *result.SelectedText != "first message" {
		t.Fatalf("root Fork(before) = (%#v, %v)", result, err)
	}
	if entries = runtime.Session().SessionManager().Entries(); len(entries) != 0 {
		t.Fatalf("root fork entries = %#v", entryIDs(entries))
	}
}

func TestRuntimeReplacementSettlesActiveResponseBeforeShutdown(t *testing.T) {
	cwd := t.TempDir()
	manager, err := session.CreateSessionManager(cwd, t.TempDir(), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	h := newRuntimeHarness(t, agent.Hooks{})
	started := make(chan struct{})
	step, err := provider.FactoryResponseStep(func(ctx context.Context, _ provider.Request, _ uint64) (llm.AssistantTerminal, error) {
		close(started)
		<-ctx.Done()
		return nil, context.Cause(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.provider.SetResponses([]provider.ScriptStep{step}); err != nil {
		t.Fatal(err)
	}
	runtime := createRuntime(t, h, manager, cwd)
	sourceFile, _ := manager.SessionFile()
	runDone := make(chan error, 1)
	go func() {
		_, runErr := runtime.Session().Run(context.Background(), "interrupt me")
		runDone <- runErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	if _, err := runtime.NewSession(context.Background(), agentruntime.NewOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("outgoing response did not settle")
	}
	reopened, err := session.OpenSessionManager(sourceFile, filepath.Dir(sourceFile), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("close reopened source: %v", err)
		}
	})
	messages := reopened.BuildContext().Messages()
	if len(messages) != 2 || messages[0].Role() != llm.RoleUser || messages[1].Role() != llm.RoleAssistant {
		t.Fatalf("outgoing durable messages = %#v", messages)
	}
	terminal, ok := messages[1].(llm.AssistantTerminal)
	if !ok || terminal.FinishReason() != llm.FinishAborted {
		t.Fatalf("outgoing terminal = %T %#v", messages[1], messages[1])
	}
}

func TestRuntimePersistentForkAndUnsavedGuard(t *testing.T) {
	cwd := t.TempDir()
	sessionDir := t.TempDir()
	manager, err := session.CreateSessionManager(cwd, sessionDir, session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	user, err := appendUser(context.Background(), manager, "saved")
	if err != nil {
		t.Fatal(err)
	}
	assistantEntry, err := appendAssistant(context.Background(), manager, "answer")
	if err != nil {
		t.Fatal(err)
	}
	sourceFile, ok := manager.SessionFile()
	if !ok {
		t.Fatal("persistent manager has no file")
	}
	var order []string
	h := newRuntimeHarness(t, agent.Hooks{
		SessionBeforeFork: func(_ context.Context, event agent.SessionBeforeForkEvent) (agent.SessionBeforeForkResult, error) {
			if event.EntryID != assistantEntry.ID() || event.Position != agent.ForkAt {
				t.Fatalf("before fork event = %#v", event)
			}
			order = append(order, "before:fork")
			return agent.SessionBeforeForkResult{}, nil
		},
		SessionShutdown: func(_ context.Context, event agent.SessionShutdownHookEvent) error {
			order = append(order, "shutdown:"+string(event.Reason))
			return nil
		},
		SessionStart: func(_ context.Context, event agent.SessionStartHookEvent) error {
			order = append(order, "start:"+string(event.Reason))
			return nil
		},
	})
	runtime := createRuntime(t, h, manager, cwd)
	order = nil
	runtime.SetBeforeSessionInvalidate(func() { order = append(order, "invalidate") })
	runtime.SetRebindSession(func(context.Context, *agent.AgentSession) error {
		order = append(order, "rebind")
		return nil
	})
	result, err := runtime.Fork(context.Background(), assistantEntry.ID(), agentruntime.ForkOptions{
		Position: agent.ForkAt,
		WithSession: func(context.Context, *agent.AgentSession) error {
			order = append(order, "with")
			return nil
		},
	})
	if err != nil || result.Cancelled {
		t.Fatalf("persistent Fork() = (%#v, %v)", result, err)
	}
	forkFile, ok := runtime.Session().SessionManager().SessionFile()
	if !ok || forkFile == sourceFile {
		t.Fatalf("fork file = %q, source = %q", forkFile, sourceFile)
	}
	if _, err := os.Stat(forkFile); err != nil {
		t.Fatalf("fork was not durable: %v", err)
	}
	if got := entryIDs(runtime.Session().SessionManager().Entries()); !reflect.DeepEqual(got, []string{user.ID(), assistantEntry.ID()}) {
		t.Fatalf("fork entries = %#v", got)
	}
	if want := []string{"before:fork", "shutdown:fork", "invalidate", "start:fork", "rebind", "with"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("fork lifecycle = %#v, want %#v", order, want)
	}
	if len(h.shutdowns) != 1 || h.shutdowns[0].TargetSessionFile == nil || *h.shutdowns[0].TargetSessionFile != forkFile {
		t.Fatalf("fork shutdown = %#v, target=%q", h.shutdowns, forkFile)
	}
	if len(h.starts) != 2 || h.starts[1].PreviousSessionFile == nil || *h.starts[1].PreviousSessionFile != sourceFile {
		t.Fatalf("fork starts = %#v, previous=%q", h.starts, sourceFile)
	}

	unsaved, err := session.CreateSessionManager(cwd, sessionDir, session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := appendUser(context.Background(), unsaved, "not saved")
	if err != nil {
		t.Fatal(err)
	}
	other := createRuntime(t, newRuntimeHarness(t, agent.Hooks{}), unsaved, cwd)
	_, err = other.Fork(context.Background(), entry.ID(), agentruntime.ForkOptions{Position: agent.ForkAt})
	if err == nil || !strings.Contains(err.Error(), "Wait for the first assistant response") {
		t.Fatalf("unsaved fork error = %v", err)
	}
}

func TestRuntimeSwitchCwdValidationImportAndFactoryFailureOwnership(t *testing.T) {
	root := t.TempDir()
	currentCWD := filepath.Join(root, "current")
	targetCWD := filepath.Join(root, "target")
	if err := os.MkdirAll(currentCWD, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(targetCWD, 0o700); err != nil {
		t.Fatal(err)
	}
	current, err := session.CreateSessionManager(currentCWD, filepath.Join(root, "sessions"), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	currentFile, ok := current.SessionFile()
	if !ok {
		t.Fatal("current runtime has no session file")
	}
	var targetFile string
	var expectedSwitchTarget string
	var order []string
	h := newRuntimeHarness(t, agent.Hooks{
		SessionBeforeSwitch: func(_ context.Context, event agent.SessionBeforeSwitchEvent) (agent.SessionBeforeSwitchResult, error) {
			if event.Reason == agent.SessionSwitchNew {
				return agent.SessionBeforeSwitchResult{}, nil
			}
			if event.Reason != agent.SessionSwitchResume || event.TargetSessionFile == nil || *event.TargetSessionFile != expectedSwitchTarget {
				t.Fatalf("before resume event = %#v, target=%q", event, expectedSwitchTarget)
			}
			order = append(order, "before:resume")
			return agent.SessionBeforeSwitchResult{}, nil
		},
		SessionShutdown: func(_ context.Context, event agent.SessionShutdownHookEvent) error {
			order = append(order, "shutdown:"+string(event.Reason))
			return nil
		},
		SessionStart: func(_ context.Context, event agent.SessionStartHookEvent) error {
			order = append(order, "start:"+string(event.Reason))
			return nil
		},
	})
	runtime := createRuntime(t, h, current, currentCWD)
	runtime.SetBeforeSessionInvalidate(func() { order = append(order, "invalidate") })
	runtime.SetRebindSession(func(context.Context, *agent.AgentSession) error {
		order = append(order, "rebind")
		return nil
	})

	target, err := session.CreateSessionManager(targetCWD, filepath.Join(root, "targets"), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appendUser(context.Background(), target, "target"); err != nil {
		t.Fatal(err)
	}
	if _, err := appendAssistant(context.Background(), target, "answer"); err != nil {
		t.Fatal(err)
	}
	targetFile, _ = target.SessionFile()
	expectedSwitchTarget = targetFile
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	missingLocation := targetCWD + "-moved"
	if err := os.Rename(targetCWD, missingLocation); err != nil {
		t.Fatal(err)
	}
	_, err = runtime.SwitchSession(context.Background(), targetFile, agentruntime.SwitchOptions{})
	var missing *agentruntime.MissingSessionCwdError
	if !errors.As(err, &missing) || missing.Issue.SessionCWD != targetCWD {
		t.Fatalf("missing cwd error = %#v (%v)", missing, err)
	}
	if err := os.Rename(missingLocation, targetCWD); err != nil {
		t.Fatal(err)
	}
	order = nil
	result, err := runtime.SwitchSession(context.Background(), targetFile, agentruntime.SwitchOptions{
		WithSession: func(context.Context, *agent.AgentSession) error {
			order = append(order, "with")
			return nil
		},
	})
	if err != nil || result.Cancelled || runtime.CWD() != targetCWD {
		t.Fatalf("SwitchSession() = (%#v, %v), cwd=%q", result, err, runtime.CWD())
	}
	if want := []string{"before:resume", "shutdown:resume", "invalidate", "start:resume", "rebind", "with"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("resume lifecycle = %#v, want %#v", order, want)
	}
	if len(h.shutdowns) != 1 || h.shutdowns[0].TargetSessionFile == nil || *h.shutdowns[0].TargetSessionFile != targetFile {
		t.Fatalf("resume shutdown = %#v, target=%q", h.shutdowns, targetFile)
	}
	if len(h.starts) != 2 || h.starts[1].PreviousSessionFile == nil || *h.starts[1].PreviousSessionFile != currentFile {
		t.Fatalf("resume starts = %#v, previous=%q", h.starts, currentFile)
	}

	importDir := filepath.Join(root, "imports")
	imported, err := session.CreateSessionManager(targetCWD, importDir, session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appendUser(context.Background(), imported, "imported"); err != nil {
		t.Fatal(err)
	}
	if _, err := appendAssistant(context.Background(), imported, "answer"); err != nil {
		t.Fatal(err)
	}
	importFile, _ := imported.SessionFile()
	if err := imported.Close(); err != nil {
		t.Fatal(err)
	}
	expectedSwitchTarget = filepath.Join(runtime.Session().SessionManager().SessionDir(), filepath.Base(importFile))
	result, err = runtime.ImportFromJSONL(context.Background(), importFile, "")
	if err != nil || result.Cancelled {
		t.Fatalf("ImportFromJSONL() = (%#v, %v)", result, err)
	}
	if got := runtime.Session().SessionManager().BuildContext().Messages(); len(got) != 2 {
		t.Fatalf("imported context has %d messages", len(got))
	}

	failAt := h.createCall + 1
	h.factoryErr = func(call int) error {
		if call == failAt {
			return errors.New("factory failed")
		}
		return nil
	}
	_, err = runtime.NewSession(context.Background(), agentruntime.NewOptions{})
	if err == nil || err.Error() != "factory failed" {
		t.Fatalf("factory failure = %v", err)
	}
	failedManager := h.created[len(h.created)-1]
	_, appendErr := appendUser(context.Background(), failedManager, "must fail")
	if !errors.Is(appendErr, session.ErrClosed) {
		t.Fatalf("failed replacement manager remained open: %v", appendErr)
	}
}

func TestRuntimeDisposeEmitsQuitBeforeInvalidationAndIsIdempotent(t *testing.T) {
	cwd := t.TempDir()
	manager, err := session.InMemorySessionManager(cwd, session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	h := newRuntimeHarness(t, agent.Hooks{SessionShutdown: func(_ context.Context, event agent.SessionShutdownHookEvent) error {
		order = append(order, "shutdown:"+string(event.Reason))
		return nil
	}})
	runtime := createRuntime(t, h, manager, cwd)
	runtime.SetBeforeSessionInvalidate(func() { order = append(order, "invalidate") })
	if err := runtime.Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"shutdown:quit", "invalidate"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("dispose order = %#v, want %#v", order, want)
	}
}

func appendUser(ctx context.Context, manager *session.SessionManager, parts ...string) (session.Entry, error) {
	blocks := make([]llm.TextBlock, 0, len(parts))
	for _, part := range parts {
		block, err := llm.NewTextBlock(part)
		if err != nil {
			return session.Entry{}, err
		}
		blocks = append(blocks, block)
	}
	message, err := llm.NewUserTextBlocksMessage(blocks, time.Now())
	if err != nil {
		return session.Entry{}, err
	}
	return manager.AppendLLMMessage(ctx, message)
}

func appendAssistant(ctx context.Context, manager *session.SessionManager, text string) (session.Entry, error) {
	block, err := llm.NewTextBlock(text)
	if err != nil {
		return session.Entry{}, err
	}
	usage, err := llm.NewUsage(llm.UsageSpec{})
	if err != nil {
		return session.Entry{}, err
	}
	message, err := llm.NewAssistantTextMessage(
		[]llm.TextBlock{block}, llm.FinishStop, usage, time.Now(),
		llm.AssistantProvenance{API: "scripted", Provider: "scripted", Model: "runtime-test"},
	)
	if err != nil {
		return session.Entry{}, err
	}
	return manager.AppendLLMMessage(ctx, message)
}

func entryIDs(entries []session.Entry) []string {
	result := make([]string, len(entries))
	for index, entry := range entries {
		result[index] = entry.ID()
	}
	return result
}
