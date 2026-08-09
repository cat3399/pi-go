package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

var ErrInvalidToolRegistry = errors.New("invalid tool registry")

// ExecutionMode is dispatch metadata, not an execution ownership transfer.
// A sequential tool causes its enclosing agent batch to serialize because its
// effects may depend on neighbouring calls in source order.
type ExecutionMode uint8

const (
	ExecutionParallel ExecutionMode = iota + 1
	ExecutionSequential
)

// JSONTool is the narrow common dispatch contract used by the agent adapter.
// Tool implementations own argument validation and provider-visible text.
type JSONTool interface {
	Name() string
	ExecuteJSON(context.Context, []byte) (ToolResult, error)
}

// ExecutionModeTool is optional so existing tools remain parallel by default.
type ExecutionModeTool interface{ ToolExecutionMode() ExecutionMode }

// Specification is the immutable provider-advertised contract for one local
// JSON tool. It deliberately lives beside execution, not in a provider
// package: registry construction remains usable with deterministic providers.
type Specification struct {
	name             string
	label            string
	description      string
	promptSnippet    string
	promptGuidelines []string
	strict           bool
	parameters       []byte
}

func NewSpecification(name, description string, strict bool, parametersJSON []byte) (Specification, error) {
	return NewSpecificationWithPrompt(name, name, description, "", nil, strict, parametersJSON)
}

// NewSpecificationWithPrompt retains the complete built-in ToolDefinition
// metadata used by system-prompt reconstruction. Provider schemas and prompt
// descriptions are two projections of the same definition, not parallel
// hard-coded registries.
func NewSpecificationWithPrompt(name, label, description, promptSnippet string, promptGuidelines []string, strict bool, parametersJSON []byte) (Specification, error) {
	specification := Specification{
		name: name, label: label, description: description, promptSnippet: promptSnippet,
		promptGuidelines: append([]string(nil), promptGuidelines...), strict: strict, parameters: bytes.Clone(parametersJSON),
	}
	if err := specification.validate(); err != nil {
		return Specification{}, err
	}
	return specification, nil
}

func (s Specification) validate() error {
	if !utf8.ValidString(s.name) || strings.TrimSpace(s.name) == "" ||
		!utf8.ValidString(s.label) || strings.TrimSpace(s.label) == "" ||
		!utf8.ValidString(s.description) || strings.TrimSpace(s.description) == "" ||
		!utf8.ValidString(s.promptSnippet) ||
		!utf8.Valid(s.parameters) || len(s.parameters) == 0 {
		return fmt.Errorf("%w: name, description, and schema are required", ErrInvalidToolRegistry)
	}
	for _, guideline := range s.promptGuidelines {
		if !utf8.ValidString(guideline) {
			return fmt.Errorf("%w: prompt guideline for %q is invalid", ErrInvalidToolRegistry, s.name)
		}
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(s.parameters, &object); err != nil || object == nil {
		return fmt.Errorf("%w: schema for %q must be a JSON object", ErrInvalidToolRegistry, s.name)
	}
	return nil
}
func (s Specification) Name() string          { return s.name }
func (s Specification) Label() string         { return s.label }
func (s Specification) Description() string   { return s.description }
func (s Specification) PromptSnippet() string { return s.promptSnippet }
func (s Specification) PromptGuidelines() []string {
	return append([]string(nil), s.promptGuidelines...)
}
func (s Specification) Strict() bool           { return s.strict }
func (s Specification) ParametersJSON() []byte { return bytes.Clone(s.parameters) }

// Registry has immutable dispatch state after construction. It returns cloned
// results so a caller cannot mutate metadata observed by another invocation.
type Registry struct {
	tools map[string]JSONTool
	names []string
	specs map[string]Specification
}

func NewRegistry(tools ...JSONTool) (*Registry, error) {
	return newRegistry(nil, tools...)
}

// NewRegistryWithSpecifications binds advertised names to the same immutable
// dispatcher. A missing or extra schema is rejected rather than silently
// making a model-visible tool non-executable (or vice versa).
func NewRegistryWithSpecifications(specifications []Specification, tools ...JSONTool) (*Registry, error) {
	return newRegistry(specifications, tools...)
}

func newRegistry(specifications []Specification, tools ...JSONTool) (*Registry, error) {
	registry := &Registry{tools: make(map[string]JSONTool, len(tools)), specs: make(map[string]Specification, len(specifications))}
	for _, specification := range specifications {
		if err := specification.validate(); err != nil {
			return nil, err
		}
		if _, exists := registry.specs[specification.Name()]; exists {
			return nil, fmt.Errorf("%w: duplicate schema %q", ErrInvalidToolRegistry, specification.Name())
		}
		registry.specs[specification.Name()] = specification
	}
	for _, candidate := range tools {
		if candidate == nil {
			return nil, fmt.Errorf("%w: nil tool", ErrInvalidToolRegistry)
		}
		name := candidate.Name()
		if name == "" {
			return nil, fmt.Errorf("%w: empty tool name", ErrInvalidToolRegistry)
		}
		if _, exists := registry.tools[name]; exists {
			return nil, fmt.Errorf("%w: duplicate tool %q", ErrInvalidToolRegistry, name)
		}
		registry.tools[name] = candidate
		registry.names = append(registry.names, name)
	}
	if len(registry.specs) != 0 {
		for _, name := range registry.names {
			if _, ok := registry.specs[name]; !ok {
				return nil, fmt.Errorf("%w: tool %q has no schema", ErrInvalidToolRegistry, name)
			}
		}
		for name := range registry.specs {
			if _, ok := registry.tools[name]; !ok {
				return nil, fmt.Errorf("%w: schema %q has no tool", ErrInvalidToolRegistry, name)
			}
		}
	}
	sort.Strings(registry.names)
	return registry, nil
}

func (r *Registry) Specifications() []Specification {
	if r == nil || len(r.specs) == 0 {
		return nil
	}
	result := make([]Specification, 0, len(r.names))
	for _, name := range r.names {
		if specification, ok := r.specs[name]; ok {
			specification.parameters = bytes.Clone(specification.parameters)
			specification.promptGuidelines = append([]string(nil), specification.promptGuidelines...)
			result = append(result, specification)
		}
	}
	return result
}
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.names...)
}
func (r *Registry) Supports(name string) bool {
	if r == nil {
		return false
	}
	_, ok := r.tools[name]
	return ok
}

