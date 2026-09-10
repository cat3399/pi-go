package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cat3399/pi-go/internal/terminal"
)

const TerminalToolName = "terminal"

type terminalTool struct{ service *terminal.Service }

func (terminalTool) Name() string                     { return TerminalToolName }
func (terminalTool) ToolExecutionMode() ExecutionMode { return ExecutionSequential }

func (t terminalTool) ExecuteJSON(ctx context.Context, arguments []byte) (ToolResult, error) {
	var input struct {
		Action      terminal.Action `json:"action"`
		ID          string          `json:"id"`
		Command     string          `json:"command"`
		Input       string          `json:"input"`
		CWD         string          `json:"cwd"`
		Title       string          `json:"title"`
		After       *int64          `json:"after"`
		Mode        string          `json:"mode"`
		WaitMS      *int            `json:"wait_ms"`
		MaxBytes    int             `json:"max_bytes"`
		MaxLines    int             `json:"max_lines"`
		IncludeUser bool            `json:"include_user"`
	}
	if err := json.Unmarshal(arguments, &input); err != nil {
		return ToolResult{Text: err.Error()}, err
	}
	req := terminal.Request{Action: input.Action, ID: input.ID, Command: input.Command, Input: input.Input,
		CWD: input.CWD, Title: input.Title, After: input.After, Mode: input.Mode, MaxBytes: input.MaxBytes, MaxLines: input.MaxLines, IncludeUser: input.IncludeUser}
	if input.WaitMS != nil {
		if *input.WaitMS < 0 || *input.WaitMS > 30000 {
			return ToolResult{Text: "wait_ms must be between 0 and 30000"}, fmt.Errorf("invalid terminal wait_ms")
		}
		wait := time.Duration(*input.WaitMS) * time.Millisecond
		req.Wait = &wait
	}
	result, err := t.service.Do(ctx, req, terminal.Agent)
	if err != nil {
		return ToolResult{Text: err.Error()}, err
	}
	if input.Action == terminal.List {
		var lines []string
		for _, item := range result.Terminals {
			lines = append(lines, fmt.Sprintf("%s · %s · %s", item.ID, item.Title, item.State))
		}
		if len(lines) == 0 {
			lines = append(lines, "No terminals")
		}
		return ToolResult{Text: strings.Join(lines, "\n")}, nil
	}
	info := result.Terminal
	text := fmt.Sprintf("Terminal %s (%s) · cursor %d", info.ID, info.State, result.Cursor)
	if result.Reason != "" {
		text += " · " + result.Reason
	}
	if result.Text != "" {
		text += "\n" + result.Text
	}
	if result.Truncated {
		text += "\n[Output truncated. Full output: " + info.LogPath + "]"
	}
	if info.Error != "" {
		text += "\n" + info.Error
	}
	details := map[string]any{"terminalId": info.ID, "title": info.Title, "state": info.State, "cursor": result.Cursor,
		"reason": result.Reason, "truncated": result.Truncated, "fullOutputPath": info.LogPath}
	if info.ExitCode != nil {
		details["exitCode"] = *info.ExitCode
	}
	return ToolResult{Text: text, Details: details}, nil
}

func terminalSpecification() Specification {
	return mustBuiltInSpecification(TerminalToolName,
		"Persistent interactive terminal. open creates an independent shell and returns its id; list returns existing terminals. run sends command(s) with a final newline; write sends exact input (use \\n for Enter); read returns output; interrupt sends Ctrl-C; close ends the shell. run/read/write/interrupt/close require an existing non-empty id. Shell state persists across calls to that id. wait_ms defaults to 1000, has a hard 30000 maximum; 0 returns immediately. A returned read means only the wait ended, not that the command finished. read modes: since (new output, default), tail (recent output), follow (wait for the next output). Output is capped at 50 KiB/2000 lines and saved to a temporary log. Shared user activity is omitted by default; include_user shows it. Attribution follows the latest input and may overlap. Keep bash for ordinary one-shot commands.",
		"Use persistent terminals for interactive programs or commands that need shared shell state.", nil,
		`{
  "type": "object",
  "properties": {
    "action": {"type":"string","enum":["open","run","read","write","list","interrupt","close"],"description":"open creates a terminal; list discovers existing terminals; all other actions operate on id"},
    "id": {"type":"string","minLength":1,"pattern":"\\S","description":"Existing terminal ID from open or list; required for run/read/write/interrupt/close"},
    "command": {"type":"string","minLength":1,"pattern":"\\S","description":"Command text for run; a final newline is added"},
    "input": {"type":"string","description":"Exact input for write, including any newline or control characters"},
    "cwd": {"type":"string","description":"Initial working directory for open; defaults to the session working directory"},
    "title": {"type":"string","maxLength":120,"description":"Display title for open"},
    "after": {"type":"integer","minimum":0,"description":"Explicit output byte cursor for read; omitted uses the Agent's cursor"},
    "mode": {"type":"string","enum":["since","tail","follow"],"description":"Output reading mode; defaults to since"},
    "wait_ms": {"type":"integer","minimum":0,"maximum":30000,"description":"Maximum wait for this operation, default 1000; 0 returns immediately"},
    "max_bytes": {"type":"integer","minimum":1,"maximum":51200},
    "max_lines": {"type":"integer","minimum":1,"maximum":2000},
    "include_user": {"type":"boolean","description":"Include output associated with user input; defaults to false"}
  },
  "required": ["action"],
  "oneOf": [
    {"properties":{"action":{"enum":["open","list"]}}},
    {"properties":{"action":{"const":"run"}},"required":["id","command"]},
    {"properties":{"action":{"const":"write"}},"required":["id","input"]},
    {"properties":{"action":{"enum":["read","interrupt","close"]}},"required":["id"]}
  ],
  "additionalProperties": false
}`)
}
