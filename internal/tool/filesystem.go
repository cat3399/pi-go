package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// FilesystemSuite is the production implementation of Pi's read/write/edit/
// grep/find/ls family. It owns path policy and mutation serialization, but no
// agent/session state or provider tool-call identifiers.
type FilesystemSuite struct {
	workingDir string
	maxLines   int
	maxBytes   int
	mutations  *mutationQueue
}

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
	return &FilesystemSuite{workingDir: filepath.Clean(workingDir), maxLines: options.MaxLines, maxBytes: options.MaxBytes, mutations: newMutationQueue()}, nil
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

// ExecuteJSON is the registry dispatch boundary. It rejects unknown and
// duplicate fields at each tool boundary; decoding into a Go struct directly
// would silently accept both and diverge from the established Bash contract.
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

func (s *FilesystemSuite) Read(ctx context.Context, input ReadInput) (ToolResult, error) {
	if err := contextFailure(ctx); err != nil {
		return ToolResult{Text: operationErrorText(err)}, err
	}
	if err := validText("path", input.Path); err != nil {
		return inputError(err)
	}
	if input.Offset != nil && *input.Offset < 1 {
		return inputError(fmt.Errorf("%w: offset must be at least 1", ErrInvalidFilesystemInput))
	}
	if input.Limit != nil && *input.Limit < 1 {
		return inputError(fmt.Errorf("%w: limit must be at least 1", ErrInvalidFilesystemInput))
	}
	path, err := resolveReadPath(s.workingDir, input.Path)
	if err != nil {
		return ToolResult{}, err
	}
	if err := context.Cause(ctx); err != nil {
		return ToolResult{Text: "Filesystem operation cancelled"}, errors.Join(ErrOperationCancelled, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ToolResult{}, fmt.Errorf("read %s: %w", input.Path, err)
	}
	if isBinary(data) {
		return ToolResult{Text: fmt.Sprintf("Cannot read %s: binary file is not supported by this text tool", input.Path)}, fmt.Errorf("%w: %s", ErrBinaryFile, input.Path)
	}
	if !utf8.Valid(data) {
		return ToolResult{Text: fmt.Sprintf("Cannot read %s: file is not valid UTF-8", input.Path)}, fmt.Errorf("%w: invalid UTF-8", ErrBinaryFile)
	}
	if err := context.Cause(ctx); err != nil {
		return ToolResult{Text: "Filesystem operation cancelled"}, errors.Join(ErrOperationCancelled, err)
	}
	text := string(data)
	lines := strings.Split(text, "\n")
	start := 0
	if input.Offset != nil {
		start = *input.Offset - 1
	}
	if start >= len(lines) {
		return ToolResult{}, fmt.Errorf("%w: offset %d is beyond end of file (%d lines total)", ErrFilesystemPath, start+1, len(lines))
	}
	selected := lines[start:]
	limited := false
	if input.Limit != nil && len(selected) > *input.Limit {
		selected, limited = selected[:*input.Limit], true
	}
	truncation := truncateFilesystemHead(strings.Join(selected, "\n"), s.maxLines, s.maxBytes)
	details := map[string]any{}
	output := truncation.Content
	if truncation.Truncated {
		details["truncation"] = s.truncationDetails(truncation)
		if truncation.FirstLineLarge {
			output = fmt.Sprintf("[Line %d is %s, exceeds %s limit. Use read with a smaller range.]", start+1, formatSize(len(lines[start])), formatSize(s.maxBytes))
		} else {
			end := start + truncation.OutputLines
			output += fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Use offset=%d to continue.]", start+1, end, len(lines), end+1)
		}
	} else if limited {
		next := start + len(selected) + 1
		output += fmt.Sprintf("\n\n[%d more lines in file. Use offset=%d to continue.]", len(lines)-(start+len(selected)), next)
	}
	if err := contextFailure(ctx); err != nil {
		return ToolResult{Text: operationErrorText(err)}, err
	}
	if len(details) == 0 {
		details = nil
	}
	return ToolResult{Text: output, Details: details}, nil
}

func isBinary(data []byte) bool {
	if bytes.IndexByte(data, 0) >= 0 {
		return true
	}
	return false
}

func (s *FilesystemSuite) truncationDetails(value FilesystemTruncation) map[string]any {
	return map[string]any{"truncated": value.Truncated, "truncatedBy": value.TruncatedBy, "totalLines": value.TotalLines, "totalBytes": value.TotalBytes, "outputLines": value.OutputLines, "outputBytes": value.OutputBytes, "maxLines": s.maxLines, "maxBytes": s.maxBytes, "firstLineExceedsLimit": value.FirstLineLarge}
}

