package tool

import (
	"errors"
	"fmt"
	"strings"
)

// Filesystem suite failures are deliberately separate from Bash failures. A
// filesystem tool never falls back to a shell command: callers can reliably
// distinguish an unsupported/binary input from an I/O failure.
var (
	ErrInvalidFilesystemOptions     = errors.New("invalid filesystem tool options")
	ErrInvalidFilesystemInput       = errors.New("invalid filesystem tool input")
	ErrFilesystemPath               = errors.New("filesystem path error")
	ErrBinaryFile                   = errors.New("binary file is not supported by this text tool")
	ErrOperationCancelled           = errors.New("filesystem operation cancelled")
	ErrEditConflict                 = errors.New("edit conflict")
	ErrFilesystemToolNotFound       = errors.New("filesystem tool not found")
	ErrUnsupportedFilesystemFeature = errors.New("unsupported filesystem tool feature")
)

const (
	ReadToolName  = "read"
	WriteToolName = "write"
	EditToolName  = "edit"
	GrepToolName  = "grep"
	FindToolName  = "find"
	LsToolName    = "ls"

	DefaultFilesystemMaxLines = 2000
	DefaultFilesystemMaxBytes = 50 * 1024
	DefaultGrepLineRunes      = 500
	DefaultGrepMatches        = 100
	DefaultFindResults        = 1000
	DefaultLsEntries          = 500
)

// ToolResult is the provider-visible, immutable output of one filesystem
// operation. Details is intentionally JSON-shaped so an eventual tool-schema
// adapter can expose it without coupling this package to a provider dialect.
type ToolResult struct {
	Text    string
	Details map[string]any
}

func (r ToolResult) clone() ToolResult {
	r.Details = cloneDetails(r.Details)
	return r
}

func cloneDetails(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

// FilesystemOptions binds the suite to a working directory. It intentionally
// does not claim to sandbox it: absolute paths and .. retain upstream Pi's
// account-permission behavior. Consumers requiring confinement must install a
// filesystem-level sandbox before constructing the suite.
type FilesystemOptions struct {
	WorkingDir string
	MaxLines   int
	MaxBytes   int
}

func (o FilesystemOptions) validate() (FilesystemOptions, error) {
	if strings.TrimSpace(o.WorkingDir) == "" {
		return FilesystemOptions{}, fmt.Errorf("%w: working directory is required", ErrInvalidFilesystemOptions)
	}
	if o.MaxLines == 0 {
		o.MaxLines = DefaultFilesystemMaxLines
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = DefaultFilesystemMaxBytes
	}
	if o.MaxLines <= 0 || o.MaxBytes <= 0 {
		return FilesystemOptions{}, fmt.Errorf("%w: output limits must be positive", ErrInvalidFilesystemOptions)
	}
	return o, nil
}

// Edit describes one replacement. All replacements are resolved against the
// same pre-write snapshot, never incrementally.
type Edit struct {
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}
