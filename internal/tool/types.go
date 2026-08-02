package tool

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidBashOptions = errors.New("invalid bash options")
	ErrCommandTimedOut    = errors.New("bash command timed out")
)

type FailureKind uint8

const (
	FailureInvalidInput FailureKind = iota + 1
	FailureWorkingDirectory
	FailureSetup
	FailureSpawn
	FailureExitStatus
	FailureTimeout
	FailureCancelled
	FailureArtifact
	FailureRunner
)

func (k FailureKind) String() string {
	switch k {
	case FailureInvalidInput:
		return "invalidInput"
	case FailureWorkingDirectory:
		return "workingDirectory"
	case FailureSetup:
		return "setup"
	case FailureSpawn:
		return "spawn"
	case FailureExitStatus:
		return "exitStatus"
	case FailureTimeout:
		return "timeout"
	case FailureCancelled:
		return "cancelled"
	case FailureArtifact:
		return "artifact"
	case FailureRunner:
		return "runner"
	default:
		return "unknown"
	}
}

func (k FailureKind) valid() bool {
	return k >= FailureInvalidInput && k <= FailureRunner
}

type TruncatedBy uint8

const (
	TruncatedByLines TruncatedBy = iota + 1
	TruncatedByBytes
)

func (b TruncatedBy) String() string {
	switch b {
	case TruncatedByLines:
		return "lines"
	case TruncatedByBytes:
		return "bytes"
	default:
		return "unknown"
	}
}

// Truncation is immutable metadata for the provider-visible output tail.
type Truncation struct {
	truncated       bool
	truncatedBy     TruncatedBy
	totalLines      uint64
	totalBytes      uint64
	outputLines     uint64
	outputBytes     uint64
	lastLinePartial bool
	maxLines        int
	maxBytes        int
}

func (t Truncation) Truncated() bool { return t.truncated }
func (t Truncation) TruncatedBy() (TruncatedBy, bool) {
	return t.truncatedBy, t.truncated
}
func (t Truncation) TotalLines() uint64    { return t.totalLines }
func (t Truncation) TotalBytes() uint64    { return t.totalBytes }
func (t Truncation) OutputLines() uint64   { return t.outputLines }
func (t Truncation) OutputBytes() uint64   { return t.outputBytes }
func (t Truncation) LastLinePartial() bool { return t.lastLinePartial }
func (t Truncation) MaxLines() int         { return t.maxLines }
func (t Truncation) MaxBytes() int         { return t.maxBytes }

// BashResult is the settled, immutable outcome of one invocation. Text is the
// content that the agent should place in the associated ToolResult.
type BashResult struct {
	text           string
	capturedOutput string
	settled        bool
	failureKind    FailureKind
	exitCode       int
	hasExitCode    bool
	truncation     Truncation
	fullOutputPath string
}

func (r BashResult) Text() string           { return r.text }
func (r BashResult) CapturedOutput() string { return r.capturedOutput }
func (r BashResult) Settled() bool          { return r.settled }
func (r BashResult) Succeeded() bool {
	return r.settled && r.failureKind == 0 && r.hasExitCode && r.exitCode == 0
}
func (r BashResult) FailureKind() (FailureKind, bool) {
	return r.failureKind, r.failureKind != 0
}
func (r BashResult) ExitCode() (int, bool) {
	return r.exitCode, r.hasExitCode
}
func (r BashResult) Truncation() Truncation { return r.truncation }
func (r BashResult) FullOutputPath() (string, bool) {
	return r.fullOutputPath, r.fullOutputPath != ""
}

// BashFailure retains a stable category, the provider-visible result, and the
// original cause for errors.Is/errors.As without deciding agent continuation.
type BashFailure struct {
	kind   FailureKind
	result BashResult
	cause  error
}

func newBashFailure(kind FailureKind, result BashResult, cause error) *BashFailure {
	if !kind.valid() {
		panic(fmt.Sprintf("tool: invalid failure kind %d", kind))
	}
	if cause == nil {
		panic("tool: BashFailure requires a cause")
	}
	result.failureKind = kind
	result.settled = true
	return &BashFailure{kind: kind, result: result, cause: cause}
}

func (e *BashFailure) Error() string {
	if e == nil {
		return "<nil bash failure>"
	}
	return e.result.text
}

func (e *BashFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *BashFailure) Kind() FailureKind {
	if e == nil {
		return 0
	}
	return e.kind
}

func (e *BashFailure) Result() BashResult {
	if e == nil {
		return BashResult{}
	}
	return e.result
}

func (e *BashFailure) Cause() error {
	if e == nil {
		return nil
	}
	return e.cause
}

type ExitCodeError struct {
	code int
}

func newExitCodeError(code int) *ExitCodeError {
	return &ExitCodeError{code: code}
}

func (e *ExitCodeError) Error() string {
	if e == nil {
		return "command exited unsuccessfully"
	}
	return fmt.Sprintf("command exited with code %d", e.code)
}

func (e *ExitCodeError) Code() int {
	if e == nil {
		return 0
	}
	return e.code
}
