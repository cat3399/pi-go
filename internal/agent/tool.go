package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/tool"
)

// BashExecutor adapts the production Bash tool to the agent-owned execution
// port without making Bash own call IDs, progress events, or transcript state.
type BashExecutor struct {
	bash *tool.Bash
}

func NewBashExecutor(bash *tool.Bash) (*BashExecutor, error) {
	if bash == nil {
		return nil, fmt.Errorf("%w: bash tool is required", ErrInvalidConfig)
	}
	return &BashExecutor{bash: bash}, nil
}

func (e *BashExecutor) Name() string {
	return tool.BashToolName
}

func (e *BashExecutor) Execute(
	ctx context.Context,
	arguments []byte,
	_ func(ToolUpdate),
) (ToolOutput, error) {
	if e == nil || e.bash == nil {
		return ToolOutput{Text: "Bash tool is not configured"}, errors.New("bash tool is not configured")
	}
	result, err := e.bash.ExecuteJSON(ctx, arguments)
	if !result.Settled() {
		if err == nil {
			err = ErrToolUnsettled
		} else {
			err = errors.Join(ErrToolUnsettled, err)
		}
	}
	if err == nil && !result.Succeeded() {
		err = ErrToolUnsettled
	}
	return ToolOutput{Text: result.Text()}, err
}

type toolPanicError struct {
	value string
	stack []byte
}

func (e *toolPanicError) Error() string {
	return "tool panicked: " + e.value
}

func (e *toolPanicError) Stack() []byte {
	return append([]byte(nil), e.stack...)
}

func executeToolSafely(
	executor ToolExecutor,
	ctx context.Context,
	arguments []byte,
	report func(ToolUpdate),
) (output ToolOutput, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &toolPanicError{value: safeValueText(recovered), stack: debug.Stack()}
			output = ToolOutput{Text: safeErrorText(err)}
		}
	}()
	return executor.Execute(ctx, arguments, report)
}

func executeNamedToolSafely(
	executor ToolExecutor,
	ctx context.Context,
	name string,
	arguments []byte,
	report func(ToolUpdate),
) (output ToolOutput, err error) {
	if named, ok := executor.(NamedToolExecutor); ok {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = &toolPanicError{value: safeValueText(recovered), stack: debug.Stack()}
				output = ToolOutput{Text: safeErrorText(err)}
			}
		}()
		return named.ExecuteNamed(ctx, name, arguments, report)
	}
	return executeToolSafely(executor, ctx, arguments, report)
}

// FilesystemExecutor adapts a complete internal/tool registry to the agent's
// existing execution port. It owns neither tool-call IDs nor transcript state.
type FilesystemExecutor struct {
	registry *tool.Registry
}

// RegistryExecutor exposes any immutable internal/tool registry through the
// existing named executor port. It is used by production so advertised bash
// and filesystem schemas have the same dispatch owner.
type RegistryExecutor struct{ registry *tool.Registry }

func NewRegistryExecutor(registry *tool.Registry) (*RegistryExecutor, error) {
	if registry == nil || len(registry.Names()) == 0 {
		return nil, fmt.Errorf("%w: tool registry is required", ErrInvalidConfig)
	}
	return &RegistryExecutor{registry: registry}, nil
}
func (e *RegistryExecutor) Name() string { return "registry" }
func (e *RegistryExecutor) Supports(name string) bool {
	return e != nil && e.registry != nil && e.registry.Supports(name)
}
func (e *RegistryExecutor) PrepareArguments(name string, arguments any) (any, error) {
	if e == nil || e.registry == nil {
		return arguments, errors.New("tool registry is not configured")
	}
	return e.registry.PrepareArguments(name, arguments)
}
func (e *RegistryExecutor) ToolExecutionMode(name string) (ToolExecutionMode, bool) {
	if e == nil || e.registry == nil {
		return 0, false
	}
	mode, ok := e.registry.ExecutionMode(name)
	if !ok {
		return 0, false
	}
	if mode == tool.ExecutionSequential {
		return ToolExecutionSequential, true
	}
	return ToolExecutionParallel, true
}
func (e *RegistryExecutor) Execute(_ context.Context, _ []byte, _ func(ToolUpdate)) (ToolOutput, error) {
	return ToolOutput{Text: "Tool registry requires a tool name"}, errors.New("tool registry requires a tool name")
}
func (e *RegistryExecutor) ExecuteNamed(ctx context.Context, name string, arguments []byte, _ func(ToolUpdate)) (ToolOutput, error) {
	if e == nil || e.registry == nil {
		return ToolOutput{Text: "Tool registry is not configured"}, errors.New("tool registry is not configured")
	}
	result, err := e.registry.ExecuteJSON(ctx, name, arguments)
	return ToolOutput{
		Text: result.Text, Content: append([]llm.ToolResultContentBlock(nil), result.Content...), Details: result.Details,
	}, err
}