func (s *FilesystemSuite) Write(ctx context.Context, input WriteInput) (ToolResult, error) {
	if err := contextFailure(ctx); err != nil {
		return ToolResult{Text: operationErrorText(err)}, err
	}
	if err := validText("path", input.Path); err != nil {
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
		if err := atomicWrite(ctx, path, key, []byte(input.Content)); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return ToolResult{Text: operationErrorText(err)}, err
	}
	return ToolResult{Text: fmt.Sprintf("Successfully wrote %d bytes to %s", len(input.Content), input.Path)}, nil
}

type pathSnapshot struct {
	exists bool
	info   fs.FileInfo
	link   string
}

type atomicWritePlan struct {
	requested         string
	target            string
	mutationKey       string
	temporaryName     string
	requestedSnapshot pathSnapshot
	targetSnapshot    pathSnapshot
	committed         bool
}

func atomicWrite(ctx context.Context, destination, expectedKey string, contents []byte) error {
	plan, err := prepareAtomicWrite(ctx, destination, expectedKey, contents)
	if err != nil {
		return err
	}
	defer plan.cleanup()
	return plan.commit(ctx)
}

func prepareAtomicWrite(ctx context.Context, destination, expectedKey string, contents []byte) (*atomicWritePlan, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, errors.Join(ErrOperationCancelled, err)
	}
	requested := filepath.Clean(destination)
	directory := filepath.Dir(requested)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, fmt.Errorf("create parent directory: %w", err)
	}
	target, err := resolveMutationDestination(requested)
	if err != nil {
		return nil, err
	}
	key, err := mutationKey(requested)
	if err != nil {
		return nil, err
	}
	if filepath.Clean(key) != filepath.Clean(expectedKey) {
		return nil, fmt.Errorf("%w: mutation target changed before write", ErrFilesystemPath)
	}
	requestedSnapshot, err := snapshotPath(requested)
	if err != nil {
		return nil, err
	}
	targetSnapshot, err := snapshotPath(target)
	if err != nil {
		return nil, err
	}
	mode := fs.FileMode(0o644)
	if targetSnapshot.exists {
		mode = targetSnapshot.info.Mode().Perm()
		if mode&0o222 == 0 {
			return nil, fmt.Errorf("destination is not writable: %w", fs.ErrPermission)
		}
	}
	targetDirectory := filepath.Dir(target)
	temporary, err := os.CreateTemp(targetDirectory, ".pi-go-write-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporaryName)
		return nil, fmt.Errorf("set temporary mode: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporaryName)
		return nil, fmt.Errorf("write temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporaryName)
		return nil, fmt.Errorf("sync temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryName)
		return nil, fmt.Errorf("close temporary file: %w", err)
	}
	return &atomicWritePlan{requested: requested, target: target, mutationKey: key, temporaryName: temporaryName, requestedSnapshot: requestedSnapshot, targetSnapshot: targetSnapshot}, nil
}

func (p *atomicWritePlan) commit(ctx context.Context) error {
	if p == nil {
		return errors.New("atomic write plan is nil")
	}
	if err := context.Cause(ctx); err != nil {
		return errors.Join(ErrOperationCancelled, err)
	}
	if err := verifyPathSnapshot(p.requested, p.requestedSnapshot); err != nil {
		return fmt.Errorf("%w: requested path changed before commit: %v", ErrFilesystemPath, err)
	}
	currentTarget, err := resolveMutationDestination(p.requested)
	if err != nil {
		return err
	}
	if filepath.Clean(currentTarget) != filepath.Clean(p.target) {
		return fmt.Errorf("%w: symlink target changed before commit", ErrFilesystemPath)
	}
	currentKey, err := mutationKey(p.requested)
	if err != nil {
		return err
	}
	if filepath.Clean(currentKey) != filepath.Clean(p.mutationKey) {
		return fmt.Errorf("%w: mutation target changed before commit", ErrFilesystemPath)
	}
	if err := verifyPathSnapshot(p.target, p.targetSnapshot); err != nil {
		return fmt.Errorf("%w: destination changed before commit: %v", ErrFilesystemPath, err)
	}
	if p.targetSnapshot.exists && p.targetSnapshot.info.Mode().Perm()&0o222 == 0 {
		return fmt.Errorf("destination is not writable: %w", fs.ErrPermission)
	}
	if err := os.Rename(p.temporaryName, p.target); err != nil {
		return fmt.Errorf("atomically replace destination: %w", err)
	}
	p.committed = true
	if directoryHandle, err := os.Open(filepath.Dir(p.target)); err == nil {
		_ = directoryHandle.Sync()
		_ = directoryHandle.Close()
	}
	return nil
}

func (p *atomicWritePlan) cleanup() {
	if p == nil || p.committed || p.temporaryName == "" {
		return
	}
	_ = os.Remove(p.temporaryName)
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

func snapshotPath(path string) (pathSnapshot, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return pathSnapshot{}, nil
	}
	if err != nil {
		return pathSnapshot{}, fmt.Errorf("inspect path snapshot: %w", err)
	}
	snapshot := pathSnapshot{exists: true, info: info}
	if info.Mode()&os.ModeSymlink != 0 {
		snapshot.link, err = os.Readlink(path)
		if err != nil {
			return pathSnapshot{}, fmt.Errorf("read symlink snapshot: %w", err)
		}
	}
	return snapshot, nil
}

func verifyPathSnapshot(path string, expected pathSnapshot) error {
	current, err := snapshotPath(path)
	if err != nil {
		return err
	}
	if current.exists != expected.exists {
		return errors.New("path existence changed")
	}
	if !current.exists {
		return nil
	}
	if current.info.Mode().Type() != expected.info.Mode().Type() || current.link != expected.link {
		return errors.New("path type or symlink changed")
	}
	if !os.SameFile(current.info, expected.info) {
		return errors.New("path identity changed")
	}
	if current.info.Mode().Perm() != expected.info.Mode().Perm() {
		return errors.New("path permissions changed")
	}
	return nil
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
	if name == "path" && value == "" {
		return fmt.Errorf("%w: path is required", ErrInvalidFilesystemInput)
	}
	return nil
}

// strictObject parses a JSON object with unique keys and consumes the complete
// stream. It is shared by all filesystem tool inputs.
func strictObject(raw []byte, allowed map[string]struct{}) (map[string]json.RawMessage, error) {
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("%w: arguments must be valid UTF-8 JSON", ErrInvalidFilesystemInput)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidFilesystemInput, err)
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, fmt.Errorf("%w: arguments must be an object", ErrInvalidFilesystemInput)
	}
	values := make(map[string]json.RawMessage)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidFilesystemInput, err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, fmt.Errorf("%w: object key must be a string", ErrInvalidFilesystemInput)
		}
		if _, exists := values[key]; exists {
			return nil, fmt.Errorf("%w: duplicate field %q", ErrInvalidFilesystemInput, key)
		}
		if _, ok := allowed[key]; !ok {
			return nil, fmt.Errorf("%w: unknown field %q", ErrInvalidFilesystemInput, key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("%w: field %q: %v", ErrInvalidFilesystemInput, key, err)
		}
		values[key] = value
	}
	token, err = decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidFilesystemInput, err)
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '}' {
		return nil, fmt.Errorf("%w: malformed object", ErrInvalidFilesystemInput)
	}
	if token, err = decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("%w: unexpected trailing token %v", ErrInvalidFilesystemInput, token)
		}
		return nil, fmt.Errorf("%w: trailing JSON: %v", ErrInvalidFilesystemInput, err)
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
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("%w: %s must be an integer", ErrInvalidFilesystemInput, name)
	}
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
	values, err := strictObject(raw, fields("path", "edits"))
	if err != nil {
		return EditInput{}, err
	}
	path, err := requiredString(values, "path")
	if err != nil {
		return EditInput{}, err
	}
	rawEdits, ok := values["edits"]
	if !ok {
		return EditInput{}, fmt.Errorf("%w: edits is required", ErrInvalidFilesystemInput)
	}
	var encoded []json.RawMessage
	if err := json.Unmarshal(rawEdits, &encoded); err != nil {
		return EditInput{}, fmt.Errorf("%w: edits must be an array", ErrInvalidFilesystemInput)
	}
	if len(encoded) == 0 {
		return EditInput{}, fmt.Errorf("%w: edits must contain at least one replacement", ErrInvalidFilesystemInput)
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
	return EditInput{Path: path, Edits: edits}, nil
}

func fields(names ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(names))
	for _, name := range names {
		result[name] = struct{}{}
	}
	return result
}
