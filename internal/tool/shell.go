package tool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type shellConfig struct {
	path           string
	arguments      []string
	commandOnStdin bool
}

func isLegacyWSLBashPath(path string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(path, "/", "\\"))
	if len(normalized) < 3 || normalized[1] != ':' {
		return false
	}
	suffix := normalized[2:]
	return suffix == "\\windows\\system32\\bash.exe" ||
		suffix == "\\windows\\sysnative\\bash.exe"
}

func environmentValue(environment []string, name string, foldCase bool) (string, bool) {
	var value string
	var found bool
	for _, entry := range environment {
		key, candidate, ok := splitEnvironmentEntry(entry)
		if !ok {
			continue
		}
		matches := key == name
		if foldCase {
			matches = strings.EqualFold(key, name)
		}
		if matches {
			value = candidate
			found = true
		}
	}
	return value, found
}

func splitEnvironmentEntry(entry string) (string, string, bool) {
	if entry == "" {
		return "", "", false
	}
	start := 0
	if entry[0] == '=' {
		start = 1
	}
	index := strings.IndexByte(entry[start:], '=')
	if index < 0 {
		return "", "", false
	}
	index += start
	return entry[:index], entry[index+1:], true
}

func executableAt(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return platformExecutable(info)
}

func existingShell(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("Custom shell path not found: %s", path)
		}
		return fmt.Errorf("Cannot inspect custom shell path %s: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("Custom shell path is a directory: %s", path)
	}
	return nil
}

func lookPathInEnvironment(
	name string,
	environment []string,
	workingDir string,
	foldPathKey bool,
) (string, bool) {
	pathValue, ok := environmentValue(environment, "PATH", foldPathKey)
	if !ok {
		return "", false
	}
	for _, directory := range filepath.SplitList(pathValue) {
		if directory == "" {
			directory = "."
		}
		if !filepath.IsAbs(directory) {
			directory = filepath.Join(workingDir, directory)
		}
		candidate := filepath.Join(directory, name)
		if executableAt(candidate) {
			absolute, err := filepath.Abs(candidate)
			if err == nil {
				return filepath.Clean(absolute), true
			}
			return candidate, true
		}
	}
	return "", false
}
