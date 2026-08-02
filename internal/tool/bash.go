package tool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var strippedSessionEnvironment = map[string]struct{}{
	"PI_SESSION_ID":      {},
	"PI_SESSION_FILE":    {},
	"PI_PROVIDER":        {},
	"PI_MODEL":           {},
	"PI_REASONING_LEVEL": {},
}

const BashToolName = "bash"

type BashOptions struct {
	WorkingDir        string
	Environment       []string
	Runner            Runner
	ShellPath         string
	ArtifactDirectory string
	MaxOutputLines    int
	MaxOutputBytes    int
}

// Bash is the production built-in Bash tool. It owns immutable execution
// configuration but no agent or session state.
type Bash struct {
	workingDir  string
	environment []string
	runner      Runner
	store       artifactFactory
	maxLines    int
	maxBytes    int
}

func NewBash(options BashOptions) (*Bash, error) {
	if options.WorkingDir == "" {
		return nil, fmt.Errorf("%w: working directory is required", ErrInvalidBashOptions)
	}
	workingDir, err := filepath.Abs(options.WorkingDir)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve working directory: %w", ErrInvalidBashOptions, err)
	}
	if !utf8.ValidString(workingDir) {
		return nil, fmt.Errorf("%w: working directory must be valid UTF-8", ErrInvalidBashOptions)
	}
	if err := validateBashPathOption("shell path", options.ShellPath); err != nil {
		return nil, err
	}
	if err := validateBashPathOption("artifact directory", options.ArtifactDirectory); err != nil {
		return nil, err
	}
	shellPath := options.ShellPath
	if shellPath != "" {
		if !filepath.IsAbs(shellPath) {
			shellPath = filepath.Join(workingDir, shellPath)
		}
		shellPath = filepath.Clean(shellPath)
	}

	environment := options.Environment
	if environment == nil {
		environment = os.Environ()
	}
	environment, err = snapshotBashEnvironment(environment)
	if err != nil {
		return nil, err
	}

	maxLines := options.MaxOutputLines
	if maxLines == 0 {
		maxLines = DefaultMaxOutputLines
	}
	maxBytes := options.MaxOutputBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxOutputBytes
	}
	store, err := newArtifactStore(options.ArtifactDirectory)
	if err != nil {
		return nil, err
	}
	if _, err := newOutputAccumulator(maxLines, maxBytes, store); err != nil {
		return nil, err
	}

	runner := options.Runner
	if runner == nil {
		runner = NewLocalRunner(LocalRunnerOptions{ShellPath: shellPath})
	}
	return &Bash{
		workingDir:  filepath.Clean(workingDir),
		environment: environment,
		runner:      runner,
		store:       store,
		maxLines:    maxLines,
		maxBytes:    maxBytes,
	}, nil
}

func (b *Bash) WorkingDir() string {
	if b == nil {
		return ""
	}
	return b.workingDir
}

func (b *Bash) Name() string {
	return BashToolName
}

// ExecuteJSON is the boundary used by the future tool dispatcher. It preserves
// invalid provider arguments as the same typed failure shape as execution
// failures, without making Bash own tool-call IDs or transcript state.
func (b *Bash) ExecuteJSON(ctx context.Context, arguments []byte) (BashResult, error) {
	input, err := DecodeBashInput(arguments)
	if err != nil {
		result := BashResult{text: safeErrorText(err)}
		failure := newBashFailure(FailureInvalidInput, result, err)
		return failure.Result(), failure
	}
	return b.Execute(ctx, input)
}

