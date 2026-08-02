//go:build !windows

package tool

import (
	"fmt"
	"os"
)

func resolveShell(customPath string, request RunRequest) (shellConfig, error) {
	if customPath != "" {
		if err := existingShell(customPath); err != nil {
			return shellConfig{}, err
		}
		return shellConfig{path: customPath, arguments: []string{"-c"}}, nil
	}
	if executableAt("/bin/bash") {
		return shellConfig{path: "/bin/bash", arguments: []string{"-c"}}, nil
	}
	if path, ok := lookPathInEnvironment("bash", request.environment, request.workingDir, false); ok {
		return shellConfig{path: path, arguments: []string{"-c"}}, nil
	}
	if path, ok := lookPathInEnvironment("sh", request.environment, request.workingDir, false); ok {
		return shellConfig{path: path, arguments: []string{"-c"}}, nil
	}
	return shellConfig{}, fmt.Errorf("no bash or sh shell found")
}

func platformExecutable(info os.FileInfo) bool {
	return info.Mode().Perm()&0o111 != 0
}
