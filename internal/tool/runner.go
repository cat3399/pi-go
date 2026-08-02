package tool

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// OutputSink is called serially in the order bytes are received by a Runner.
// Returning an error asks the Runner to terminate and reap the process tree.
type OutputSink func([]byte) error

// RunRequest is an immutable snapshot passed to the process boundary.
type RunRequest struct {
	command     string
	workingDir  string
	environment []string
}

func newRunRequest(command, workingDir string, environment []string) RunRequest {
	return RunRequest{
		command:     command,
		workingDir:  workingDir,
		environment: append([]string(nil), environment...),
	}
}

func (r RunRequest) Command() string    { return r.command }
func (r RunRequest) WorkingDir() string { return r.workingDir }
func (r RunRequest) Environment() []string {
	return append([]string(nil), r.environment...)
}

// ExitStatus represents direct-shell termination. A process killed by its own
// signal may have no portable numeric exit code.
type ExitStatus struct {
	code    int
	hasCode bool
}

func NewExitStatus(code int) (ExitStatus, error) {
	if code < 0 {
		return ExitStatus{}, fmt.Errorf("invalid exit code %d", code)
	}
	return ExitStatus{code: code, hasCode: true}, nil
}

func UnknownExitStatus() ExitStatus {
	return ExitStatus{}
}

func (s ExitStatus) Code() (int, bool) {
	return s.code, s.hasCode
}

// Runner owns process start, stream capture, cancellation, tree termination,
// direct-child reaping, and pipe settlement. It must not call sink after Run
// returns; Bash still gates late calls defensively at its boundary.
type Runner interface {
	Run(context.Context, RunRequest, OutputSink) (ExitStatus, error)
}

type RunnerFailureStage uint8

const (
	RunnerFailureSetup RunnerFailureStage = iota + 1
	RunnerFailureSpawn
	RunnerFailureCapture
	RunnerFailureWait
	RunnerFailureCleanup
)

func (s RunnerFailureStage) String() string {
	switch s {
	case RunnerFailureSetup:
		return "setup"
	case RunnerFailureSpawn:
		return "spawn"
	case RunnerFailureCapture:
		return "capture"
	case RunnerFailureWait:
		return "wait"
	case RunnerFailureCleanup:
		return "cleanup"
	default:
		return "unknown"
	}
}

type RunnerFailure struct {
	stage RunnerFailureStage
	cause error
}

func newRunnerFailure(stage RunnerFailureStage, cause error) *RunnerFailure {
	if cause == nil {
		panic("tool: RunnerFailure requires a cause")
	}
	return &RunnerFailure{stage: stage, cause: cause}
}

func (e *RunnerFailure) Error() string {
	if e == nil {
		return "<nil runner failure>"
	}
	return fmt.Sprintf("bash process %s failed: %s", e.stage, safeErrorText(e.cause))
}

func (e *RunnerFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *RunnerFailure) Stage() RunnerFailureStage {
	if e == nil {
		return 0
	}
	return e.stage
}

type RunInterruptedError struct {
	cause error
}

func newRunInterruptedError(cause error) *RunInterruptedError {
	if cause == nil {
		cause = context.Canceled
	}
	return &RunInterruptedError{cause: cause}
}

func NewRunInterruptedError(cause error) *RunInterruptedError {
	return newRunInterruptedError(cause)
}

func (e *RunInterruptedError) Error() string {
	if e == nil {
		return "bash process interrupted"
	}
	return "bash process interrupted: " + safeErrorText(e.cause)
}

func (e *RunInterruptedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func safeErrorText(err error) (text string) {
	if err == nil {
		return "unknown error"
	}
	defer func() {
		if recover() != nil {
			text = fmt.Sprintf("error value of type %T", err)
		}
	}()
	text = err.Error()
	if text == "" {
		return fmt.Sprintf("error value of type %T", err)
	}
	return strings.ToValidUTF8(text, "\ufffd")
}

func isInterruption(err error) bool {
	var interrupted *RunInterruptedError
	return errors.As(err, &interrupted) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrCommandTimedOut)
}
