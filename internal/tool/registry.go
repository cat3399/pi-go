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

// JSONTool is the narrow common dispatch contract used by the agent adapter.
// Tool implementations own argument validation and provider-visible text.
type JSONTool interface {
	Name() string
	ExecuteJSON(context.Context, []byte) (ToolResult, error)
}

// Specification is the immutable provider-advertised contract for one local
// JSON tool. It deliberately lives beside execution, not in a provider
// package: registry construction remains usable with deterministic providers.
type Specification struct {
	name        string
	description string
	strict      bool
	parameters  []byte
}

func NewSpecification(name, description string, strict bool, parametersJSON []byte) (Specification, error) {
	specification := Specification{name: name, description: description, strict: strict, parameters: bytes.Clone(parametersJSON)}
	if err := specification.validate(); err != nil {
		return Specification{}, err
	}
	return specification, nil
}

func (s Specification) validate() error {
	if !utf8.ValidString(s.name) || strings.TrimSpace(s.name) == "" ||
		!utf8.ValidString(s.description) || strings.TrimSpace(s.description) == "" ||
		!utf8.Valid(s.parameters) || len(s.parameters) == 0 {
		return fmt.Errorf("%w: name, description, and schema are required", ErrInvalidToolRegistry)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(s.parameters, &object); err != nil || object == nil {
		return fmt.Errorf("%w: schema for %q must be a JSON object", ErrInvalidToolRegistry, s.name)
	}
	return nil
}
func (s Specification) Name() string           { return s.name }
func (s Specification) Description() string    { return s.description }
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
	return ToolResult{Text: result.Text()}, err
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

func mustSpecification(name, description, schema string) Specification {
	specification, err := NewSpecification(name, description, true, []byte(schema))
	if err != nil {
		panic(err)
	}
	return specification
}

func bashSpecification() Specification {
	schema := fmt.Sprintf(
		`{"type":"object","additionalProperties":false,"properties":{"command":{"type":"string"},"timeout":{"type":"number","exclusiveMinimum":0,"maximum":%s}},"required":["command"]}`,
		formatSeconds(MaxBashTimeout),
	)
	return mustSpecification(BashToolName, "Run a shell command in the working directory.", schema)
}

func filesystemSpecifications() []Specification {
	return []Specification{
		mustSpecification(ReadToolName, "Read a UTF-8 text file.", `{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string","minLength":1},"offset":{"type":"integer","minimum":1},"limit":{"type":"integer","minimum":1}},"required":["path"]}`),
		mustSpecification(WriteToolName, "Write UTF-8 text to a file.", `{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string","minLength":1},"content":{"type":"string"}},"required":["path","content"]}`),
		mustSpecification(EditToolName, "Apply exact text replacements to a file.", `{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string","minLength":1},"edits":{"type":"array","minItems":1,"items":{"type":"object","additionalProperties":false,"properties":{"oldText":{"type":"string","minLength":1},"newText":{"type":"string"}},"required":["oldText","newText"]}}},"required":["path","edits"]}`),
		mustSpecification(GrepToolName, "Search text files for a pattern.", `{"type":"object","additionalProperties":false,"properties":{"pattern":{"type":"string"},"path":{"type":"string","minLength":1},"glob":{"type":"string"},"ignoreCase":{"type":"boolean"},"literal":{"type":"boolean"},"context":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1}},"required":["pattern"]}`),
		mustSpecification(FindToolName, "Find paths matching a glob.", `{"type":"object","additionalProperties":false,"properties":{"pattern":{"type":"string"},"path":{"type":"string","minLength":1},"limit":{"type":"integer","minimum":1}},"required":["pattern"]}`),
		mustSpecification(LsToolName, "List directory entries.", `{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string","minLength":1},"limit":{"type":"integer","minimum":1}},"required":[]}`),
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
