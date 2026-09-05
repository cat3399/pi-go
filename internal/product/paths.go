package product

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func EnvironmentValue(environment []string, key string) string {
	if environment == nil {
		return os.Getenv(key)
	}
	prefix := key + "="
	for index := len(environment) - 1; index >= 0; index-- {
		if strings.HasPrefix(environment[index], prefix) {
			return strings.TrimPrefix(environment[index], prefix)
		}
	}
	return ""
}

func ResolveAgentDirectory(explicit, cwd string, environment []string) (string, error) {
	path := explicit
	if path == "" {
		path = EnvironmentValue(environment, AgentDirectoryEnvironment)
	}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		path = filepath.Join(home, DirectoryName, "agent")
	}
	return ResolvePath(path, cwd)
}

func ResolvePath(path, cwd string) (string, error) {
	if path == "" || !utf8.ValidString(path) || strings.TrimSpace(path) == "" || strings.IndexByte(path, 0) >= 0 {
		return "", fmt.Errorf("path must be non-empty valid text")
	}
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand home directory: %w", err)
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, path[2:])
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	return filepath.Abs(path)
}

func ProjectDirectory(cwd string) string { return filepath.Join(cwd, DirectoryName) }

// KnowledgeDirectory is shared by agent directories under the same data root.
// Each build's initial source content selects a directory users can customize.
func KnowledgeDirectory(agentDir string) string {
	return filepath.Join(filepath.Dir(agentDir), "knowledge")
}
