package tui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
)

type externalEditorFinishedMsg struct {
	path string
	err  error
}

func prepareExternalEditor(environment []string, cwd, content string) (*exec.Cmd, string, error) {
	root := "/tmp"
	if runtime.GOOS == "windows" {
		root = os.TempDir()
	}
	return prepareExternalEditorIn(root, environment, cwd, content)
}

func prepareExternalEditorIn(root string, environment []string, cwd, content string) (*exec.Cmd, string, error) {
	directory, err := os.MkdirTemp(root, "pi-go-editor-")
	if err != nil {
		return nil, "", fmt.Errorf("create external editor workspace: %w", err)
	}
	path := filepath.Join(directory, "prompt.md")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return nil, path, fmt.Errorf("write external editor draft: %w", err)
	}
	specification := strings.TrimSpace(environmentValue(environment, "VISUAL"))
	if specification == "" {
		specification = strings.TrimSpace(environmentValue(environment, "EDITOR"))
	}
	if specification == "" {
		if runtime.GOOS == "windows" {
			specification = "notepad"
		} else {
			specification = "vi"
		}
	}
	parts, err := splitEditorCommand(specification)
	if err != nil {
		return nil, path, err
	}
	command := exec.Command(parts[0], append(parts[1:], path)...)
	if strings.TrimSpace(cwd) != "" {
		command.Dir = cwd
	}
	if environment != nil {
		command.Env = append([]string(nil), environment...)
	}
	return command, path, nil
}

func splitEditorCommand(value string) ([]string, error) {
	var result []string
	var current []rune
	var quote rune
	escaped := false
	flush := func() {
		if len(current) != 0 {
			result = append(result, string(current))
			current = nil
		}
	}
	for _, char := range value {
		if escaped {
			current = append(current, char)
			escaped = false
			continue
		}
		if char == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
			} else {
				current = append(current, char)
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			continue
		}
		if unicode.IsSpace(char) {
			flush()
			continue
		}
		current = append(current, char)
	}
	if escaped || quote != 0 {
		return nil, errors.New("EDITOR or VISUAL contains an unmatched quote or escape")
	}
	flush()
	if len(result) == 0 {
		return nil, errors.New("EDITOR or VISUAL is empty")
	}
	return result, nil
}
