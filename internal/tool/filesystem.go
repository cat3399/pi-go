package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// FilesystemSuite is the production implementation of Pi's read/write/edit/
// grep/find/ls family. It owns path policy and mutation serialization, but no
// agent/session state or provider tool-call identifiers.
type FilesystemSuite struct {
	workingDir       string
	maxLines         int
	maxBytes         int
	maxTextUnits     int64
	maxImagePixels   int64
	maxImageBytes    int
	autoResizeImages bool
	mutations        *mutationQueue
	openReadFile     func(string) (*os.File, error)
	openSearchFile   func(string) (*os.File, int64, error)
	readSearchDir    func(string) ([]os.DirEntry, error)
}

var builtInFileMutations = newMutationQueue()

func NewFilesystemSuite(options FilesystemOptions) (*FilesystemSuite, error) {
	options, err := options.validate()
	if err != nil {
		return nil, err
	}
	workingDir, err := filepath.Abs(options.WorkingDir)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve working directory: %w", ErrInvalidFilesystemOptions, err)
	}
	info, err := os.Stat(workingDir)
	if err != nil {
		return nil, fmt.Errorf("%w: inspect working directory: %w", ErrInvalidFilesystemOptions, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: working directory is not a directory", ErrInvalidFilesystemOptions)
	}
	autoResizeImages := true
	if options.AutoResizeImages != nil {
		autoResizeImages = *options.AutoResizeImages
	}
	return &FilesystemSuite{
		workingDir: filepath.Clean(workingDir), maxLines: options.MaxLines, maxBytes: options.MaxBytes,
		maxTextUnits: options.MaxTextUnits, maxImagePixels: options.MaxImagePixels, maxImageBytes: options.MaxImageBytes,
		autoResizeImages: autoResizeImages, mutations: builtInFileMutations,
		openReadFile: openRegularReadFile, openSearchFile: openBoundedRegularSearchFile,
		readSearchDir: os.ReadDir,
	}, nil
}

func (s *FilesystemSuite) WorkingDir() string {
	if s == nil {
		return ""
	}
	return s.workingDir
}

func (s *FilesystemSuite) Names() []string {
	return []string{ReadToolName, WriteToolName, EditToolName, GrepToolName, FindToolName, LsToolName}
}

func (s *FilesystemSuite) Supports(name string) bool {
	for _, candidate := range s.Names() {
		if name == candidate {
			return true
		}
	}
	return false
}

// ExecuteJSON is the registry dispatch boundary. Its decoders apply the same
// last-value and extra-property behavior as JavaScript JSON.parse plus
// TypeBox's default Object schemas.
func (s *FilesystemSuite) ExecuteJSON(ctx context.Context, name string, raw []byte) (ToolResult, error) {
	if s == nil {
		return ToolResult{Text: "Filesystem tools are not configured"}, errors.New("filesystem suite is nil")
	}
	if ctx == nil {
		return ToolResult{Text: "Filesystem operation cancelled"}, ErrOperationCancelled
	}
	if err := context.Cause(ctx); err != nil {
		return ToolResult{Text: "Filesystem operation cancelled"}, errors.Join(ErrOperationCancelled, err)
	}
	switch name {
	case ReadToolName:
		input, err := decodeReadInput(raw)
		if err != nil {
			return inputError(err)
		}
		return s.Read(ctx, input)
	case WriteToolName:
		input, err := decodeWriteInput(raw)
		if err != nil {
			return inputError(err)
		}
		return s.Write(ctx, input)
	case EditToolName:
		input, err := decodeEditInput(raw)
		if err != nil {
			return inputError(err)
		}
		return s.Edit(ctx, input)
	case GrepToolName:
		input, err := decodeGrepInput(raw)
		if err != nil {
			return inputError(err)
		}
		return s.Grep(ctx, input)
	case FindToolName:
		input, err := decodeFindInput(raw)
		if err != nil {
			return inputError(err)
		}
		return s.Find(ctx, input)
	case LsToolName:
		input, err := decodeLsInput(raw)
		if err != nil {
			return inputError(err)
		}
		return s.Ls(ctx, input)
	default:
		return ToolResult{Text: fmt.Sprintf("Tool %s not found", name)}, fmt.Errorf("%w: %s", ErrFilesystemToolNotFound, name)
	}
}

