package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"unicode/utf8"

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

func normalizeToolOutcome(output ToolOutput, err error) (ToolOutput, error) {
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
