package application

import "context"

// API is the process-local boundary consumed by GUI, TUI, WebUI, and CLI
// surfaces. A transport adapter may project it, but product code should depend
// on this interface instead of HTTP, SSE, or JSON wire types.
type API interface {
	AgentDir() string
	DefaultCWD() string

	NewSession(context.Context, NewSessionOptions) (State, error)
	Dispatch(context.Context, string, Command) (CommandResult, error)
	LiveState(string) (State, bool, error)

	ListSessions() ([]SessionInfo, error)
	SnapshotSession(string, string) (SessionSnapshot, error)
	SessionExists(string) (bool, error)
	RenameSession(context.Context, string, string) error
	DeleteSession(context.Context, string) error
	ExportSession(context.Context, string) (SessionExport, error)
	GenerateSessionTitle(context.Context, string) (GeneratedSessionTitle, error)
	SessionThinking(context.Context, string, string, int) (string, error)
	OpenBashOutput(context.Context, string, string) (BashOutput, error)
	RunningIDs() []string

	ProjectTrust(context.Context, string) (ProjectTrustStatus, error)
	TrustProject(context.Context, string) (ProjectTrustStatus, error)
	ListSkills(context.Context, string) (SkillsSnapshot, error)
	SetSkillModelInvocation(context.Context, string, string, bool) error
	SearchSkills(context.Context, string, int) ([]SkillSearchResult, error)
	InstallSkill(context.Context, SkillInstallRequest) (SkillInfo, error)
	CheckSkillUpdates(context.Context, SkillUpdateRequest) ([]SkillUpdateResult, error)
	UpdateSkill(context.Context, SkillUpdateRequest) (SkillInfo, error)

	CurrentRevision() uint64
	SubscribeEvents(uint64) (*EventSubscription, error)
}

var _ API = (*Service)(nil)