func inputError(err error) (ToolResult, error) { return ToolResult{Text: err.Error()}, err }

type ReadInput struct {
	Path   string
	Offset *int
	Limit  *int
}
type WriteInput struct{ Path, Content string }
type EditInput struct {
	Path  string
	Edits []Edit
}

// PrepareEditArguments is the pre-schema compatibility transform used by pi's
// edit definition. Some models serialize edits as a JSON string, while older
// calls provide top-level oldText/newText fields. The transform must run before
// JSON Schema validation because the advertised schema requires edits[].
func PrepareEditArguments(arguments any) any {
	object, ok := arguments.(map[string]any)
	if !ok || object == nil {
		return arguments
	}
	prepared := make(map[string]any, len(object))
	for key, value := range object {
		prepared[key] = value
	}
	if encoded, ok := prepared["edits"].(string); ok {
		var parsed any
		if json.Unmarshal([]byte(encoded), &parsed) == nil {
			if edits, ok := parsed.([]any); ok {
				prepared["edits"] = edits
			}
		}
	}
	oldText, hasOldText := prepared["oldText"].(string)
	newText, hasNewText := prepared["newText"].(string)
	if !hasOldText || !hasNewText {
		return prepared
	}
	edits, _ := prepared["edits"].([]any)
	edits = append(append([]any(nil), edits...), map[string]any{"oldText": oldText, "newText": newText})
	prepared["edits"] = edits
	delete(prepared, "oldText")
	delete(prepared, "newText")
	return prepared
}

func (s *FilesystemSuite) Read(ctx context.Context, input ReadInput) (ToolResult, error) {
	return s.read(ctx, input)
}

func isBinary(data []byte) bool {
	if bytes.IndexByte(data, 0) >= 0 {
		return true
	}
	return false
}

func truncationDetails(value FilesystemTruncation, maxLines, maxBytes int) map[string]any {
	return map[string]any{
		"content": value.Content, "truncated": value.Truncated, "truncatedBy": value.TruncatedBy,
		"totalLines": value.TotalLines, "totalBytes": value.TotalBytes,
		"outputLines": value.OutputLines, "outputBytes": value.OutputBytes,
		"lastLinePartial": false, "firstLineExceedsLimit": value.FirstLineLarge,
		"maxLines": maxLines, "maxBytes": maxBytes,
	}
}