func NewFilesystemExecutor(registry *tool.Registry) (*FilesystemExecutor, error) {
	if registry == nil {
		return nil, fmt.Errorf("%w: filesystem registry is required", ErrInvalidConfig)
	}
	if len(registry.Names()) == 0 {
		return nil, fmt.Errorf("%w: filesystem registry is empty", ErrInvalidConfig)
	}
	return &FilesystemExecutor{registry: registry}, nil
}

func (e *FilesystemExecutor) Name() string { return "filesystem" }
func (e *FilesystemExecutor) Supports(name string) bool {
	return e != nil && e.registry != nil && e.registry.Supports(name)
}
func (e *FilesystemExecutor) PrepareArguments(name string, arguments any) (any, error) {
	if e == nil || e.registry == nil {
		return arguments, errors.New("filesystem tools are not configured")
	}
	return e.registry.PrepareArguments(name, arguments)
}
func (e *FilesystemExecutor) ToolExecutionMode(name string) (ToolExecutionMode, bool) {
	if e == nil || e.registry == nil {
		return 0, false
	}
	mode, ok := e.registry.ExecutionMode(name)
	if !ok {
		return 0, false
	}
	if mode == tool.ExecutionSequential {
		return ToolExecutionSequential, true
	}
	return ToolExecutionParallel, true
}
func (e *FilesystemExecutor) Execute(_ context.Context, _ []byte, _ func(ToolUpdate)) (ToolOutput, error) {
	return ToolOutput{Text: "Filesystem executor requires a tool name"}, errors.New("filesystem executor requires a tool name")
}
func (e *FilesystemExecutor) ExecuteNamed(ctx context.Context, name string, arguments []byte, _ func(ToolUpdate)) (ToolOutput, error) {
	if e == nil || e.registry == nil {
		return ToolOutput{Text: "Filesystem tools are not configured"}, errors.New("filesystem tools are not configured")
	}
	result, err := e.registry.ExecuteJSON(ctx, name, arguments)
	return ToolOutput{
		Text: result.Text, Content: append([]llm.ToolResultContentBlock(nil), result.Content...), Details: result.Details,
	}, err
}

func normalizeToolOutcome(output ToolOutput, err error) (ToolOutput, error) {
	// Rich content is authoritative. A tool may deliberately have no text
	// fallback (for example an image result), so validating/repairing the legacy
	// Text field here would silently discard a valid rich result and its details.
	if output.Content != nil {
		return output, err
	}
	if utf8.ValidString(output.Text) {
		if err == nil || output.Text != "" {
			return output, err
		}
		return ToolOutput{Text: safeErrorText(err)}, err
	}
	invalid := fmt.Errorf("tool output is not valid UTF-8")
	if err != nil {
		invalid = errors.Join(invalid, err)
	}
	return ToolOutput{Text: "Tool execution returned invalid text"}, invalid
}

func safeErrorText(err error) string {
	if err == nil {
		return "Tool execution failed"
	}
	var text string
	func() {
		defer func() {
			if recover() != nil {
				text = "error formatting failed"
			}
		}()
		text = err.Error()
	}()
	text = strings.ToValidUTF8(text, "�")
	if strings.TrimSpace(text) == "" {
		return "Tool execution failed"
	}
	return text
}

func safeValueText(value any) string {
	var text string
	func() {
		defer func() {
			if recover() != nil {
				text = "unprintable panic"
			}
		}()
		text = fmt.Sprint(value)
	}()
	text = strings.ToValidUTF8(text, "�")
	if strings.TrimSpace(text) == "" {
		return "unknown panic"
	}
	return text
}

func validToolUpdate(update ToolUpdate) bool {
	if !utf8.ValidString(update.Text) {
		return false
	}
	for _, block := range update.Content {
		switch block.(type) {
		case llm.TextBlock, llm.ImageBlock:
		default:
			return false
		}
	}
	if _, ok := cloneToolDetails(update.Details); !ok {
		return false
	}
	seen := map[string]struct{}{}
	for _, name := range update.AddedToolNames {
		if !utf8.ValidString(name) || name == "" {
			return false
		}
		if _, ok := seen[name]; ok {
			return false
		}
		seen[name] = struct{}{}
	}
	return true
}

func supportsToolCall(executor ToolExecutor, requestedName string) (supported bool, err error) {
	if isNilInterface(executor) {
		return false, nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("tool lookup panicked: %s", safeValueText(recovered))
		}
	}()
	if named, ok := executor.(NamedToolExecutor); ok {
		return named.Supports(requestedName), nil
	}
	name, err := configuredToolName(executor)
	if err != nil {
		return false, err
	}
	return name == requestedName, nil
}