// PrepareArguments exposes definition-specific transforms that must run before
// agent-side JSON Schema validation. Tools without a transform return the same
// value unchanged.
func (r *Registry) PrepareArguments(name string, arguments any) (any, error) {
	if r == nil || !r.Supports(name) {
		return arguments, nil
	}
	if name == EditToolName {
		return PrepareEditArguments(arguments), nil
	}
	return arguments, nil
}

// ExecutionMode returns the selected tool override. Unknown and malformed
// metadata deliberately fail closed to sequential rather than allowing an
// undeclared side-effecting tool to race a batch.
func (r *Registry) ExecutionMode(name string) (ExecutionMode, bool) {
	if r == nil {
		return 0, false
	}
	candidate, ok := r.tools[name]
	if !ok {
		return 0, false
	}
	modeTool, ok := candidate.(ExecutionModeTool)
	if !ok {
		return ExecutionParallel, true
	}
	mode := modeTool.ToolExecutionMode()
	if mode != ExecutionParallel && mode != ExecutionSequential {
		return ExecutionSequential, true
	}
	return mode, true
}
func (r *Registry) ExecuteJSON(ctx context.Context, name string, arguments []byte) (ToolResult, error) {
	if r == nil {
		return ToolResult{Text: "Tool registry is not configured"}, errors.New("tool registry is nil")
	}
	candidate, ok := r.tools[name]
	if !ok {
		return ToolResult{Text: fmt.Sprintf("Tool %s not found", name)}, fmt.Errorf("%w: %s", ErrFilesystemToolNotFound, name)
	}
	result, err := candidate.ExecuteJSON(ctx, arguments)
	return result.clone(), err
}

// FilesystemTool adapts one named suite member to Registry without duplicating
// its JSON decoder. It is intentionally unexported in behavior: callers use
// NewFilesystemRegistry to receive all six consistent tools together.
type filesystemTool struct {
	suite *FilesystemSuite
	name  string
}

func (t filesystemTool) Name() string { return t.name }
func (t filesystemTool) ExecuteJSON(ctx context.Context, arguments []byte) (ToolResult, error) {
	return t.suite.ExecuteJSON(ctx, t.name, arguments)
}

func NewFilesystemRegistry(suite *FilesystemSuite) (*Registry, error) {
	if suite == nil {
		return nil, fmt.Errorf("%w: filesystem suite is required", ErrInvalidToolRegistry)
	}
	names := suite.Names()
	tools := make([]JSONTool, 0, len(names))
	for _, name := range names {
		tools = append(tools, filesystemTool{suite, name})
	}
	return NewRegistryWithSpecifications(filesystemSpecifications(), tools...)
}

