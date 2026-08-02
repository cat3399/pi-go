package tool

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

var ErrInvalidToolRegistry = errors.New("invalid tool registry")

// JSONTool is the narrow common dispatch contract used by the agent adapter.
// Tool implementations own argument validation and provider-visible text.
type JSONTool interface {
	Name() string
	ExecuteJSON(context.Context, []byte) (ToolResult, error)
}

// Registry has immutable dispatch state after construction. It returns cloned
// results so a caller cannot mutate metadata observed by another invocation.
type Registry struct {
	tools map[string]JSONTool
	names []string
}

func NewRegistry(tools ...JSONTool) (*Registry, error) {
	registry := &Registry{tools: make(map[string]JSONTool, len(tools))}
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
	sort.Strings(registry.names)
	return registry, nil
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
	return NewRegistry(tools...)
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
