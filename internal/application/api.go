package application

import (
	"context"
	"encoding/json"
)

// API is the process-local boundary consumed by GUI, TUI, WebUI, and CLI
// surfaces. A transport adapter may project it, but product code should depend
// on this interface instead of HTTP, SSE, or JSON wire types.
type API interface {
	AgentDir() string
	DefaultCWD() string
	BrowseDirectories(context.Context, string) (DirectoryBrowseResult, error)
	ResolveFile(context.Context, string, string) (FileResource, error)
	ListFiles(context.Context, string) (FileList, error)
	DeleteFile(context.Context, string) error
	InspectUploadTargets(context.Context, string, []string) (UploadTargetInspection, error)
	SaveUploads(context.Context, string, []UploadFile, UploadConflictStrategy) (UploadResult, error)
	QueryFileIndex(context.Context, string, string) (FileIndexResult, error)
	GetGitStatus(context.Context, string) (GitStatus, error)
	GetGitFileDiff(context.Context, string, string) (GitFileDiff, error)
	ListWorktrees(context.Context, string) (WorktreeList, error)
	AddWorktree(context.Context, string, string) (WorktreeCreated, error)
	RemoveWorktree(context.Context, string, string, bool) error

	NewSession(context.Context, NewSessionOptions) (State, error)
	Dispatch(context.Context, string, Command) (CommandResult, error)
	LiveState(string) (State, bool, error)

	ListSessions() ([]SessionInfo, error)
	ListProjects() ([]ProjectInfo, error)
	AddProject(context.Context, string) (ProjectInfo, error)
	RemoveProject(context.Context, string) error
	SnapshotSession(string, string) (SessionSnapshot, error)
	SessionExists(string) (bool, error)
	RenameSession(context.Context, string, string) error
	DeleteSession(context.Context, string) error
	ExportSession(context.Context, string) (SessionExport, error)
	ExportSessionJSONL(context.Context, string) (SessionJSONLExport, error)
	ImportSession(context.Context, string, string, string) (SessionImportResult, error)
	GenerateSessionTitle(context.Context, string) (GeneratedSessionTitle, error)
	SessionThinking(context.Context, string, string, int) (string, error)
	OpenBashOutput(context.Context, string, string) (BashOutput, error)
	RunningIDs() []string
	ListModels(context.Context, string) (ModelsSnapshot, error)
	GetUISettings(context.Context, string) (UISettings, error)
	SetTheme(context.Context, string, string) (UISettings, error)
	ListModelProviders(context.Context, string) ([]ProviderAuthInfo, error)
	SetProviderAPIKey(context.Context, string, string) error
	DeleteProviderCredential(context.Context, string, string) error
	StartProviderOAuth(context.Context, string) (*ProviderOAuthLogin, error)
	ReadModelsConfig(context.Context) (ModelsConfigDocument, error)
	WriteModelsConfig(context.Context, ModelsConfigDocument) error
	DiscoverModels(context.Context, string, ModelProviderDraft) (ModelDiscoveryResult, error)
	QueryModelCatalog(context.Context, string, string, string, int) (ModelCatalogResult, error)
	TestModel(context.Context, string, json.RawMessage, json.RawMessage) (ModelProbeResult, error)

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