type bashJSONTool struct{ bash *Bash }

func (t bashJSONTool) Name() string { return BashToolName }
func (t bashJSONTool) ExecuteJSON(ctx context.Context, arguments []byte) (ToolResult, error) {
	result, err := t.bash.ExecuteJSON(ctx, arguments)
	details := map[string]any(nil)
	truncation := result.Truncation()
	if truncatedBy, ok := truncation.TruncatedBy(); ok {
		details = map[string]any{
			"truncation": map[string]any{
				"content":               result.CapturedOutput(),
				"truncated":             true,
				"truncatedBy":           truncatedBy.String(),
				"totalLines":            truncation.TotalLines(),
				"totalBytes":            truncation.TotalBytes(),
				"outputLines":           truncation.OutputLines(),
				"outputBytes":           truncation.OutputBytes(),
				"lastLinePartial":       truncation.LastLinePartial(),
				"firstLineExceedsLimit": false,
				"maxLines":              truncation.MaxLines(),
				"maxBytes":              truncation.MaxBytes(),
			},
		}
		if path, exists := result.FullOutputPath(); exists {
			details["fullOutputPath"] = path
		}
	}
	return ToolResult{Text: result.Text(), Details: details}, err
}

// NewBuiltInRegistry is the provider-visible built-in tool set. It contains
// only tools with schemas and concrete executors; callers never advertise a
// name that the registry cannot dispatch.
func NewBuiltInRegistry(bash *Bash, filesystem *FilesystemSuite) (*Registry, error) {
	if bash == nil || filesystem == nil {
		return nil, fmt.Errorf("%w: bash and filesystem suite are required", ErrInvalidToolRegistry)
	}
	filesystemNames := filesystem.Names()
	tools := make([]JSONTool, 0, len(filesystemNames)+1)
	tools = append(tools, bashJSONTool{bash: bash})
	for _, name := range filesystemNames {
		tools = append(tools, filesystemTool{suite: filesystem, name: name})
	}
	specifications := append([]Specification{bashSpecification()}, filesystemSpecifications()...)
	return NewRegistryWithSpecifications(specifications, tools...)
}

func mustBuiltInSpecification(name, description, promptSnippet string, promptGuidelines []string, schema string) Specification {
	// The fixed upstream Responses conversion defaults ordinary JSON-schema
	// tools to non-strict mode. Keep built-ins false unless their schemas are
	// deliberately redesigned so every optional property is required-nullable.
	specification, err := NewSpecificationWithPrompt(name, name, description, promptSnippet, promptGuidelines, false, []byte(schema))
	if err != nil {
		panic(err)
	}
	return specification
}

func bashSpecification() Specification {
	schema := `{"type":"object","properties":{"command":{"type":"string","description":"Bash command to execute"},"timeout":{"type":"number","description":"Timeout in seconds (optional, no default timeout)"}},"required":["command"]}`
	return mustBuiltInSpecification(
		BashToolName,
		"Execute a bash command in the current working directory. Returns stdout and stderr. Output is truncated to last 2000 lines or 50KB (whichever is hit first). If truncated, full output is saved to a temp file. Optionally provide a timeout in seconds.",
		"Execute bash commands (ls, grep, find, etc.)", []string{"Inspect PI_* environment variables for current model and session details."}, schema,
	)
}

