// Package product defines the identity and filesystem namespace shared by all
// pi-go entry points. Resolving a path never creates or imports user data.
package product

import "strings"

const (
	Name                      = "pi-go"
	DirectoryName             = ".pi-go"
	AgentDirectoryEnvironment = "PI_GO_AGENT_DIR"
)

// Version is set by the build's -ldflags and is shared by the core and surfaces.
var Version = "0.1.0-dev"

// StableVersion recognizes release versions (or tags), without a prerelease or
// build suffix. Keep decimal components as strings; SemVer does not bound them.
func StableVersion(value string) (string, bool) {
	value = strings.TrimPrefix(value, "v")
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return "", false
	}
	for _, part := range parts {
		if part == "" || len(part) > 1 && part[0] == '0' {
			return "", false
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return "", false
			}
		}
	}
	return value, true
}

// Documentation identifies ordinary, locally readable files from one build.
// The values are installation facts, not a second copy of Agent/Session state.
type Documentation struct {
	Version    string
	BuildID    string
	ReadmePath string
	DocsPath   string
	SourcePath string
}
