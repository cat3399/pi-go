// Package product defines the identity and filesystem namespace shared by all
// pi-go entry points. Resolving a path never creates or imports user data.
package product

const (
	Name                      = "pi-go"
	DirectoryName             = ".pi-go"
	AgentDirectoryEnvironment = "PI_GO_AGENT_DIR"
)

// Version is set by the build's -ldflags and is shared by the core and surfaces.
var Version = "0.1.0-dev"

// Documentation identifies ordinary, locally readable files from one build.
// The values are installation facts, not a second copy of Agent/Session state.
type Documentation struct {
	Version    string
	BuildID    string
	ReadmePath string
	DocsPath   string
	SourcePath string
}
