package webui

type CapabilityStatus string

const (
	CapabilityComplete    CapabilityStatus = "complete"
	CapabilityConnected   CapabilityStatus = "connected"
	CapabilityInProgress  CapabilityStatus = "in_progress"
	CapabilityUnsupported CapabilityStatus = "unsupported"
	CapabilityDeferred    CapabilityStatus = "deferred"
)

type Capability struct {
	ID          string           `json:"id"`
	Status      CapabilityStatus `json:"status"`
	Description string           `json:"description"`
}

// Capabilities is the executable product boundary, not a marketing list. A
// module may only move to connected/complete when its real Go path is wired.
func Capabilities() []Capability {
	return []Capability{
		{ID: "visual_shell", Status: CapabilityComplete, Description: "pi-web layout, theme, responsive CSS, components, localization and static assets"},
		{ID: "native_web_host", Status: CapabilityConnected, Description: "optional Go HTTP host serves the embedded frontend and calls Host in-process"},
		{ID: "agent_chat", Status: CapabilityConnected, Description: "prompt, abort, queue, tools, model/thinking, compaction, bash and ordered SSE events use the in-process Agent Host"},
		{ID: "sessions", Status: CapabilityConnected, Description: "multi-session lifecycle, discovery, restore, context projection, branch tree, state and rename are implemented in Go"},
		{ID: "models", Status: CapabilityConnected, Description: "Go catalog discovery and live Host model selection are connected; auth/configuration management remains unsupported"},
		{ID: "files", Status: CapabilityUnsupported, Description: "file explorer, preview and upload Web APIs are not connected yet"},
		{ID: "git_worktrees", Status: CapabilityUnsupported, Description: "Git status, diff and worktree Web APIs are not connected yet"},
		{ID: "plugins", Status: CapabilityDeferred, Description: "plugin and extension management is intentionally deferred"},
		{ID: "skills_management", Status: CapabilityDeferred, Description: "skill search, install and update UI is intentionally deferred"},
	}
}
