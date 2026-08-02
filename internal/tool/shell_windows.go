//go:build windows

package tool

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func resolveShell(customPath string, request RunRequest) (shellConfig, error) {
	if customPath != "" {
		if err := existingShell(customPath); err != nil {
			return shellConfig{}, err
		}
		return windowsBashConfig(customPath), nil
	}

	var searched []string
	for _, variable := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
		if root, ok := environmentValue(request.environment, variable, true); ok && root != "" {
			path := filepath.Join(root, "Git", "bin", "bash.exe")
			searched = append(searched, path)
			if executableAt(path) {
				return windowsBashConfig(path), nil
			}
		}
	}
	if path, ok := lookPathInEnvironment("bash.exe", request.environment, request.workingDir, true); ok {
		return windowsBashConfig(path), nil
	}
	return shellConfig{}, fmt.Errorf("no bash shell found; searched Git Bash paths: %s", strings.Join(searched, ", "))
}

func windowsBashConfig(path string) shellConfig {
	if isLegacyWSLBashPath(path) {
		return shellConfig{path: path, arguments: []string{"-s"}, commandOnStdin: true}
	}
	return shellConfig{path: path, arguments: []string{"-c"}}
}

func platformExecutable(info os.FileInfo) bool {
	return !info.IsDir()
}
