package tool

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/llm"
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
	ErrFilesystemReadTooLarge       = errors.New("filesystem read exceeds input limit")
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
	// DefaultFilesystemMaxTextUnits matches buffer.constants.MAX_STRING_LENGTH
	// in the pinned Node 24 runtime. It is a decoded JavaScript UTF-16 string
	// boundary, not a raw file-byte limit.
	DefaultFilesystemMaxTextUnits int64 = 536_870_888
	// DefaultFilesystemMaxReadBytes is retained as a source-compatible alias.
	// MaxReadBytes likewise configures decoded UTF-16 units, never source bytes.
	DefaultFilesystemMaxReadBytes = DefaultFilesystemMaxTextUnits
	// The Go decoder runs in-process rather than in upstream's disposable image
	// worker. Limit decoded pixels to four times the 2000x2000 provider target
	// to prevent compressed-image allocation bombs; callers can explicitly
	// override it for a differently isolated decoder.
	DefaultFilesystemMaxImagePixels int64 = 16_000_000
	// This is the upstream 4.5 MiB base64 payload ceiling, chosen below the
	// strictest provider limit used by the pinned implementation.
	DefaultFilesystemMaxImageBytes = 4_718_592
	DefaultGrepLineRunes           = 500
	DefaultGrepMatches             = 100
	DefaultFindResults             = 1000
	DefaultLsEntries               = 500
)

// ToolResult is the provider-visible, immutable output of one filesystem
// operation. Details is intentionally JSON-shaped so an eventual tool-schema
// adapter can expose it without coupling this package to a provider dialect.
type ToolResult struct {
	Text string
	// Content is authoritative when non-nil. It mirrors pi's rich tool result
	// contract, allowing built-ins such as read to return image attachments
	// without flattening them into provider-visible text.
	Content []llm.ToolResultContentBlock
	Details map[string]any
}

func (r ToolResult) clone() ToolResult {
	r.Content = append([]llm.ToolResultContentBlock(nil), r.Content...)
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
// filesystem-level sandbox before constructing the suite. Cancellation closes
// opened file handles, but filesystem open/stat/ReadDir calls on a failed hard
// mount and individual codec calls cannot be interrupted portably; image work
// checks cancellation between decode, orientation, resize, and encode stages.
type FilesystemOptions struct {
	WorkingDir   string
	MaxLines     int
	MaxBytes     int
	MaxTextUnits int64
	// MaxReadBytes is the legacy name for MaxTextUnits. It no longer limits raw
	// image bytes or UTF-8 source bytes.
	MaxReadBytes int64
	// MaxImagePixels bounds decoded pixels even when AutoResizeImages is false.
	// Zero selects the configurable Go safety default of 16 million pixels.
	MaxImagePixels int64
	// MaxImageBytes bounds the base64 provider payload even when
	// AutoResizeImages is false. Zero selects the upstream 4.5 MiB ceiling.
	MaxImageBytes int
	// AutoResizeImages=false disables automatic resizing, not the configurable
	// decoded-pixel and encoded-output safety boundaries above.
	AutoResizeImages *bool
}

func (o FilesystemOptions) validate() (FilesystemOptions, error) {
	if strings.TrimSpace(o.WorkingDir) == "" {
		return FilesystemOptions{}, fmt.Errorf("%w: working directory is required", ErrInvalidFilesystemOptions)
	}
	if !utf8.ValidString(o.WorkingDir) || strings.IndexByte(o.WorkingDir, 0) >= 0 {
		return FilesystemOptions{}, fmt.Errorf("%w: working directory is invalid", ErrInvalidFilesystemOptions)
	}
	if o.MaxLines == 0 {
		o.MaxLines = DefaultFilesystemMaxLines
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = DefaultFilesystemMaxBytes
	}
	if o.MaxTextUnits != 0 && o.MaxReadBytes != 0 {
		return FilesystemOptions{}, fmt.Errorf("%w: MaxTextUnits and legacy MaxReadBytes cannot both be set", ErrInvalidFilesystemOptions)
	}
	if o.MaxTextUnits == 0 {
		o.MaxTextUnits = o.MaxReadBytes
	}
	if o.MaxTextUnits == 0 {
		o.MaxTextUnits = DefaultFilesystemMaxTextUnits
	}
	if o.MaxImagePixels == 0 {
		o.MaxImagePixels = DefaultFilesystemMaxImagePixels
	}
	if o.MaxImageBytes == 0 {
		o.MaxImageBytes = DefaultFilesystemMaxImageBytes
	}
	if o.MaxLines <= 0 || o.MaxBytes <= 0 || o.MaxTextUnits <= 0 || o.MaxImagePixels <= 0 || o.MaxImageBytes <= 0 {
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
