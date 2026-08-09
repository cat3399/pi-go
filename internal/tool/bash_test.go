package tool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type runnerFunc func(context.Context, RunRequest, OutputSink) (ExitStatus, error)

func (function runnerFunc) Run(
	ctx context.Context,
	request RunRequest,
	sink OutputSink,
) (ExitStatus, error) {
	return function(ctx, request, sink)
}

func testExitStatus(t *testing.T, code int) ExitStatus {
	t.Helper()
	status, err := NewExitStatus(code)
	if err != nil {
		t.Fatal(err)
	}
	return status
}

func testBashInput(t *testing.T, command string, timeout *time.Duration) BashInput {
	t.Helper()
	input, err := NewBashInput(command, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func TestBashSuccessUsesFixedConfigurationAndMergedRunnerOrder(t *testing.T) {
	t.Parallel()
	workingDir := t.TempDir()
	environment := []string{
		"PATH=/controlled",
		"KEEP=original",
		"PI_SESSION_ID=secret",
		"PI_SESSION_FILE=secret-file",
		"PI_PROVIDER=secret-provider",
		"PI_MODEL=secret-model",
		"PI_REASONING_LEVEL=secret-reasoning",
	}
	var captured RunRequest
	runner := runnerFunc(func(_ context.Context, request RunRequest, sink OutputSink) (ExitStatus, error) {
		captured = request
		for _, chunk := range [][]byte{
			[]byte("stdout-1\n"),
			[]byte("stderr-1\n"),
			[]byte("stdout-2\n"),
		} {
			if err := sink(chunk); err != nil {
				return ExitStatus{}, err
			}
		}
		return testExitStatus(t, 0), nil
	})
	bash, err := NewBash(BashOptions{
		WorkingDir:    workingDir,
		Environment:   environment,
		Runner:        runner,
		ShellPath:     "/ignored/by/custom/runner",
		CommandPrefix: "prepare-shell",
	})
	if err != nil {
		t.Fatal(err)
	}
	environment[1] = "KEEP=mutated"

	result, err := bash.Execute(context.Background(), testBashInput(t, "do-work", nil))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.Succeeded() {
		t.Fatal("result is not successful")
	}
	if result.Text() != "stdout-1\nstderr-1\nstdout-2\n" {
		t.Fatalf("Text() = %q", result.Text())
	}
	if captured.Command() != "prepare-shell\ndo-work" {
		t.Fatalf("runner command = %q", captured.Command())
	}
	if captured.WorkingDir() != filepath.Clean(workingDir) {
		t.Fatalf("runner cwd = %q, want %q", captured.WorkingDir(), filepath.Clean(workingDir))
	}
	gotEnvironment := strings.Join(captured.Environment(), "\n")
	if !strings.Contains(gotEnvironment, "KEEP=original") || strings.Contains(gotEnvironment, "KEEP=mutated") {
		t.Fatalf("environment snapshot was not immutable:\n%s", gotEnvironment)
	}
	for name := range strippedSessionEnvironment {
		if strings.Contains(gotEnvironment, name+"=") {
			t.Fatalf("environment leaked %s:\n%s", name, gotEnvironment)
		}
	}
	code, ok := result.ExitCode()
	if !ok || code != 0 {
		t.Fatalf("ExitCode() = (%d, %v), want (0, true)", code, ok)
	}
}

func TestBashPerCallExecutionContextInjectsSessionAndOverlaysEnvironment(t *testing.T) {
	t.Parallel()
	workingDir := t.TempDir()
	callDir := filepath.Join(workingDir, "call")
	if err := os.Mkdir(callDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var captured RunRequest
	bash, err := NewBash(BashOptions{
		WorkingDir: workingDir,
		Environment: []string{
			"KEEP=base",
			"PI_SESSION_ID=must-not-leak",
			"PI_MODEL=must-not-leak",
		},
		Runner: runnerFunc(func(_ context.Context, request RunRequest, _ OutputSink) (ExitStatus, error) {
			captured = request
			return testExitStatus(t, 0), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	execution := BashExecutionContext{
		WorkingDir:  "call",
		Environment: map[string]string{"KEEP": "call", "EXTRA": "value", "PI_MODEL": "hook-model"},
		SessionEnvironment: &BashSessionEnvironment{
			SessionID: "session-id", SessionFile: "session.jsonl", Provider: "provider",
			Model: "session-model", ReasoningLevel: "high",
		},
	}
	ctx := WithBashExecutionContext(context.Background(), execution)
	// The context owns a snapshot rather than the caller's mutable maps.
	execution.Environment["KEEP"] = "mutated"
	execution.SessionEnvironment.SessionID = "mutated"
	if _, err := bash.ExecuteJSON(ctx, []byte(`{"command":"true"}`)); err != nil {
		t.Fatal(err)
	}
	if captured.WorkingDir() != callDir {
		t.Fatalf("working directory = %q, want %q", captured.WorkingDir(), callDir)
	}
	got := map[string]string{}
	for _, entry := range captured.Environment() {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			got[name] = value
		}
	}
	want := map[string]string{
		"KEEP": "call", "EXTRA": "value", "PI_SESSION_ID": "session-id",
		"PI_SESSION_FILE": "session.jsonl", "PI_PROVIDER": "provider",
		"PI_MODEL": "hook-model", "PI_REASONING_LEVEL": "high",
	}
	for name, value := range want {
		if got[name] != value {
			t.Fatalf("%s = %q, want %q; environment = %#v", name, got[name], value, got)
		}
	}
}

func TestBashPerCallEnvironmentOverlayRemovesDuplicateBaseEntries(t *testing.T) {
	t.Parallel()
	workingDir := t.TempDir()
	var captured RunRequest
	bash, err := NewBash(BashOptions{
		WorkingDir:  workingDir,
		Environment: []string{"DUP=first", "KEEP=value", "DUP=last"},
		Runner: runnerFunc(func(_ context.Context, request RunRequest, _ OutputSink) (ExitStatus, error) {
			captured = request
			return testExitStatus(t, 0), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bash.ExecuteWithContext(context.Background(), testBashInput(t, "true", nil), BashExecutionContext{
		Environment: map[string]string{"DUP": "overlay"},
	}); err != nil {
		t.Fatal(err)
	}
	duplicates := 0
	for _, entry := range captured.Environment() {
		name, value, ok := strings.Cut(entry, "=")
		if ok && name == "DUP" {
			duplicates++
			if value != "overlay" {
				t.Fatalf("DUP = %q, want overlay", value)
			}
		}
	}
	if duplicates != 1 {
		t.Fatalf("DUP entries = %d; environment = %#v", duplicates, captured.Environment())
	}
}

func TestBashPerCallExecutionContextRejectsNUL(t *testing.T) {
	t.Parallel()
	bash, err := NewBash(BashOptions{
		WorkingDir: t.TempDir(), Environment: []string{},
		Runner: runnerFunc(func(context.Context, RunRequest, OutputSink) (ExitStatus, error) {
			t.Fatal("runner must not be called")
			return ExitStatus{}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, execution := range []BashExecutionContext{
		{WorkingDir: "bad\x00cwd"},
		{Environment: map[string]string{"BAD": "bad\x00value"}},
		{Environment: map[string]string{"BAD\x00KEY": "value"}},
	} {
		_, err := bash.ExecuteWithContext(context.Background(), testBashInput(t, "true", nil), execution)
		var failure *BashFailure
		if !errors.As(err, &failure) || failure.Kind() != FailureInvalidInput {
			t.Fatalf("error = %v, want invalid input", err)
		}
	}
}

func TestBashSuccessWithNoOutput(t *testing.T) {
	t.Parallel()
	bash := newFakeBash(t, runnerFunc(func(context.Context, RunRequest, OutputSink) (ExitStatus, error) {
		return testExitStatus(t, 0), nil
	}))
	result, err := bash.Execute(context.Background(), testBashInput(t, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	if result.Text() != "(no output)" {
		t.Fatalf("Text() = %q, want no-output marker", result.Text())
	}
}

func TestBashResultZeroValueIsNotSuccessful(t *testing.T) {
	t.Parallel()
	var result BashResult
	if result.Settled() || result.Succeeded() {
		t.Fatalf("zero result was accepted as settled success: %#v", result)
	}
}

func TestBashPreservesProviderVisibleControlAndANSIBYtes(t *testing.T) {
	t.Parallel()
	output := "\x1b[31mred\x1b[0m\r\n\x00"
	bash := newFakeBash(t, runnerFunc(func(_ context.Context, _ RunRequest, sink OutputSink) (ExitStatus, error) {
		if err := sink([]byte(output)); err != nil {
			return ExitStatus{}, err
		}
		return testExitStatus(t, 0), nil
	}))
	result, err := bash.Execute(context.Background(), testBashInput(t, "binary", nil))
	if err != nil {
		t.Fatal(err)
	}
	if result.Text() != output {
		t.Fatalf("provider-visible output = %q, want exact %q", result.Text(), output)
	}
}

func TestBashExecuteJSONOwnsArgumentValidationBoundary(t *testing.T) {
	t.Parallel()
	called := false
	bash := newFakeBash(t, runnerFunc(func(context.Context, RunRequest, OutputSink) (ExitStatus, error) {
		called = true
		return testExitStatus(t, 0), nil
	}))
	if bash.Name() != BashToolName {
		t.Fatalf("Name() = %q", bash.Name())
	}
	result, err := bash.ExecuteJSON(context.Background(), []byte("{\"timeout\":1}"))
	var failure *BashFailure
	if !errors.As(err, &failure) || failure.Kind() != FailureInvalidInput {
		t.Fatalf("error = %v, want invalid-input failure", err)
	}
	if !errors.Is(err, ErrInvalidBashInput) || result.Text() == "" {
		t.Fatalf("invalid input lost diagnostic/cause: result=%#v error=%v", result, err)
	}
	if called {
		t.Fatal("invalid JSON reached runner")
	}
}

func TestBashNonzeroRetainsOutputAndTypedCause(t *testing.T) {
	t.Parallel()
	bash := newFakeBash(t, runnerFunc(func(_ context.Context, _ RunRequest, sink OutputSink) (ExitStatus, error) {
		if err := sink([]byte("diagnostic\n")); err != nil {
			return ExitStatus{}, err
		}
		return testExitStatus(t, 17), nil
	}))
	result, err := bash.Execute(context.Background(), testBashInput(t, "exit 17", nil))
	var failure *BashFailure
	if !errors.As(err, &failure) || failure.Kind() != FailureExitStatus {
		t.Fatalf("Execute() error = %v, want exit-status BashFailure", err)
	}
	var exitError *ExitCodeError
	if !errors.As(err, &exitError) || exitError.Code() != 17 {
		t.Fatalf("Execute() error does not retain ExitCodeError: %v", err)
	}
	if result.Text() != "diagnostic\n\n\nCommand exited with code 17" {
		t.Fatalf("Text() = %q", result.Text())
	}
	code, ok := result.ExitCode()
	if !ok || code != 17 {
		t.Fatalf("ExitCode() = (%d, %v)", code, ok)
	}
	if failure.Result().Text() != result.Text() {
		t.Fatal("returned result and failure result differ")
	}
}

func TestBashRunnerFailureCategoriesAndOutput(t *testing.T) {
	t.Parallel()
	cause := errors.New("fixture failure")
	tests := []struct {
		name      string
		runnerErr error
		wantKind  FailureKind
	}{
		{name: "setup", runnerErr: newRunnerFailure(RunnerFailureSetup, cause), wantKind: FailureSetup},
		{name: "spawn", runnerErr: newRunnerFailure(RunnerFailureSpawn, cause), wantKind: FailureSpawn},
		{name: "capture", runnerErr: newRunnerFailure(RunnerFailureCapture, cause), wantKind: FailureRunner},
		{name: "wait", runnerErr: newRunnerFailure(RunnerFailureWait, cause), wantKind: FailureRunner},
		{name: "cleanup", runnerErr: newRunnerFailure(RunnerFailureCleanup, cause), wantKind: FailureRunner},
		{name: "unknown", runnerErr: cause, wantKind: FailureRunner},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			bash := newFakeBash(t, runnerFunc(func(_ context.Context, _ RunRequest, sink OutputSink) (ExitStatus, error) {
				if err := sink([]byte("partial")); err != nil {
					return ExitStatus{}, err
				}
				return ExitStatus{}, test.runnerErr
			}))
			result, err := bash.Execute(context.Background(), testBashInput(t, "fixture", nil))
			var failure *BashFailure
			if !errors.As(err, &failure) || failure.Kind() != test.wantKind {
				t.Fatalf("error = %v, want kind %s", err, test.wantKind)
			}
			if !errors.Is(err, cause) {
				t.Fatalf("error does not retain cause: %v", err)
			}
			if !strings.Contains(result.Text(), "partial") {
				t.Fatalf("failure lost captured output: %q", result.Text())
			}
		})
	}
}

func TestBashFailureTextIsAlwaysValidUTF8(t *testing.T) {
	t.Parallel()
	invalid := errors.New("runner path \xff failed")
	bash := newFakeBash(t, runnerFunc(func(context.Context, RunRequest, OutputSink) (ExitStatus, error) {
		return ExitStatus{}, newRunnerFailure(RunnerFailureSpawn, invalid)
	}))
	result, err := bash.Execute(context.Background(), testBashInput(t, "fixture", nil))
	if err == nil {
		t.Fatal("invalid-byte runner failure unexpectedly succeeded")
	}
	if !utf8.ValidString(result.Text()) || !strings.Contains(result.Text(), "\ufffd") {
		t.Fatalf("failure text is not sanitized UTF-8: %q", result.Text())
	}
}

func TestBashRejectsInvalidUTF8Configuration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		options BashOptions
	}{
		{name: "shell", options: BashOptions{ShellPath: "shell-\xff"}},
		{name: "prefix", options: BashOptions{CommandPrefix: "prefix-\xff"}},
		{name: "artifact", options: BashOptions{ArtifactDirectory: "artifact-\xff"}},
		{name: "environment", options: BashOptions{Environment: []string{"VALUE=\xff"}}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			test.options.WorkingDir = t.TempDir()
			if test.options.Environment == nil {
				test.options.Environment = []string{}
			}
			if _, err := NewBash(test.options); !errors.Is(err, ErrInvalidBashOptions) {
				t.Fatalf("NewBash() error = %v, want invalid options", err)
			}
		})
	}
}

func TestBashMissingWorkingDirectoryDoesNotCallRunner(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	called := false
	bash, err := NewBash(BashOptions{
		WorkingDir:  missing,
		Environment: []string{},
		Runner: runnerFunc(func(context.Context, RunRequest, OutputSink) (ExitStatus, error) {
			called = true
			return ExitStatus{}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = bash.Execute(context.Background(), testBashInput(t, "echo no", nil))
	var failure *BashFailure
	if !errors.As(err, &failure) || failure.Kind() != FailureWorkingDirectory {
		t.Fatalf("error = %v, want working-directory failure", err)
	}
	if called {
		t.Fatal("runner was called for missing working directory")
	}
}

func TestBashTimeoutAndCancellationAreDistinct(t *testing.T) {
	t.Parallel()
	waitingRunner := runnerFunc(func(ctx context.Context, _ RunRequest, _ OutputSink) (ExitStatus, error) {
		<-ctx.Done()
		return ExitStatus{}, NewRunInterruptedError(context.Cause(ctx))
	})

	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		bash := newFakeBash(t, waitingRunner)
		timeout := 15 * time.Millisecond
		result, err := bash.Execute(context.Background(), testBashInput(t, "wait", &timeout))
		var failure *BashFailure
		if !errors.As(err, &failure) || failure.Kind() != FailureTimeout {
			t.Fatalf("error = %v, want timeout", err)
		}
		if !errors.Is(err, ErrCommandTimedOut) {
			t.Fatalf("timeout cause was not retained: %v", err)
		}
		if result.Text() != "Command timed out after 0.015 seconds" {
			t.Fatalf("Text() = %q", result.Text())
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		t.Parallel()
		bash := newFakeBash(t, waitingRunner)
		ctx, cancel := context.WithCancelCause(context.Background())
		cause := errors.New("user interrupted")
		done := make(chan struct{})
		var result BashResult
		var err error
		go func() {
			defer close(done)
			result, err = bash.Execute(ctx, testBashInput(t, "wait", nil))
		}()
		cancel(cause)
		<-done
		var failure *BashFailure
		if !errors.As(err, &failure) || failure.Kind() != FailureCancelled {
			t.Fatalf("error = %v, want cancellation", err)
		}
		if !errors.Is(err, cause) {
			t.Fatalf("cancellation cause was not retained: %v", err)
		}
		if result.Text() != "Command aborted" {
			t.Fatalf("Text() = %q", result.Text())
		}
	})

	t.Run("caller cancellation wins while a tool timeout is settling", func(t *testing.T) {
		t.Parallel()
		timeoutObserved := make(chan struct{})
		releaseRunner := make(chan struct{})
		runner := runnerFunc(func(ctx context.Context, _ RunRequest, _ OutputSink) (ExitStatus, error) {
			<-ctx.Done()
			close(timeoutObserved)
			<-releaseRunner
			return ExitStatus{}, NewRunInterruptedError(context.Cause(ctx))
		})
		bash := newFakeBash(t, runner)
		ctx, cancel := context.WithCancelCause(context.Background())
		callerCause := errors.New("caller cancelled during timeout cleanup")
		timeout := 15 * time.Millisecond
		done := make(chan struct{})
		var result BashResult
		var err error
		go func() {
			defer close(done)
			result, err = bash.Execute(ctx, testBashInput(t, "wait", &timeout))
		}()
		<-timeoutObserved
		cancel(callerCause)
		close(releaseRunner)
		<-done

		var failure *BashFailure
		if !errors.As(err, &failure) || failure.Kind() != FailureCancelled {
			t.Fatalf("error = %v, want cancellation", err)
		}
		if !errors.Is(err, callerCause) || result.Text() != "Command aborted" {
			t.Fatalf("cancel precedence lost cause/text: result=%q error=%v", result.Text(), err)
		}
	})
}

func TestBashIgnoresConcurrentLateOutputAfterRunnerReturns(t *testing.T) {
	t.Parallel()
	var lateSink OutputSink
	bash := newFakeBash(t, runnerFunc(func(_ context.Context, _ RunRequest, sink OutputSink) (ExitStatus, error) {
		lateSink = sink
		if err := sink([]byte("before\n")); err != nil {
			return ExitStatus{}, err
		}
		return testExitStatus(t, 0), nil
	}))
	result, err := bash.Execute(context.Background(), testBashInput(t, "late", nil))
	if err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := lateSink([]byte("late\n")); err != nil {
				t.Errorf("late sink returned error: %v", err)
			}
		}()
	}
	wait.Wait()
	if result.Text() != "before\n" || result.CapturedOutput() != "before\n" {
		t.Fatalf("late output mutated settled result: %#v", result)
	}
}

func TestBashArtifactFailureIsTypedAndDoesNotExposePath(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "broad")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	bash, err := NewBash(BashOptions{
		WorkingDir:        t.TempDir(),
		Environment:       []string{},
		ArtifactDirectory: root,
		MaxOutputBytes:    4,
		Runner: runnerFunc(func(_ context.Context, _ RunRequest, sink OutputSink) (ExitStatus, error) {
			if err := sink([]byte("too much output")); err != nil {
				return ExitStatus{}, err
			}
			return testExitStatus(t, 0), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := bash.Execute(context.Background(), testBashInput(t, "chatty", nil))
	var failure *BashFailure
	if !errors.As(err, &failure) || failure.Kind() != FailureArtifact {
		t.Fatalf("error = %v, want artifact failure", err)
	}
	if !errors.Is(err, ErrArtifactSecurity) {
		t.Fatalf("error does not retain artifact security cause: %v", err)
	}
	if _, ok := result.FullOutputPath(); ok {
		t.Fatalf("failed artifact exposed path: %#v", result)
	}
}

func TestBashTruncatedSuccessPersistsEveryRawChunk(t *testing.T) {
	t.Parallel()
	var raw bytes.Buffer
	runner := runnerFunc(func(_ context.Context, _ RunRequest, sink OutputSink) (ExitStatus, error) {
		for line := 1; line <= 4000; line++ {
			chunk := []byte(fmt.Sprintf("line-%04d\n", line))
			raw.Write(chunk)
			if err := sink(chunk); err != nil {
				return ExitStatus{}, err
			}
		}
		return testExitStatus(t, 0), nil
	})
	bash := newFakeBash(t, runner)
	result, err := bash.Execute(context.Background(), testBashInput(t, "many-lines", nil))
	if err != nil {
		t.Fatal(err)
	}
	truncation := result.Truncation()
	by, ok := truncation.TruncatedBy()
	if !truncation.Truncated() ||
		!ok ||
		by != TruncatedByLines ||
		truncation.TotalLines() != 4000 ||
		truncation.OutputLines() != 2000 {
		t.Fatalf("truncation = %#v", truncation)
	}
	if !strings.HasPrefix(result.CapturedOutput(), "line-2001\n") ||
		!strings.HasSuffix(result.CapturedOutput(), "line-4000") {
		t.Fatalf("captured tail boundaries are wrong: %q ... %q",
			result.CapturedOutput()[:min(20, len(result.CapturedOutput()))],
			result.CapturedOutput()[max(0, len(result.CapturedOutput())-20):],
		)
	}
	path, ok := result.FullOutputPath()
	if !ok {
		t.Fatal("truncated result has no artifact path")
	}
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(full, raw.Bytes()) {
		t.Fatalf("artifact length/content = %d bytes, want %d exact bytes", len(full), raw.Len())
	}
	if !strings.Contains(result.Text(), "[Showing lines 2001-4000 of 4000. Full output: "+path+"]") {
		t.Fatalf("result footer = %q", result.Text()[max(0, len(result.Text())-200):])
	}
}

func TestBashTruncatedTimeoutRetainsArtifactPathInFailureText(t *testing.T) {
	t.Parallel()
	runner := runnerFunc(func(_ context.Context, _ RunRequest, sink OutputSink) (ExitStatus, error) {
		for line := 1; line <= 3000; line++ {
			if err := sink([]byte(fmt.Sprintf("%d\n", line))); err != nil {
				return ExitStatus{}, err
			}
		}
		return ExitStatus{}, NewRunInterruptedError(ErrCommandTimedOut)
	})
	bash := newFakeBash(t, runner)
	timeout := 5 * time.Second
	result, err := bash.Execute(context.Background(), testBashInput(t, "chatty-timeout", &timeout))
	var failure *BashFailure
	if !errors.As(err, &failure) || failure.Kind() != FailureTimeout {
		t.Fatalf("error = %v, want timeout", err)
	}
	path, ok := result.FullOutputPath()
	if !ok || !strings.Contains(result.Text(), "Full output: "+path) {
		t.Fatalf("timeout failure lost artifact path: %q", result.Text())
	}
	if !strings.HasSuffix(result.Text(), "Command timed out after 5 seconds") {
		t.Fatalf("timeout status = %q", result.Text()[max(0, len(result.Text())-100):])
	}
}

func TestBashByteTruncationFooterDistinguishesPartialLastLine(t *testing.T) {
	t.Parallel()
	bash, err := NewBash(BashOptions{
		WorkingDir:     t.TempDir(),
		Environment:    []string{},
		MaxOutputBytes: 5,
		Runner: runnerFunc(func(_ context.Context, _ RunRequest, sink OutputSink) (ExitStatus, error) {
			if err := sink([]byte("prefix\n€€€")); err != nil {
				return ExitStatus{}, err
			}
			return testExitStatus(t, 0), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := bash.Execute(context.Background(), testBashInput(t, "long-line", nil))
	if err != nil {
		t.Fatal(err)
	}
	path, ok := result.FullOutputPath()
	if !ok {
		t.Fatal("missing artifact path")
	}
	wantFooter := "[Showing last 3B of line 2 (line is 9B). Full output: " + path + "]"
	if result.CapturedOutput() != "€" || !strings.Contains(result.Text(), wantFooter) {
		t.Fatalf("result = %#v, want footer %q", result, wantFooter)
	}
}

func TestBashDoesNotCreateArtifactWhenDecodedOutputFitsLimits(t *testing.T) {
	t.Parallel()
	bash, err := NewBash(BashOptions{
		WorkingDir:     t.TempDir(),
		Environment:    []string{},
		MaxOutputBytes: 4,
		Runner: runnerFunc(func(_ context.Context, _ RunRequest, sink OutputSink) (ExitStatus, error) {
			if err := sink([]byte{0xef, 0xbb, 0xbf, 'a', 'b', 'c', 'd'}); err != nil {
				return ExitStatus{}, err
			}
			return testExitStatus(t, 0), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := bash.Execute(context.Background(), testBashInput(t, "bom", nil))
	if err != nil {
		t.Fatal(err)
	}
	if result.Text() != "abcd" || result.Truncation().Truncated() {
		t.Fatalf("decoded exact-limit result = %#v", result)
	}
	if _, ok := result.FullOutputPath(); ok {
		t.Fatalf("non-truncated result exposed internal spool path: %#v", result)
	}
}

func TestBashConcurrentExecutionsOwnIndependentCaptureAndArtifacts(t *testing.T) {
	t.Parallel()
	runner := runnerFunc(func(_ context.Context, request RunRequest, sink OutputSink) (ExitStatus, error) {
		for line := 0; line < 4; line++ {
			if err := sink([]byte(request.Command() + "\n")); err != nil {
				return ExitStatus{}, err
			}
		}
		return testExitStatus(t, 0), nil
	})
	bash, err := NewBash(BashOptions{
		WorkingDir:     t.TempDir(),
		Environment:    []string{},
		Runner:         runner,
		MaxOutputLines: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	const executions = 16
	type outcome struct {
		command string
		result  BashResult
		err     error
	}
	outcomes := make(chan outcome, executions)
	for index := 0; index < executions; index++ {
		command := fmt.Sprintf("command-%02d", index)
		input := testBashInput(t, command, nil)
		go func() {
			result, err := bash.Execute(context.Background(), input)
			outcomes <- outcome{command: command, result: result, err: err}
		}()
	}
	paths := make(map[string]struct{}, executions)
	for range executions {
		outcome := <-outcomes
		if outcome.err != nil {
			t.Fatalf("%s failed: %v", outcome.command, outcome.err)
		}
		path, ok := outcome.result.FullOutputPath()
		if !ok {
			t.Fatalf("%s has no artifact", outcome.command)
		}
		if _, duplicate := paths[path]; duplicate {
			t.Fatalf("artifact path reused across executions: %s", path)
		}
		paths[path] = struct{}{}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Repeat(outcome.command+"\n", 4)
		if string(raw) != want {
			t.Fatalf("%s artifact = %q, want %q", outcome.command, raw, want)
		}
	}
}

func TestNewBashRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		options BashOptions
	}{
		{name: "empty cwd", options: BashOptions{}},
		{name: "NUL cwd", options: BashOptions{WorkingDir: "bad\x00cwd"}},
		{name: "malformed environment", options: BashOptions{WorkingDir: t.TempDir(), Environment: []string{"BROKEN"}}},
		{name: "negative lines", options: BashOptions{WorkingDir: t.TempDir(), Environment: []string{}, MaxOutputLines: -1}},
		{name: "negative bytes", options: BashOptions{WorkingDir: t.TempDir(), Environment: []string{}, MaxOutputBytes: -1}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewBash(test.options); !errors.Is(err, ErrInvalidBashOptions) {
				t.Fatalf("NewBash() error = %v, want ErrInvalidBashOptions", err)
			}
		})
	}
}

func newFakeBash(t *testing.T, runner Runner) *Bash {
	t.Helper()
	bash, err := NewBash(BashOptions{
		WorkingDir:  t.TempDir(),
		Environment: []string{},
		Runner:      runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	return bash
}