func filesystemSpecifications() []Specification {
	return []Specification{
		mustBuiltInSpecification(
			ReadToolName,
			"Read the contents of a file. Supports text files and images (jpg, png, gif, webp, bmp). Images are sent as attachments. For text files, output is truncated to 2000 lines or 50KB (whichever is hit first). Use offset/limit for large files. When you need the full file, continue with offset until complete.",
			"Read file contents", []string{"Use read to examine files instead of cat or sed."},
			`{"type":"object","properties":{"path":{"type":"string","description":"Path to the file to read (relative or absolute)"},"offset":{"type":"number","description":"Line number to start reading from (1-indexed)"},"limit":{"type":"number","description":"Maximum number of lines to read"}},"required":["path"]}`,
		),
		mustBuiltInSpecification(
			WriteToolName,
			"Write content to a file. Creates the file if it doesn't exist, overwrites if it does. Automatically creates parent directories.",
			"Create or overwrite files", []string{"Use write only for new files or complete rewrites."},
			`{"type":"object","properties":{"path":{"type":"string","description":"Path to the file to write (relative or absolute)"},"content":{"type":"string","description":"Content to write to the file"}},"required":["path","content"]}`,
		),
		mustBuiltInSpecification(
			EditToolName,
			"Edit a single file using exact text replacement. Every edits[].oldText must match a unique, non-overlapping region of the original file. If two changes affect the same block or nearby lines, merge them into one edit instead of emitting overlapping edits. Do not include large unchanged regions just to connect distant changes.",
			"Make precise file edits with exact text replacement, including multiple disjoint edits in one call",
			[]string{
				"Use edit for precise changes (edits[].oldText must match exactly)",
				"When changing multiple separate locations in one file, use one edit call with multiple entries in edits[] instead of multiple edit calls",
				"Each edits[].oldText is matched against the original file, not after earlier edits are applied. Do not emit overlapping or nested edits. Merge nearby changes into one edit.",
				"Keep edits[].oldText as small as possible while still being unique in the file. Do not pad with large unchanged regions.",
			},
			`{"type":"object","properties":{"path":{"type":"string","description":"Path to the file to edit (relative or absolute)"},"edits":{"type":"array","description":"One or more targeted replacements. Each edit is matched against the original file, not incrementally. Do not include overlapping or nested edits. If two changes touch the same block or nearby lines, merge them into one edit instead.","items":{"type":"object","properties":{"oldText":{"type":"string","description":"Exact text for one targeted replacement. It must be unique in the original file and must not overlap with any other edits[].oldText in the same call."},"newText":{"type":"string","description":"Replacement text for this targeted edit."}},"required":["oldText","newText"]}}},"required":["path","edits"]}`,
		),
		mustBuiltInSpecification(
			GrepToolName,
			"Search file contents for a pattern. Returns matching lines with file paths and line numbers. Respects .gitignore. Output is truncated to 100 matches or 50KB (whichever is hit first). Long lines are truncated to 500 chars.",
			"Search file contents for patterns (respects .gitignore)", nil,
			`{"type":"object","properties":{"pattern":{"type":"string","description":"Search pattern (regex or literal string)"},"path":{"type":"string","description":"Directory or file to search (default: current directory)"},"glob":{"type":"string","description":"Filter files by glob pattern, e.g. '*.ts' or '**/*.spec.ts'"},"ignoreCase":{"type":"boolean","description":"Case-insensitive search (default: false)"},"literal":{"type":"boolean","description":"Treat pattern as literal string instead of regex (default: false)"},"context":{"type":"number","description":"Number of lines to show before and after each match (default: 0)"},"limit":{"type":"number","description":"Maximum number of matches to return (default: 100)"}},"required":["pattern"]}`,
		),
		mustBuiltInSpecification(
			FindToolName,
			"Search for files by glob pattern. Returns matching file paths relative to the search directory. Respects .gitignore. Output is truncated to 1000 results or 50KB (whichever is hit first).",
			"Find files by glob pattern (respects .gitignore)", nil,
			`{"type":"object","properties":{"pattern":{"type":"string","description":"Glob pattern to match files, e.g. '*.ts', '**/*.json', or 'src/**/*.spec.ts'"},"path":{"type":"string","description":"Directory to search in (default: current directory)"},"limit":{"type":"number","description":"Maximum number of results (default: 1000)"}},"required":["pattern"]}`,
		),
		mustBuiltInSpecification(
			LsToolName,
			"List directory contents. Returns entries sorted alphabetically, with '/' suffix for directories. Includes dotfiles. Output is truncated to 500 entries or 50KB (whichever is hit first).",
			"List directory contents", nil,
			`{"type":"object","properties":{"path":{"type":"string","description":"Directory to list (default: current directory)"},"limit":{"type":"number","description":"Maximum number of entries to return (default: 500)"}},"required":[]}`,
		),
	}
}

// DispatchExecutor is a small holder for non-agent consumers that need a
// validated immutable registry without reaching into construction details.
type DispatchExecutor struct{ registry *Registry }

func NewDispatchExecutor(registry *Registry) (*DispatchExecutor, error) {
	if registry == nil {
		return nil, fmt.Errorf("%w: registry is required", ErrInvalidToolRegistry)
	}
	return &DispatchExecutor{registry: registry}, nil
}
func (d *DispatchExecutor) Registry() *Registry {
	if d == nil {
		return nil
	}
	return d.registry
}
