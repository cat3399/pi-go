package app_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
	"github.com/cat3399/pi-go/internal/tool"
)

var appTestEpoch = time.Date(2026, time.August, 1, 12, 30, 0, 0, time.UTC)

func TestRunCompletesToolWorkflowAndReopensSession(t *testing.T) {
	workingDir := t.TempDir()
	sessionPath := filepath.Join(workingDir, "workflow.jsonl")
	runner := &scriptedRunner{output: []byte("tool-output\n")}
	providerImpl := newScriptedProvider(
		t,
		fixedStep(t, toolTerminal(t, "call-app", "bash", []byte(`{"command":"ignored"}`))),
		factoryStep(t, func(_ context.Context, request provider.Request, call uint64) (llm.AssistantTerminal, error) {
			messages := request.Messages()
			if call != 2 || len(messages) != 3 {
				return nil, fmt.Errorf("provider two context = call %d, messages %d", call, len(messages))
			}
			result, ok := messages[2].(llm.ToolResultMessage)
			if !ok || result.IsError() || !strings.Contains(textBlocks(result.Content()), "tool-output") {
				return nil, fmt.Errorf("provider two tool result = %T", messages[2])
			}
			return textTerminal(t, "final answer"), nil
		}),
	)
	deps := testDependencies(t, workingDir, providerImpl, runner)
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := app.Run(
		context.Background(),
		deps,
		[]string{"-p", "do the work", "--session", filepath.Base(sessionPath)},
		&stdout,
		&stderr,
	)
	if exitCode != app.ExitSuccess || stdout.String() != "final answer\n" || stderr.Len() != 0 {
		t.Fatalf("Run() = code %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
	}
	if providerImpl.CallCount() != 2 || runner.calls.Load() != 1 {
		t.Fatalf("calls = provider %d, runner %d", providerImpl.CallCount(), runner.calls.Load())
	}

	reopened, err := session.Open(sessionPath, session.OpenOptions{})
	if err != nil {
		t.Fatalf("session.Open() after Run = %v", err)
	}
	defer reopened.Close()
	messages := reopened.Context().Messages()
	if roles := messageRoles(messages); !reflect.DeepEqual(roles, []llm.Role{
		llm.RoleUser,
		llm.RoleAssistant,
		llm.RoleToolResult,
		llm.RoleAssistant,
	}) {
		t.Fatalf("durable roles = %v", roles)
	}
	if final, ok := messages[3].(llm.AssistantTextMessage); !ok || textBlocks(final.Content()) != "final answer" {
		t.Fatalf("durable final = %T", messages[3])
	}
}

func TestRunUsesInjectedDefaultSessionPath(t *testing.T) {
	workingDir := t.TempDir()
	sessionPath := filepath.Join(workingDir, "state", "sessions", "default.jsonl")
	providerImpl := newScriptedProvider(t, fixedStep(t, textTerminalBlocks(t, "default", "path")))
	deps := testDependencies(t, workingDir, providerImpl, &scriptedRunner{})
	var pathCalls atomic.Uint32
	deps.DefaultSessionPath = func(gotWorkingDir string) (string, error) {
		pathCalls.Add(1)
		if gotWorkingDir != workingDir {
			t.Fatalf("default path working dir = %q, want %q", gotWorkingDir, workingDir)
		}
		return sessionPath, nil
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := app.Run(context.Background(), deps, []string{"--print", "hello"}, &stdout, &stderr)
	if exitCode != app.ExitSuccess || stdout.String() != "default\npath\n" || stderr.Len() != 0 {
		t.Fatalf("Run() = code %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
	}
	if pathCalls.Load() != 1 {
		t.Fatalf("default path calls = %d, want 1", pathCalls.Load())
	}
	if _, err := os.Stat(sessionPath); err != nil {
		t.Fatalf("default session was not created: %v", err)
	}
}

func TestRunCreatesExplicitSessionParentDirectories(t *testing.T) {
	workingDir := t.TempDir()
	sessionPath := filepath.Join(workingDir, "explicit", "nested", "session.jsonl")
	providerImpl := newScriptedProvider(t, fixedStep(t, textTerminal(t, "created")))
	deps := testDependencies(t, workingDir, providerImpl, &scriptedRunner{})
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := app.Run(context.Background(), deps, []string{
		"-p", "hello", "--session", sessionPath,
	}, &stdout, &stderr)
	if exitCode != app.ExitSuccess || stdout.String() != "created\n" || stderr.Len() != 0 {
		t.Fatalf("Run() = code %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(sessionPath); err != nil {
		t.Fatalf("explicit nested session was not created: %v", err)
	}
}

func TestRunAcceptsLeadingDashPromptAndRelativeSessionPath(t *testing.T) {
	workingDir := t.TempDir()
	sessionName := "-session.jsonl"
	providerImpl := newScriptedProvider(t, factoryStep(t, func(
		_ context.Context,
		request provider.Request,
		_ uint64,
	) (llm.AssistantTerminal, error) {
		messages := request.Messages()
		user, ok := messages[0].(llm.UserTextMessage)
		if !ok || textBlocks(user.Content()) != "- explain" {
			return nil, fmt.Errorf("user prompt = %T", messages[0])
		}
		return textTerminal(t, "accepted"), nil
	}))
	deps := testDependencies(t, workingDir, providerImpl, &scriptedRunner{})
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := app.Run(context.Background(), deps, []string{
		"--session", sessionName, "-p", "- explain",
	}, &stdout, &stderr)
	if exitCode != app.ExitSuccess || stdout.String() != "accepted\n" || stderr.Len() != 0 {
		t.Fatalf("Run() = code %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(workingDir, sessionName)); err != nil {
		t.Fatalf("leading-dash session path was not used: %v", err)
	}
}

func TestRunResumeUsesDurableSessionWorkingDirectory(t *testing.T) {
	sessionWorkingDir := t.TempDir()
	launchWorkingDir := t.TempDir()
	sessionPath := filepath.Join(sessionWorkingDir, "other-project.jsonl")
	transcript, err := session.Create(sessionPath, session.CreateOptions{
		ID:         "other-project-session",
		WorkingDir: sessionWorkingDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedRunner{
		output:         []byte("right-directory\n"),
		wantWorkingDir: sessionWorkingDir,
	}
	providerImpl := newScriptedProvider(
		t,
		fixedStep(t, toolTerminal(t, "call-resume-cwd", "bash", []byte(`{"command":"pwd"}`))),
		factoryStep(t, func(_ context.Context, request provider.Request, _ uint64) (llm.AssistantTerminal, error) {
			messages := request.Messages()
			result, ok := messages[len(messages)-1].(llm.ToolResultMessage)
			if !ok || result.IsError() || !strings.Contains(textBlocks(result.Content()), "right-directory") {
				return nil, fmt.Errorf("resumed tool result = %T", messages[len(messages)-1])
			}
			return textTerminal(t, "resumed cwd"), nil
		}),
	)
	deps := testDependencies(t, launchWorkingDir, providerImpl, runner)
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := app.Run(context.Background(), deps, []string{
		"--session", sessionPath, "-p", "run there",
	}, &stdout, &stderr)
	if exitCode != app.ExitSuccess || stdout.String() != "resumed cwd\n" || stderr.Len() != 0 {
		t.Fatalf("Run() = code %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
	}
}

func TestRunReportsTerminalFailureAndClosesSession(t *testing.T) {
	workingDir := t.TempDir()
	sessionPath := filepath.Join(workingDir, "failure.jsonl")
	providerImpl := newScriptedProvider(t)
	deps := testDependencies(t, workingDir, providerImpl, &scriptedRunner{})
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := app.Run(
		context.Background(),
		deps,
		[]string{"-p", "fail", "--session", sessionPath},
		&stdout,
		&stderr,
	)
	if exitCode != app.ExitFailure || stdout.Len() != 0 {
		t.Fatalf("Run() = code %d, stdout %q", exitCode, stdout.String())
	}
	if stderr.String() != provider.ErrQueueExhausted.Error()+"\n" {
		t.Fatalf("stderr = %q", stderr.String())
	}
	reopened, err := session.Open(sessionPath, session.OpenOptions{})
	if err != nil {
		t.Fatalf("session remained writer-owned after failure: %v", err)
	}
	messages := reopened.Context().Messages()
	_ = reopened.Close()
	if len(messages) != 2 {
		t.Fatalf("failure transcript messages = %d, want 2", len(messages))
	}
	if failure, ok := messages[1].(llm.AssistantFailureMessage); !ok || failure.FinishReason() != llm.FinishError {
		t.Fatalf("failure terminal = %T", messages[1])
	}
}

func TestRunPreservesInvalidExplicitSession(t *testing.T) {
	workingDir := t.TempDir()
	sessionPath := filepath.Join(workingDir, "not-a-session.jsonl")
	original := []byte("{\"type\":\"event\",\"data\":\"not a session\"}\n")
	if err := os.WriteFile(sessionPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	providerImpl := newScriptedProvider(t, fixedStep(t, textTerminal(t, "unused")))
	deps := testDependencies(t, workingDir, providerImpl, &scriptedRunner{})
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := app.Run(
		context.Background(),
		deps,
		[]string{"--session", sessionPath, "-p", "hello"},
		&stdout,
		&stderr,
	)
	if exitCode != app.ExitFailure || stdout.Len() != 0 || providerImpl.CallCount() != 0 {
		t.Fatalf("Run() = code %d, stdout %q, provider calls %d", exitCode, stdout.String(), providerImpl.CallCount())
	}
	if !strings.Contains(stderr.String(), "open session "+sessionPath) || strings.Contains(stderr.String(), "goroutine") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	after, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("invalid session changed: %q", after)
	}
}

func TestRunRejectsArgumentsAndDependenciesBeforeSessionMutation(t *testing.T) {
	workingDir := t.TempDir()
	sessionParent := filepath.Join(workingDir, "must-not-exist")
	sessionPath := filepath.Join(sessionParent, "session.jsonl")
	validProvider := newScriptedProvider(t, fixedStep(t, textTerminal(t, "unused")))
	validDeps := testDependencies(t, workingDir, validProvider, &scriptedRunner{})
	invalidSessionIDDeps := validDeps
	invalidSessionIDDeps.SessionID = "   "

	for name, testCase := range map[string]struct {
		deps app.Dependencies
		args []string
	}{
		"missing prompt":  {deps: validDeps, args: []string{"-p", "--session", sessionPath}},
		"unknown flag":    {deps: validDeps, args: []string{"--future", "x", "--session", sessionPath}},
		"duplicate print": {deps: validDeps, args: []string{"-p", "one", "-p", "two", "--session", sessionPath}},
		"nil provider":    {deps: app.Dependencies{WorkingDir: workingDir}, args: []string{"-p", "hello", "--session", sessionPath}},
		"invalid session id": {
			deps: invalidSessionIDDeps,
			args: []string{"-p", "hello", "--session", sessionPath},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			if exitCode := app.Run(context.Background(), testCase.deps, testCase.args, &stdout, &stderr); exitCode != app.ExitFailure {
				t.Fatalf("exit = %d, want 1", exitCode)
			}
			if stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("stdout %q, stderr %q", stdout.String(), stderr.String())
			}
			if _, err := os.Stat(sessionPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected run changed session path: %v", err)
			}
			if _, err := os.Stat(sessionParent); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected run provisioned session parent: %v", err)
			}
		})
	}
	if validProvider.CallCount() != 0 {
		t.Fatalf("rejected runs called provider %d times", validProvider.CallCount())
	}
}

type scriptedRunner struct {
	output             []byte
	waitCancel         bool
	entered            chan struct{}
	markerPath         string
	cancelObservedPath string
	cancelReleasePath  string
	wantWorkingDir     string
	calls              atomic.Uint32
	once               sync.Once
}

func (r *scriptedRunner) Run(ctx context.Context, request tool.RunRequest, sink tool.OutputSink) (tool.ExitStatus, error) {
	r.calls.Add(1)
	if r.wantWorkingDir != "" && request.WorkingDir() != r.wantWorkingDir {
		return tool.UnknownExitStatus(), fmt.Errorf("runner working directory = %q, want %q", request.WorkingDir(), r.wantWorkingDir)
	}
	if r.markerPath != "" {
		if err := os.WriteFile(r.markerPath, []byte("ready"), 0o600); err != nil {
			return tool.UnknownExitStatus(), err
		}
	}
	if r.entered != nil {
		r.once.Do(func() { close(r.entered) })
	}
	if r.waitCancel {
		<-ctx.Done()
		if r.cancelObservedPath != "" {
			if err := os.WriteFile(r.cancelObservedPath, []byte("cancelled"), 0o600); err != nil {
				return tool.UnknownExitStatus(), err
			}
		}
		for r.cancelReleasePath != "" {
			if _, err := os.Stat(r.cancelReleasePath); err == nil {
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				return tool.UnknownExitStatus(), err
			}
			time.Sleep(5 * time.Millisecond)
		}
		return tool.UnknownExitStatus(), tool.NewRunInterruptedError(context.Cause(ctx))
	}
	if len(r.output) > 0 {
		if err := sink(append([]byte(nil), r.output...)); err != nil {
			return tool.UnknownExitStatus(), err
		}
	}
	return tool.NewExitStatus(0)
}

func testDependencies(
	t *testing.T,
	workingDir string,
	providerImpl provider.Provider,
	runner tool.Runner,
) app.Dependencies {
	t.Helper()
	model, err := provider.NewModelRef("scripted", "scripted", "app-test")
	if err != nil {
		t.Fatal(err)
	}
	var sessionTicks atomic.Int64
	var agentTicks atomic.Int64
	var entryIDs atomic.Uint64
	return app.Dependencies{
		Provider:     providerImpl,
		Model:        model,
		SystemPrompt: "app test system",
		WorkingDir:   workingDir,
		SessionID:    "app-test-session",
		SessionNow: func() time.Time {
			return appTestEpoch.Add(time.Duration(sessionTicks.Add(1)) * time.Millisecond)
		},
		NewSessionEntryID: func() (string, error) {
			return fmt.Sprintf("app-entry-%06d", entryIDs.Add(1)), nil
		},
		AgentNow: func() time.Time {
			return appTestEpoch.Add(time.Duration(agentTicks.Add(1)) * time.Millisecond)
		},
		BashRunner: runner,
	}
}

func newScriptedProvider(t *testing.T, steps ...provider.ScriptStep) *provider.ScriptedProvider {
	t.Helper()
	providerImpl, err := provider.NewScriptedProvider(provider.ScriptedConfig{
		ChunkRunes: 2,
		Clock:      func() time.Time { return appTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := providerImpl.SetResponses(steps); err != nil {
		t.Fatal(err)
	}
	return providerImpl
}

func fixedStep(t *testing.T, terminal llm.AssistantTerminal) provider.ScriptStep {
	t.Helper()
	step, err := provider.FixedResponseStep(terminal)
	if err != nil {
		t.Fatal(err)
	}
	return step
}

func factoryStep(t *testing.T, factory provider.ResponseFactory) provider.ScriptStep {
	t.Helper()
	step, err := provider.FactoryResponseStep(factory)
	if err != nil {
		t.Fatal(err)
	}
	return step
}

func textTerminal(t *testing.T, text string) llm.AssistantTextMessage {
	t.Helper()
	return textTerminalBlocks(t, text)
}

func textTerminalBlocks(t *testing.T, texts ...string) llm.AssistantTextMessage {
	t.Helper()
	blocks := make([]llm.TextBlock, len(texts))
	for index, text := range texts {
		block, err := llm.NewTextBlock(text)
		if err != nil {
			t.Fatal(err)
		}
		blocks[index] = block
	}
	terminal, err := llm.NewAssistantTextMessage(blocks, llm.FinishStop, testUsage(t), appTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	return terminal
}

func toolTerminal(t *testing.T, id, name string, arguments []byte) llm.AssistantToolUseMessage {
	t.Helper()
	call, err := llm.NewToolCallBlock(id, name, arguments)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := llm.NewAssistantToolUseMessage([]llm.AssistantBlock{call}, testUsage(t), appTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	return terminal
}

func testUsage(t *testing.T) llm.Usage {
	t.Helper()
	usage, err := llm.NewUsage(llm.UsageSpec{Input: 1, Output: 1})
	if err != nil {
		t.Fatal(err)
	}
	return usage
}

func textBlocks(blocks []llm.TextBlock) string {
	var result strings.Builder
	for _, block := range blocks {
		result.WriteString(block.Text())
	}
	return result.String()
}

func messageRoles(messages []llm.ConversationMessage) []llm.Role {
	roles := make([]llm.Role, len(messages))
	for index, message := range messages {
		roles[index] = message.Role()
	}
	return roles
}

var _ tool.Runner = (*scriptedRunner)(nil)
