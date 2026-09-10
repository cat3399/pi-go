package protocolv1

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/terminal"
)

func decodeTerminal(data []byte) (application.TerminalCommand, error) {
	var input struct {
		Action     terminal.Action `json:"action"`
		TerminalID string          `json:"terminalId"`
		Command    string          `json:"command"`
		Input      string          `json:"input"`
		CWD        string          `json:"cwd"`
		Title      string          `json:"title"`
		After      *int64          `json:"after"`
		Mode       string          `json:"mode"`
		WaitMS     *int            `json:"waitMs"`
		MaxBytes   int             `json:"maxBytes"`
		MaxLines   int             `json:"maxLines"`
		Cols       int             `json:"cols"`
		Rows       int             `json:"rows"`
	}
	if err := json.Unmarshal(data, &input); err != nil {
		return application.TerminalCommand{}, err
	}
	req := terminal.Request{Action: input.Action, ID: input.TerminalID, Command: input.Command, Input: input.Input,
		CWD: input.CWD, Title: input.Title, After: input.After, Mode: input.Mode, MaxBytes: input.MaxBytes, MaxLines: input.MaxLines, Cols: input.Cols, Rows: input.Rows}
	if input.WaitMS != nil {
		if *input.WaitMS < 0 || *input.WaitMS > 30000 {
			return application.TerminalCommand{}, fmt.Errorf("waitMs must be between 0 and 30000")
		}
		wait := time.Duration(*input.WaitMS) * time.Millisecond
		req.Wait = &wait
	}
	return application.TerminalCommand{Request: req}, nil
}

func terminalInfoWire(info terminal.Info) map[string]any {
	result := map[string]any{"id": info.ID, "title": info.Title, "cwd": info.CWD, "shell": info.Shell, "state": info.State,
		"logPath": info.LogPath, "createdAt": info.CreatedAt, "cols": info.Cols, "rows": info.Rows}
	if info.ExitCode != nil {
		result["exitCode"] = *info.ExitCode
	}
	if info.Error != "" {
		result["error"] = info.Error
	}
	return result
}

func terminalResultWire(value terminal.Result) map[string]any {
	result := map[string]any{"start": value.Start, "cursor": value.Cursor, "truncated": value.Truncated, "hasMore": value.HasMore, "reason": value.Reason}
	if value.Terminal != nil {
		result["terminal"] = terminalInfoWire(*value.Terminal)
	}
	if value.Terminals != nil {
		items := make([]map[string]any, len(value.Terminals))
		for i, info := range value.Terminals {
			items[i] = terminalInfoWire(info)
		}
		result["terminals"] = items
	}
	if len(value.Data) > 0 {
		result["data"] = value.Data
	} // JSON base64 preserves arbitrary PTY bytes.
	return result
}