func (b *Bash) Execute(ctx context.Context, input BashInput) (BashResult, error) {
	if b == nil {
		cause := errors.New("bash tool is nil")
		result := BashResult{text: "Bash tool is not configured"}
		failure := newBashFailure(FailureSetup, result, cause)
		return failure.Result(), failure
	}
	if err := input.validate(); err != nil {
		result := BashResult{text: safeErrorText(err)}
		failure := newBashFailure(FailureInvalidInput, result, err)
		return failure.Result(), failure
	}
	if ctx == nil {
		cause := errors.New("context is nil")
		result := BashResult{text: "Command aborted"}
		failure := newBashFailure(FailureCancelled, result, cause)
		return failure.Result(), failure
	}
	if cause := context.Cause(ctx); cause != nil {
		result := BashResult{text: "Command aborted"}
		failure := newBashFailure(FailureCancelled, result, cause)
		return failure.Result(), failure
	}
	if err := validateWorkingDirectory(b.workingDir); err != nil {
		result := BashResult{text: safeErrorText(err)}
		failure := newBashFailure(FailureWorkingDirectory, result, err)
		return failure.Result(), failure
	}

	accumulator, err := newOutputAccumulator(b.maxLines, b.maxBytes, b.store)
	if err != nil {
		result := BashResult{text: safeErrorText(err)}
		failure := newBashFailure(FailureSetup, result, err)
		return failure.Result(), failure
	}

	runContext, cancelRun := context.WithCancelCause(ctx)
	var timeoutTimer *time.Timer
	if timeout, ok := input.Timeout(); ok {
		timeoutTimer = time.AfterFunc(timeout, func() {
			cancelRun(ErrCommandTimedOut)
		})
	}

	state := &bashOutputState{
		accepting:   true,
		accumulator: accumulator,
		cancel:      cancelRun,
	}
	status, runErr := b.runner.Run(
		runContext,
		newRunRequest(input.Command(), b.workingDir, b.environment),
		state.append,
	)
	if timeoutTimer != nil {
		timeoutTimer.Stop()
	}

	snapshot, outputErr := state.settle()
	cancelRun(context.Canceled)
	if outputErr != nil {
		// The cause retains the private/incomplete artifact path for trusted
		// diagnostics. Provider-visible text must not advertise an artifact
		// that failed to become complete and usable.
		result := resultFromSnapshot(snapshot, "", "Could not securely preserve full command output", false)
		failure := newBashFailure(FailureArtifact, result, outputErr)
		return failure.Result(), failure
	}

	parentCause := context.Cause(ctx)
	if parentCause != nil {
		// Match the user-visible cancellation contract through the complete
		// Bash settlement boundary. The tool's own timeout may have fired first,
		// but an observed caller cancellation wins while the runner is settling.
		runErr = errors.Join(newRunInterruptedError(parentCause), runErr)
	}
	if runErr != nil {
		kind, statusText := classifyRunnerFailure(runErr, input, parentCause)
		result := resultFromSnapshot(snapshot, "", statusText, true)
		if exitCode, ok := status.Code(); ok {
			result.exitCode = exitCode
			result.hasExitCode = true
		}
		failure := newBashFailure(kind, result, runErr)
		return failure.Result(), failure
	}

	exitCode, hasExitCode := status.Code()
	if !hasExitCode {
		cause := errors.New("direct shell terminated without a portable exit code")
		result := resultFromSnapshot(snapshot, "", "Command terminated without an exit code", true)
		failure := newBashFailure(FailureRunner, result, cause)
		return failure.Result(), failure
	}
	if exitCode != 0 {
		cause := newExitCodeError(exitCode)
		result := resultFromSnapshot(
			snapshot,
			"(no output)",
			fmt.Sprintf("Command exited with code %d", exitCode),
			true,
		)
		result.exitCode = exitCode
		result.hasExitCode = true
		failure := newBashFailure(FailureExitStatus, result, cause)
		return failure.Result(), failure
	}

	result := resultFromSnapshot(snapshot, "(no output)", "", true)
	result.exitCode = 0
	result.hasExitCode = true
	return result, nil
}

type bashOutputState struct {
	mu          sync.Mutex
	accepting   bool
	accumulator *outputAccumulator
	cancel      context.CancelCauseFunc
	firstErr    error
}

func (s *bashOutputState) append(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.accepting {
		return nil
	}
	if s.firstErr != nil {
		return s.firstErr
	}
	if err := s.accumulator.append(data); err != nil {
		s.firstErr = err
		s.cancel(err)
		return err
	}
	return nil
}

func (s *bashOutputState) settle() (outputSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accepting = false
	if err := s.accumulator.finish(); err != nil && s.firstErr == nil {
		s.firstErr = err
	}
	if err := s.accumulator.close(); err != nil && s.firstErr == nil {
		s.firstErr = err
	}
	return s.accumulator.snapshot(), s.firstErr
}

func validateWorkingDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("Working directory does not exist: %s\nCannot execute bash commands.", path)
		}
		return fmt.Errorf("Cannot inspect working directory %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("Working directory is not a directory: %s", path)
	}
	return nil
}