func (s *FilesystemSuite) Write(ctx context.Context, input WriteInput) (ToolResult, error) {
	if err := contextFailure(ctx); err != nil {
		return ToolResult{Text: operationErrorText(err)}, err
	}
	if err := validFilesystemArgument("path", input.Path); err != nil {
		return inputError(err)
	}
	if err := validText("content", input.Content); err != nil {
		return inputError(err)
	}
	path, err := resolveToolPath(s.workingDir, input.Path)
	if err != nil {
		return ToolResult{}, err
	}
	key, err := mutationKey(path)
	if err != nil {
		return ToolResult{}, err
	}
	err = s.mutations.with(ctx, key, func() error {
		if err := context.Cause(ctx); err != nil {
			return errors.Join(ErrOperationCancelled, err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("create parent directory: %w", err)
		}
		if err := context.Cause(ctx); err != nil {
			return errors.Join(ErrOperationCancelled, err)
		}
		if err := os.WriteFile(path, []byte(input.Content), 0o666); err != nil {
			return fmt.Errorf("write file: %w", err)
		}
		if err := context.Cause(ctx); err != nil {
			return errors.Join(ErrOperationCancelled, err)
		}
		return nil
	})
	if err != nil {
		return ToolResult{Text: operationErrorText(err)}, err
	}
	// The upstream message labels JavaScript string.length as bytes. Preserve
	// that observable value, including UTF-16 surrogate-pair accounting.
	length := len(utf16.Encode([]rune(input.Content)))
	return ToolResult{Text: fmt.Sprintf("Successfully wrote %d bytes to %s", length, input.Path)}, nil
}

func resolveMutationDestination(requested string) (string, error) {
	current := filepath.Clean(requested)
	for depth := 0; depth < 255; depth++ {
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return resolveMissingDestination(current)
		}
		if err != nil {
			return "", fmt.Errorf("inspect destination: %w", err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			resolved, resolveErr := filepath.EvalSymlinks(current)
			if resolveErr != nil {
				return "", fmt.Errorf("%w: resolve destination: %w", ErrFilesystemPath, resolveErr)
			}
			return filepath.Clean(resolved), nil
		}
		link, readErr := os.Readlink(current)
		if readErr != nil {
			return "", fmt.Errorf("%w: read destination symlink: %w", ErrFilesystemPath, readErr)
		}
		if filepath.IsAbs(link) {
			current = filepath.Clean(link)
		} else {
			current = filepath.Join(filepath.Dir(current), link)
		}
	}
	return "", fmt.Errorf("%w: too many destination symlinks", ErrFilesystemPath)
}

func resolveMissingDestination(destination string) (string, error) {
	parent := filepath.Dir(destination)
	remainder := filepath.Base(destination)
	for {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			return filepath.Join(resolved, remainder), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%w: resolve destination parent: %w", ErrFilesystemPath, err)
		}
		next := filepath.Dir(parent)
		if next == parent {
			return filepath.Clean(destination), nil
		}
		remainder = filepath.Join(filepath.Base(parent), remainder)
		parent = next
	}
}

func operationErrorText(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrOperationCancelled) {
		return "Filesystem operation cancelled"
	}
	return err.Error()
}

func contextFailure(ctx context.Context) error {
	if ctx == nil {
		return ErrOperationCancelled
	}
	if cause := context.Cause(ctx); cause != nil {
		return errors.Join(ErrOperationCancelled, cause)
	}
	return nil
}

func validText(name, value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s must be valid UTF-8", ErrInvalidFilesystemInput, name)
	}
	return nil
}

func validFilesystemArgument(name, value string) error {
	if err := validText(name, value); err != nil {
		return err
	}
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%w: %s contains NUL", ErrInvalidFilesystemInput, name)
	}
	return nil
}

// strictObject retains its historical name, but follows JavaScript JSON.parse
// and TypeBox Object semantics: duplicate names use the final value and
// undeclared properties are permitted unless the schema says otherwise.
func strictObject(raw []byte, _ map[string]struct{}) (map[string]json.RawMessage, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidFilesystemInput, err)
	}
	if values == nil {
		return nil, fmt.Errorf("%w: arguments must be an object", ErrInvalidFilesystemInput)
	}
	return values, nil
}

func requiredString(values map[string]json.RawMessage, name string) (string, error) {
	raw, ok := values[name]
	if !ok {
		return "", fmt.Errorf("%w: %s is required", ErrInvalidFilesystemInput, name)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%w: %s must be a string", ErrInvalidFilesystemInput, name)
	}
	return value, validText(name, value)
}

func optionalString(values map[string]json.RawMessage, name string) (*string, error) {
	raw, ok := values[name]
	if !ok {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("%w: %s must be a string", ErrInvalidFilesystemInput, name)
	}
	if err := validText(name, value); err != nil {
		return nil, err
	}
	return &value, nil
}

