package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestToolRendererCollapsesGenericOutputAndExpandsWithoutDataLoss(t *testing.T) {
	renderer := newContentRenderer(DefaultTheme())
	rows := make([]string, 25)
	for index := range rows {
		rows[index] = fmt.Sprintf("ROW_%02d", index+1)
	}
	item := toolContentItem("call", "custom", `{"query":"value"}`, strings.Join(rows, "\n"), false)

	collapsed := StripTerminalSequences(strings.Join(renderer.Render(item, 100), "\n"))
	if !strings.Contains(collapsed, "ROW_01") || strings.Contains(collapsed, "ROW_11") || !strings.Contains(collapsed, "15 more lines") {
		t.Fatalf("collapsed tool output:\n%s", collapsed)
	}

	renderer.SetToolsExpanded(true)
	expanded := StripTerminalSequences(strings.Join(renderer.Render(item, 100), "\n"))
	if !strings.Contains(expanded, "ROW_25") || strings.Contains(expanded, "more lines") {
		t.Fatalf("expanded tool output:\n%s", expanded)
	}
}

func TestReadRendererHidesSuccessfulBodyButKeepsErrorsVisible(t *testing.T) {
	renderer := newContentRenderer(DefaultTheme())
	item := toolContentItem("read-call", "read", `{"path":"internal/agent.go"}`, "PRIVATE_BODY", false)
	collapsed := StripTerminalSequences(strings.Join(renderer.Render(item, 100), "\n"))
	if !strings.Contains(collapsed, "read internal/agent.go") || strings.Contains(collapsed, "PRIVATE_BODY") || !strings.Contains(collapsed, "lines hidden") {
		t.Fatalf("collapsed read output:\n%s", collapsed)
	}

	item.Blocks[1].IsError = true
	failed := StripTerminalSequences(strings.Join(renderer.Render(item, 100), "\n"))
	if !strings.Contains(failed, "PRIVATE_BODY") {
		t.Fatalf("read error was hidden:\n%s", failed)
	}
}

func TestBashRendererUsesVisualTailPreview(t *testing.T) {
	renderer := newContentRenderer(DefaultTheme())
	rows := make([]string, 8)
	for index := range rows {
		rows[index] = fmt.Sprintf("BASH_%02d", index+1)
	}
	item := toolContentItem("bash-call", "bash", `{"command":"generate"}`, strings.Join(rows, "\n"), false)
	view := StripTerminalSequences(strings.Join(renderer.Render(item, 100), "\n"))
	if strings.Contains(view, "BASH_03") || !strings.Contains(view, "BASH_04") || !strings.Contains(view, "BASH_08") || !strings.Contains(view, "3 more lines") {
		t.Fatalf("bash tail preview:\n%s", view)
	}
}

func TestToolRendererSanitizesUntrustedTerminalControls(t *testing.T) {
	renderer := newContentRenderer(DefaultTheme())
	item := toolContentItem(
		"unsafe-call", "custom", `{}`,
		"safe\x1b]52;c;ZXZpbA==\aafter\x1b[2Jdone\x00",
		false,
	)
	view := StripTerminalSequences(strings.Join(renderer.Render(item, 100), "\n"))
	if strings.Contains(view, "ZXZpbA==") || !strings.Contains(view, "safeafterdone") {
		t.Fatalf("sanitized output:\n%s", view)
	}
}

func TestMergeToolResultBlocksKeepsOneExecutionTransaction(t *testing.T) {
	item := contentItem{
		ID: "assistant", Revision: 1, Role: contentRoleAssistant,
		Blocks: []contentBlock{
			{Kind: contentBlockText, Text: "before"},
			{Kind: contentBlockToolCall, ToolCallID: "call", ToolName: "read", Text: `{"path":"a.go"}`},
			{Kind: contentBlockText, Text: "after"},
		},
	}
	result := []contentBlock{{
		Kind: contentBlockToolResult, ToolCallID: "call", ToolName: "read", Text: "contents",
		ToolDetails: json.RawMessage(`{"truncation":{"truncated":true}}`),
	}}
	if !mergeToolResultBlocks(&item, "call", result) {
		t.Fatal("tool result did not merge")
	}
	if len(item.Blocks) != 4 || item.Blocks[1].Kind != contentBlockToolCall || item.Blocks[2].Kind != contentBlockToolResult || item.Blocks[3].Text != "after" {
		t.Fatalf("merged blocks = %#v", item.Blocks)
	}
	if item.Revision != 2 || string(item.Blocks[2].ToolDetails) != string(result[0].ToolDetails) {
		t.Fatalf("merged item = %#v", item)
	}
}

func toolContentItem(callID, name, arguments, output string, failed bool) contentItem {
	return contentItem{
		ID: "tool:" + callID, Revision: 1, Role: contentRoleTool, Title: name, Failed: failed,
		Blocks: []contentBlock{
			{Kind: contentBlockToolCall, ToolCallID: callID, ToolName: name, Text: arguments, IsError: failed},
			{Kind: contentBlockToolResult, ToolCallID: callID, ToolName: name, Text: output, IsError: failed},
		},
	}
}