func snapshotBashEnvironment(source []string) ([]string, error) {
	snapshot := make([]string, 0, len(source))
	for index, entry := range source {
		if !utf8.ValidString(entry) {
			return nil, fmt.Errorf("%w: environment entry %d is not valid UTF-8", ErrInvalidBashOptions, index)
		}
		if strings.IndexByte(entry, 0) >= 0 {
			return nil, fmt.Errorf("%w: environment entry %d contains NUL", ErrInvalidBashOptions, index)
		}
		key, _, ok := splitEnvironmentEntry(entry)
		if !ok || key == "" {
			return nil, fmt.Errorf("%w: environment entry %d is malformed", ErrInvalidBashOptions, index)
		}
		strip := false
		for name := range strippedSessionEnvironment {
			if key == name || runtime.GOOS == "windows" && strings.EqualFold(key, name) {
				strip = true
				break
			}
		}
		if !strip {
			snapshot = append(snapshot, entry)
		}
	}
	return snapshot, nil
}

func validateBashPathOption(name, value string) error {
	if value == "" {
		return nil
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s must be valid UTF-8", ErrInvalidBashOptions, name)
	}
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%w: %s contains NUL", ErrInvalidBashOptions, name)
	}
	return nil
}

func classifyRunnerFailure(err error, input BashInput, parentCause error) (FailureKind, string) {
	if parentCause != nil {
		return FailureCancelled, "Command aborted"
	}
	if errors.Is(err, ErrCommandTimedOut) {
		timeout, _ := input.Timeout()
		return FailureTimeout, "Command timed out after " + formatSeconds(timeout) + " seconds"
	}
	if isInterruption(err) {
		return FailureCancelled, "Command aborted"
	}
	var runnerFailure *RunnerFailure
	if errors.As(err, &runnerFailure) {
		switch runnerFailure.Stage() {
		case RunnerFailureSetup:
			return FailureSetup, safeErrorText(runnerFailure.Unwrap())
		case RunnerFailureSpawn:
			return FailureSpawn, safeErrorText(runnerFailure.Unwrap())
		default:
			return FailureRunner, safeErrorText(runnerFailure)
		}
	}
	return FailureRunner, "Command execution failed: " + safeErrorText(err)
}

func resultFromSnapshot(
	snapshot outputSnapshot,
	emptyText string,
	status string,
	allowArtifact bool,
) BashResult {
	path := ""
	if allowArtifact && snapshot.truncation.truncated && snapshot.artifactComplete {
		path = snapshot.fullOutputPath
	}
	text := snapshot.content
	if text == "" {
		text = emptyText
	}
	if snapshot.truncation.truncated && path != "" {
		text = appendStatus(text, truncationFooter(snapshot, path))
	}
	if status != "" {
		text = appendStatus(text, status)
	}
	return BashResult{
		text:           text,
		capturedOutput: snapshot.content,
		settled:        true,
		truncation:     snapshot.truncation,
		fullOutputPath: path,
	}
}

func truncationFooter(snapshot outputSnapshot, path string) string {
	truncation := snapshot.truncation
	endLine := truncation.totalLines
	startLine := uint64(1)
	if truncation.outputLines <= endLine {
		startLine = endLine - truncation.outputLines + 1
	}
	if truncation.lastLinePartial {
		return fmt.Sprintf(
			"[Showing last %s of line %d (line is %s). Full output: %s]",
			formatByteSize(truncation.outputBytes),
			endLine,
			formatByteSize(snapshot.lastLineBytes),
			path,
		)
	}
	if truncation.truncatedBy == TruncatedByLines {
		return fmt.Sprintf(
			"[Showing lines %d-%d of %d. Full output: %s]",
			startLine,
			endLine,
			endLine,
			path,
		)
	}
	return fmt.Sprintf(
		"[Showing lines %d-%d of %d (%s limit). Full output: %s]",
		startLine,
		endLine,
		endLine,
		formatByteSize(uint64(truncation.maxBytes)),
		path,
	)
}

func appendStatus(text, status string) string {
	if text == "" {
		return status
	}
	return text + "\n\n" + status
}

func formatByteSize(bytes uint64) string {
	const (
		kib = 1024
		mib = 1024 * 1024
	)
	switch {
	case bytes < kib:
		return strconv.FormatUint(bytes, 10) + "B"
	case bytes < mib:
		return strconv.FormatFloat(float64(bytes)/kib, 'f', 1, 64) + "KB"
	default:
		return strconv.FormatFloat(float64(bytes)/mib, 'f', 1, 64) + "MB"
	}
}