func optionalInt(values map[string]json.RawMessage, name string) (*int, error) {
	raw, ok := values[name]
	if !ok {
		return nil, nil
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return nil, fmt.Errorf("%w: %s must be a number", ErrInvalidFilesystemInput, name)
	}
	value64, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || math.IsNaN(value64) || math.IsInf(value64, 0) || value64 > float64(int(^uint(0)>>1)) || value64 < -float64(int(^uint(0)>>1))-1 {
		return nil, fmt.Errorf("%w: %s must be a finite number", ErrInvalidFilesystemInput, name)
	}
	value := int(math.Trunc(value64))
	return &value, nil
}

func optionalBool(values map[string]json.RawMessage, name string) (*bool, error) {
	raw, ok := values[name]
	if !ok {
		return nil, nil
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("%w: %s must be a boolean", ErrInvalidFilesystemInput, name)
	}
	return &value, nil
}

func decodeReadInput(raw []byte) (ReadInput, error) {
	values, err := strictObject(raw, fields("path", "offset", "limit"))
	if err != nil {
		return ReadInput{}, err
	}
	path, err := requiredString(values, "path")
	if err != nil {
		return ReadInput{}, err
	}
	offset, err := optionalInt(values, "offset")
	if err != nil {
		return ReadInput{}, err
	}
	limit, err := optionalInt(values, "limit")
	if err != nil {
		return ReadInput{}, err
	}
	return ReadInput{Path: path, Offset: offset, Limit: limit}, nil
}

func decodeWriteInput(raw []byte) (WriteInput, error) {
	values, err := strictObject(raw, fields("path", "content"))
	if err != nil {
		return WriteInput{}, err
	}
	path, err := requiredString(values, "path")
	if err != nil {
		return WriteInput{}, err
	}
	content, err := requiredString(values, "content")
	if err != nil {
		return WriteInput{}, err
	}
	return WriteInput{Path: path, Content: content}, nil
}

func decodeEditInput(raw []byte) (EditInput, error) {
	values, err := strictObject(raw, fields("path", "edits", "oldText", "newText"))
	if err != nil {
		return EditInput{}, err
	}
	path, err := requiredString(values, "path")
	if err != nil {
		return EditInput{}, err
	}
	var encoded []json.RawMessage
	if rawEdits, ok := values["edits"]; ok {
		if len(rawEdits) > 0 && rawEdits[0] == '"' {
			var stringified string
			if err := json.Unmarshal(rawEdits, &stringified); err == nil {
				rawEdits = []byte(stringified)
			}
		}
		if err := json.Unmarshal(rawEdits, &encoded); err != nil {
			return EditInput{}, fmt.Errorf("%w: edits must be an array", ErrInvalidFilesystemInput)
		}
	}
	edits := make([]Edit, 0, len(encoded))
	for index, rawEdit := range encoded {
		values, err := strictObject(rawEdit, fields("oldText", "newText"))
		if err != nil {
			return EditInput{}, fmt.Errorf("edits[%d]: %w", index, err)
		}
		oldText, err := requiredString(values, "oldText")
		if err != nil {
			return EditInput{}, err
		}
		newText, err := requiredString(values, "newText")
		if err != nil {
			return EditInput{}, err
		}
		edits = append(edits, Edit{oldText, newText})
	}
	if _, hasOld := values["oldText"]; hasOld {
		oldText, oldErr := requiredString(values, "oldText")
		newText, newErr := requiredString(values, "newText")
		if oldErr != nil {
			return EditInput{}, oldErr
		}
		if newErr != nil {
			return EditInput{}, newErr
		}
		edits = append(edits, Edit{OldText: oldText, NewText: newText})
	}
	if len(edits) == 0 {
		return EditInput{}, fmt.Errorf("%w: edits must contain at least one replacement", ErrInvalidFilesystemInput)
	}
	return EditInput{Path: path, Edits: edits}, nil
}

func fields(names ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(names))
	for _, name := range names {
		result[name] = struct{}{}
	}
	return result
}
