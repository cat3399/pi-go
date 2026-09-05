package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

// SessionConfig supplies the long-lived product state around the in-memory
// Agent and its AgentLoop execution core.
// Provider and SessionManager are lifecycle dependencies. Agent owns the
// prompt-visible runtime state; AgentSession supplies its initial values and
// product services, then reads immutable Agent snapshots per provider turn.
type SessionConfig struct {
	Provider       provider.Provider
	SessionManager *session.SessionManager
	Model          provider.Model
	ThinkingLevel  provider.ThinkingLevel
	SystemPrompt   string
	// SystemPromptOptions preserves the structured inputs exposed by the
	// original before_agent_start hook. Product resource assembly may populate
	// the complete value; AgentSession fills CWD and selected tool names when
	// callers leave those fields unset.
	SystemPromptOptions BuildSystemPromptOptions
	Tool                ToolExecutor
	// Tools is the initially active provider-visible set. AllTools retains the
	// complete registry so tools can be enabled later without reconstructing
	// the executor. A nil AllTools keeps low-level callers source-compatible by
	// treating Tools as both the registry and active set.
	Tools           []provider.ToolDefinition
	AllTools        []provider.ToolDefinition
	ActiveToolNames []string
	ToolMetadata    map[string]ToolMetadata
	// Resources is the last-healthy resource snapshot owned through the
	// AgentSession lifecycle. It rebuilds the prompt when active tools change
	// and expands skill/template commands after extension command/input
	// preflight and before message construction.
	Resources SessionResources
	// ReloadRuntime refreshes settings/catalog state owned outside AgentSession.
	// AgentSession invokes it after session_shutdown(reload) and before queue
	// modes/resources are refreshed. Nil leaves low-level injected sessions with
	// resource-only reload behavior.
	ReloadRuntime func(context.Context) error
	// ReloadTools rebuilds the complete non-extension tool runtime after
	// settings and resources have reloaded. The returned runtime is validated
	// off to the side and then published while preserving the latest active tool
	// names. Nil keeps injected/low-level sessions on their existing registry.
	ReloadTools func(context.Context) (ToolRuntime, error)
	// StandaloneBash is the user-initiated !/!! execution port. It is distinct
	// from Tool because its result is a BashExecution AgentMessage, non-zero exit
	// codes are data, and output is streamed through session events.
	StandaloneBash StandaloneBashExecutor
	// ResolveStandaloneBash mirrors executeBash's live SettingsManager shell
	// lookup. A custom per-call ExecuteBashOptions.Executor still takes
	// precedence. Nil uses StandaloneBash as a stable injected fallback.
	ResolveStandaloneBash func(context.Context) (StandaloneBashExecutor, error)
	// BashCommandPrefix is prepended to the executed command but not the command
	// stored in BashExecution history. ResolveBashCommandPrefix provides the
	// live SettingsManager-style production path when configured.
	BashCommandPrefix        string
	ResolveBashCommandPrefix func() string
	BeforeToolCall           BeforeToolCallHook
	AfterToolCall            AfterToolCallHook
	Stream                   provider.StreamOptions
	// ResolveStreamOptions resolves credentials and request headers for the
	// model selected by a concrete turn. It is invoked outside session locks.
	ResolveStreamOptions func(context.Context, provider.Model) (provider.StreamOptions, error)
	// ValidateModelAccess performs transport-neutral credential/admission checks
	// for the selected model. Product factories use it to reject inaccessible
	// models before prompt construction, hooks, compaction, persistence, or a
	// provider request. Nil preserves the low-level constructor's behavior.
	ValidateModelAccess func(context.Context, provider.Model) error
	// ValidateModelSelection is the corresponding model-selection boundary.
	// It is separate because the original setModel surface has distinct
	// user-facing authentication errors from prompt and compaction.
	ValidateModelSelection func(context.Context, provider.Model) error
	// AllModels and ScopedModels are the model-cycle inputs assembled by the
	// product factory. They are copied on construction and never expose mutable
	// session-owned slices to callers.
	AllModels    []provider.Model
	ScopedModels []ScopedModel
	// ModelAvailable is the credential/route check used while cycling. A nil
	// callback treats every configured model as available, matching low-level
	// injected sessions which have already admitted their catalog.
	ModelAvailable func(context.Context, provider.Model) (bool, error)
	// ResolveAvailableModels refreshes the unscoped cycle list on every call,
	// matching ModelRuntime.getAvailable(). Nil uses AllModels as a low-level
	// static fallback.
	ResolveAvailableModels func(context.Context) ([]provider.Model, error)
	// DefaultThinkingLevel is the settings preference restored when switching
	// from a non-reasoning model. Empty means pi's default (medium).
	DefaultThinkingLevel provider.ThinkingLevel
	// ResolveDefaultThinkingLevel reads the current effective (global merged
	// with trusted project) setting at the instant model-switch preference is
	// chosen. Nil uses DefaultThinkingLevel as the low-level fallback.
	ResolveDefaultThinkingLevel func() (provider.ThinkingLevel, bool)
	// PersistSettings writes the global default provider/model/thinking fields.
	// AgentSession invokes it before transcript/state publication and uses its
	// conditional undo after a definite transcript failure. Commit-unknown
	// outcomes deliberately keep the forward setting for later reconciliation.
	PersistSettings SettingsPersistence
	Hooks           Hooks
	// SessionStartEvent selects the lifecycle reason emitted after construction.
	// Nil preserves the ordinary process-start behavior.
	SessionStartEvent *SessionStartHookEvent
	// NoModelSelectedMessage is the product-facing guidance returned by prompt
	// and continue while no model is selected. Empty uses ErrNoModelSelected's
	// low-level text; factories may inject installation-specific docs paths.
	NoModelSelectedMessage string
	// InitializeSessionState performs createAgentSession's initial durable
	// model/thinking metadata writes as part of real AgentSession construction,
	// before session_start is observed. The zero value keeps direct low-level
	// constructors source-compatible; the transport-neutral session factory
	// enables it for product sessions.
	InitializeSessionState bool

	ToolExecution    ToolExecutionMode
	TransformContext ContextTransform
	// ConvertToLLM is the final AgentMessage-to-provider boundary. Product
	// assembly uses it for the original dynamic blockImages defense while
	// retaining unmodified rich content in memory and session storage.
	ConvertToLLM     AgentLoopConvertToLLM
	GetAPIKey        AgentLoopAPIKey
	SteeringMode     QueueMode
	FollowUpMode     QueueMode
	ContextWindow    uint64
	ContextReserve   uint64
	KeepRecentTokens uint64
	// Presence flags allow production settings to preserve an explicit zero.
	// Existing low-level callers retain zero-means-default behavior.
	ContextReserveSet   bool
	KeepRecentTokensSet bool
	// CompactionEnabled follows pi's settings.compaction.enabled. Nil uses the
	// upstream default (enabled); a pointer is required so false remains a real
	// configured value.
	CompactionEnabled *bool
	// AutoRetryEnabled gates retries without discarding the configured retry
	// budget. Nil follows pi's default (enabled).
	AutoRetryEnabled *bool
	// ResolveRuntimeSettings reads current effective settings at each dynamic
	// control boundary. Production backs it with ModelRuntime.Snapshot.
	ResolveRuntimeSettings func() RuntimeControlSettings
	Summarizer             session.Summarizer
	// ResolveSummarizer is the production seam. It is invoked once per
	// compaction after current model/thinking/stream auth have been snapshotted.
	// Summarizer remains the static injection seam for deterministic tests and
	// embedders.
	ResolveSummarizer func(context.Context, SummarizerResolveRequest) (session.Summarizer, error)
	// BranchSummarizer and ResolveBranchSummarizer are the corresponding seams
	// for navigateTree branch summaries. Production normally resolves the same
	// ContextSummarizer implementation used by compaction.
	BranchSummarizer           session.BranchSummarizer
	ResolveBranchSummarizer    func(context.Context, SummarizerResolveRequest) (session.BranchSummarizer, error)
	BranchSummaryReserveTokens uint64
	BranchSummaryReserveSet    bool
	Retry                      RetryPolicy
	Now                        func() time.Time
	SettlementTimeout          time.Duration
}

// ToolRuntime is one complete, publishable generation of the tool execution
// boundary. Tools is the full registry; AgentSession selects the active subset
// by name and owns publication with the effective system prompt.
type ToolRuntime struct {
	Executor       ToolExecutor
	Tools          []provider.ToolDefinition
	Metadata       map[string]ToolMetadata
	StandaloneBash StandaloneBashExecutor
}

// ToolMetadata is prompt/UI metadata that does not belong in the
// provider-visible function definition.
type ToolMetadata struct {
	PromptGuidelines []string
	SourceInfo       SystemPromptSourceInfo
}

// ToolInfo is the complete inspection projection exposed by AgentSession.
type ToolInfo struct {
	Definition       provider.ToolDefinition
	PromptGuidelines []string
	SourceInfo       SystemPromptSourceInfo
}

const (
	DefaultCompactionReserveTokens    uint64 = 16_384
	DefaultCompactionKeepRecentTokens uint64 = 20_000
)

// SummarizerResolveRequest contains the exact request-scoped state selected
// for one compaction. Stream includes dynamically resolved auth; the concrete
// summarizer adds request-local cache/session isolation when it actually runs.
type SummarizerResolveRequest struct {
	Model         provider.Model
	ThinkingLevel provider.ThinkingLevel
	Stream        provider.StreamOptions
	Retry         RetryPolicy
}

// RuntimeControlSettings is the effective settings view used by controls that
// original AgentSession reads dynamically from SettingsManager.
type RuntimeControlSettings struct {
	SteeringMode               QueueMode
	FollowUpMode               QueueMode
	AutoCompactionEnabled      bool
	AutoRetryEnabled           bool
	Retry                      RetryPolicy
	CompactionReserveTokens    uint64
	CompactionReserveSet       bool
	CompactionKeepRecentTokens uint64
	CompactionKeepRecentSet    bool
	BranchSummaryReserveTokens uint64
	BranchSummaryReserveSet    bool
}

// SessionState is a copy-only view of the mutable product configuration.
// Active Agent state owns the current runtime conversation; SessionManager
// entries and BuildContext own its durable form and resume reconstruction.
type SessionState struct {
	Model         provider.Model
	HasModel      bool
	ThinkingLevel provider.ThinkingLevel
	SystemPrompt  string
	Tools         []provider.ToolDefinition
	Active        State
}

// SessionActivity is the high-level product lifecycle projection exposed to
// hosts. IsStreaming deliberately tracks the complete Agent prompt lifecycle
// (including retry waits, automatic compaction, and queued continuations), not
// merely an individual provider request. Manual compaction and tree navigation
// can therefore be compacting while IsStreaming remains false, as in pi.
type SessionActivity struct {
	Phase        Phase
	IsStreaming  bool
	IsCompacting bool
	RetryAttempt uint32
	RetryWaiting bool
}

// SessionEvent is pi's AgentSessionEvent union. Core AgentEvent members are
// reused directly; session-only control members have their own concrete
// structs and therefore cannot carry unrelated zero-valued fields.
type SessionEvent interface {
	Type() AgentEventType
	sessionEvent()
}

const (
	AgentSettledEventType         AgentEventType = "agent_settled"
	ThinkingLevelChangedEventType AgentEventType = "thinking_level_changed"
	AutoRetryStartEventType       AgentEventType = "auto_retry_start"
	AutoRetryEndEventType         AgentEventType = "auto_retry_end"
	EntryAppendedEventType        AgentEventType = "entry_appended"
	SessionInfoChangedEventType   AgentEventType = "session_info_changed"
	BashExecutionUpdateEventType  AgentEventType = "bash_execution_update"
)

type SessionAgentEndEvent struct {
	Messages  []agentmsg.Message
	Terminal  llm.AssistantTerminal
	WillRetry bool
}
type AgentSettledEvent struct{}
type SessionQueueUpdateEvent struct {
	Steering         []string
	FollowUp         []string
	SteeringMessages []llm.ConversationMessage
	FollowUpMessages []llm.ConversationMessage
}

// QueueState is a copy-only view of AgentSession's pending product queues.
// The text fields match coding-agent's public queue contract; the rich fields
// retain Go's image/content messages without forcing a transport to lose them.
type QueueState struct {
	Steering         []string
	FollowUp         []string
	SteeringMessages []llm.ConversationMessage
	FollowUpMessages []llm.ConversationMessage
}
type ThinkingLevelChangedEvent struct{ Level provider.ThinkingLevel }
type AutoRetryStartEvent struct {
	Attempt      uint32
	MaxAttempts  uint32
	Delay        time.Duration
	ErrorMessage string
}
type AutoRetryEndEvent struct {
	Success    bool
	Attempt    uint32
	FinalError string
}
type SessionSummarizationRetryScheduledEvent struct {
	Attempt      uint32
	MaxAttempts  uint32
	Delay        time.Duration
	ErrorMessage string
	Reason       CompactionReason
	FailureKind  provider.FailureKind
	HTTPStatus   int
}
type SessionSummarizationRetryAttemptEvent struct {
	Source string
	Reason CompactionReason
}
type SessionSummarizationRetryFinishedEvent struct {
	Reason       CompactionReason
	Attempt      uint32
	FailureKind  provider.FailureKind
	HTTPStatus   int
	Succeeded    bool
	FinishReason provider.RetryFinishReason
	FinalError   string
}
type EntryAppendedEvent struct{ Entry session.Entry }
type SessionInfoChangeEvent struct{ Name *string }
type BashExecutionUpdateEvent struct {
	ID    *string
	Delta string
}

func (SessionAgentEndEvent) Type() AgentEventType      { return AgentEndEventType }
func (AgentSettledEvent) Type() AgentEventType         { return AgentSettledEventType }
func (SessionQueueUpdateEvent) Type() AgentEventType   { return QueueUpdateEventType }
func (ThinkingLevelChangedEvent) Type() AgentEventType { return ThinkingLevelChangedEventType }
func (AutoRetryStartEvent) Type() AgentEventType       { return AutoRetryStartEventType }
func (AutoRetryEndEvent) Type() AgentEventType         { return AutoRetryEndEventType }
func (SessionSummarizationRetryScheduledEvent) Type() AgentEventType {
	return SummarizationRetryScheduledEventType
}
func (SessionSummarizationRetryAttemptEvent) Type() AgentEventType {
	return SummarizationRetryAttemptEventType
}
func (SessionSummarizationRetryFinishedEvent) Type() AgentEventType {
	return SummarizationRetryFinishedEventType
}
func (EntryAppendedEvent) Type() AgentEventType       { return EntryAppendedEventType }
func (SessionInfoChangeEvent) Type() AgentEventType   { return SessionInfoChangedEventType }
func (BashExecutionUpdateEvent) Type() AgentEventType { return BashExecutionUpdateEventType }

func (AgentStartEvent) sessionEvent()                         {}
func (TurnStartEvent) sessionEvent()                          {}
func (TurnEndEvent) sessionEvent()                            {}
func (MessageStartEvent) sessionEvent()                       {}
func (MessageUpdateEvent) sessionEvent()                      {}
func (MessageEndEvent) sessionEvent()                         {}
func (ToolExecutionStartEvent) sessionEvent()                 {}
func (ToolExecutionUpdateEvent) sessionEvent()                {}
func (ToolExecutionEndEvent) sessionEvent()                   {}
func (CompactionStartEvent) sessionEvent()                    {}
func (CompactionEndEvent) sessionEvent()                      {}
func (SessionAgentEndEvent) sessionEvent()                    {}
func (AgentSettledEvent) sessionEvent()                       {}
func (SessionQueueUpdateEvent) sessionEvent()                 {}
func (ThinkingLevelChangedEvent) sessionEvent()               {}
func (AutoRetryStartEvent) sessionEvent()                     {}
func (AutoRetryEndEvent) sessionEvent()                       {}
func (SessionSummarizationRetryScheduledEvent) sessionEvent() {}
func (SessionSummarizationRetryAttemptEvent) sessionEvent()   {}
func (SessionSummarizationRetryFinishedEvent) sessionEvent()  {}
func (EntryAppendedEvent) sessionEvent()                      {}
func (SessionInfoChangeEvent) sessionEvent()                  {}
func (BashExecutionUpdateEvent) sessionEvent()                {}

type SessionObserver func(context.Context, SessionEvent)

// AgentSession owns the persistent agent product state. It never invokes
// providers, tools, session writes, or observers while holding mu. The
// loop calls prepareTurn immediately before each provider request, so a
// model/tool/prompt change made while tools are running applies to the next
// request in that same run.
type AgentSession struct {
	mu sync.RWMutex
	// controlMu serializes every settings-backed persist+publish operation,
	// including model/thinking and runtime controls, after any asynchronous
	// discovery phase. Lock order is controlMu,
	// lifecycleMu, selectionMu, then the Agent/AgentSession state mutexes.
	controlMu sync.Mutex
	// selectionMu keeps selection readers outside the final in-memory publish
	// step of a durable/settings transaction. The selected values themselves
	// live only in Agent state.
	selectionMu            sync.RWMutex
	loop                   *Agent
	sessionManager         *session.SessionManager
	systemOptions          BuildSystemPromptOptions
	resources              SessionResources
	reloadRuntime          func(context.Context) error
	reloadTools            func(context.Context) (ToolRuntime, error)
	standaloneBash         StandaloneBashExecutor
	resolveStandaloneBash  func(context.Context) (StandaloneBashExecutor, error)
	bashCommandPrefix      string
	resolveBashPrefix      func() string
	toolExecutor           ToolExecutor
	toolRegistry           map[string]provider.ToolDefinition
	toolOrder              []string
	toolMetadata           map[string]ToolMetadata
	beforeToolCall         BeforeToolCallHook
	afterToolCall          AfterToolCallHook
	stream                 provider.StreamOptions
	resolveStream          func(context.Context, provider.Model) (provider.StreamOptions, error)
	validateAccess         func(context.Context, provider.Model) error
	validateSelect         func(context.Context, provider.Model) error
	allModels              []provider.Model
	scopedModels           []ScopedModel
	modelAvailable         func(context.Context, provider.Model) (bool, error)
	resolveAvailableModels func(context.Context) ([]provider.Model, error)
	defaultThinking        provider.ThinkingLevel
	resolveDefaultThinking func() (provider.ThinkingLevel, bool)
	persistSettings        SettingsPersistence
	appendModelControl     func(context.Context, string, string, *string) ([]session.Entry, error)
	appendThinkingControl  func(context.Context, string) (session.Entry, error)
	hooks                  Hooks
	noModelMessage         string
	// lifecycleMu owns admission, close state, and the complete top-level
	// lifecycle.  A low Agent run is only one phase of sessionRun: retry waits
	// and post-run continuations remain active too.
	lifecycleMu sync.Mutex
	run         *sessionRun
	// pendingNextTurn mirrors coding-agent's deliverAs:"nextTurn" buffer. It is
	// consumed only by the next ordinary prompt, after model/compaction
	// preflight and before before_agent_start messages are injected.
	pendingNextTurn []agentmsg.Message
	// standaloneMutation reserves the idle transcript while an extension custom
	// message is durably appended without triggering an agent turn.
	standaloneMutation bool
	standaloneDone     chan struct{}
	// bashRecordMu linearizes completion-order persistence with low Agent run
	// starts. bashMu independently owns all concurrently executing shell jobs.
	bashRecordMu sync.Mutex
	pendingBash  []agentmsg.BashExecution
	bashMu       sync.Mutex
	bashNextID   uint64
	bashRuns     map[uint64]context.CancelCauseFunc
	bashIdle     chan struct{}
	// reloading gates the in-place product refresh without occupying run, so an
	// active agent turn may continue while settings/resources are rebuilt.
	reloading  bool
	reloadDone chan struct{}
	// idleWait is the shared idle-generation latch. A run admitted from an
	// agent_settled callback joins the same generation, so callers that began
	// waiting on the preceding run do not return until the replacement run also
	// settles. run.done remains the completion signal for one sessionRun only.
	idleWait                chan struct{}
	settlingCallbacks       uint32
	closing                 bool
	closed                  bool
	shutdown                *sessionShutdownAttempt
	retryPolicy             RetryPolicy
	retryEnabled            bool
	resolveRuntimeSettings  func() RuntimeControlSettings
	contextWindow           uint64
	contextReserve          uint64
	keepRecentTokens        uint64
	keepRecentSet           bool
	compactionEnabled       bool
	summarizer              session.Summarizer
	resolveSummarizer       func(context.Context, SummarizerResolveRequest) (session.Summarizer, error)
	branchSummarizer        session.BranchSummarizer
	resolveBranchSummarizer func(context.Context, SummarizerResolveRequest) (session.BranchSummarizer, error)
	branchSummaryReserve    uint64
	settlementTimeout       time.Duration
	observers               []sessionObserverEntry
	nextObserver            uint64
	loopUnsubscribe         func()
}

type sessionRun struct {
	ctx                          context.Context
	cancel                       context.CancelCauseFunc
	done                         chan struct{}
	phase                        Phase
	acceptingQueues              bool
	retryAttempt                 uint32
	retrySeries                  bool
	retryDelay                   time.Duration
	retryError                   string
	retryMax                     uint32
	retryCancel                  context.CancelCauseFunc
	compaction                   *runCancellation
	overflowCompacted            bool
	thresholdCompactionAttempted bool
	assistantStarted             bool
	assistantHookStarted         bool
	committed                    []llm.ConversationMessage
	committedAgent               []agentmsg.Message
	toolResults                  []llm.ConversationMessage
	terminalModel                provider.Model
	started                      bool
	extensionSystemPrompt        *string
	agentRunActive               bool
	agentPipeline                bool
	branchSummary                bool
	branchCancellation           *runCancellation
	finishOnce                   sync.Once
}

type runCancellation struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
}

type sessionShutdownAttempt struct {
	done chan struct{}
	err  error
}
type sessionObserverEntry struct {
	id       uint64
	observer SessionObserver
}

func sameMessages(left, right []llm.ConversationMessage) bool {
	return reflect.DeepEqual(left, right)
}

// NewSession constructs the long-lived product layer and its in-memory Agent. It
// deliberately does not recreate the loop on configuration changes: doing so
// would lose cancellation, queue and event ordering guarantees mid-run.
func NewSession(config SessionConfig) (*AgentSession, error) {
	if isNilInterface(config.Provider) {
		return nil, fmt.Errorf("%w: provider is required", ErrInvalidConfig)
	}
	if config.SessionManager == nil {
		return nil, fmt.Errorf("%w: session manager is required", ErrInvalidConfig)
	}
	if config.ContextReserve > config.ContextWindow && config.ContextWindow != 0 {
		return nil, fmt.Errorf("%w: context reserve exceeds window", ErrInvalidConfig)
	}
	if config.ContextWindow != 0 && config.Summarizer == nil && config.ResolveSummarizer == nil {
		return nil, fmt.Errorf("%w: automatic compaction requires summarizer", ErrInvalidConfig)
	}
	if !utf8.ValidString(config.BashCommandPrefix) || strings.IndexByte(config.BashCommandPrefix, 0) >= 0 {
		return nil, fmt.Errorf("%w: bash command prefix is invalid", ErrInvalidConfig)
	}
	activeTools, toolRegistry, toolOrder, err := buildToolCatalog(config.Tools, config.AllTools, config.ActiveToolNames)
	if err != nil {
		return nil, err
	}
	config.Tools = activeTools
	if len(config.Tools) != 0 && isNilInterface(config.Tool) {
		return nil, fmt.Errorf("%w: advertised tools require a non-nil executor", ErrInvalidConfig)
	}
	if config.SettlementTimeout < 0 {
		return nil, fmt.Errorf("%w: settlement timeout cannot be negative", ErrInvalidConfig)
	}
	initialContext := config.SessionManager.BuildContext()
	if config.ThinkingLevel == "" {
		if stored, ok := initialContext.ThinkingLevel(); ok && provider.ThinkingLevel(stored).Valid() {
			config.ThinkingLevel = provider.ThinkingLevel(stored)
		} else {
			config.ThinkingLevel = provider.ThinkingOff
		}
	}
	if !config.ThinkingLevel.Valid() {
		return nil, fmt.Errorf("%w: invalid thinking level %q", ErrInvalidConfig, config.ThinkingLevel)
	}
	if config.DefaultThinkingLevel != "" && !config.DefaultThinkingLevel.Valid() {
		return nil, fmt.Errorf("%w: invalid default thinking level %q", ErrInvalidConfig, config.DefaultThinkingLevel)
	}
	hasModel := modelPresent(config.Model)
	if hasModel {
		config.ThinkingLevel = config.Model.ClampThinkingLevel(config.ThinkingLevel)
	} else {
		config.ThinkingLevel = provider.ThinkingOff
	}
	if _, err := provider.NewRetryController(config.Retry); err != nil {
		return nil, fmt.Errorf("%w: retry policy: %w", ErrInvalidConfig, err)
	}
	if config.ContextReserve == 0 && !config.ContextReserveSet {
		config.ContextReserve = DefaultCompactionReserveTokens
	}
	if config.KeepRecentTokens == 0 && !config.KeepRecentTokensSet {
		config.KeepRecentTokens = DefaultCompactionKeepRecentTokens
	}
	if config.BranchSummaryReserveTokens == 0 && !config.BranchSummaryReserveSet {
		config.BranchSummaryReserveTokens = session.BranchSummaryDefaultReserveTokens
	}
	if err := validateExtensionCommands(config.Hooks.Commands); err != nil {
		return nil, err
	}
	compactionEnabled := true
	if config.CompactionEnabled != nil {
		compactionEnabled = *config.CompactionEnabled
	}
	retryEnabled := true
	if config.AutoRetryEnabled != nil {
		retryEnabled = *config.AutoRetryEnabled
	}
	systemOptions := cloneBuildSystemPromptOptions(config.SystemPromptOptions)
	if systemOptions.CWD == "" {
		systemOptions.CWD = config.SessionManager.Cwd()
	}
	if systemOptions.SelectedTools == nil {
		systemOptions.SelectedTools = make([]string, len(config.Tools))
		for index, definition := range config.Tools {
			systemOptions.SelectedTools[index] = definition.Name()
		}
	}
	if config.Resources != nil {
		prompt, options, resourceErr := config.Resources.BuildSystemPrompt(toolDefinitionNames(config.Tools))
		if resourceErr != nil {
			return nil, fmt.Errorf("%w: build system prompt from resources: %w", ErrInvalidConfig, resourceErr)
		}
		config.SystemPrompt = prompt
		systemOptions = cloneBuildSystemPromptOptions(options)
	}
	hooks := config.Hooks
	hooks.InputHandlers = append([]InputHook(nil), config.Hooks.InputHandlers...)
	hooks.Commands = append([]ExtensionCommand(nil), config.Hooks.Commands...)
	hooks.MessageHandlers = append([]MessageHook(nil), config.Hooks.MessageHandlers...)
	hooks.ToolResultHandlers = append([]AfterToolCallHook(nil), config.Hooks.ToolResultHandlers...)
	config.Hooks = hooks
	s := &AgentSession{
		sessionManager: config.SessionManager, systemOptions: systemOptions,
		resources: config.Resources, reloadRuntime: config.ReloadRuntime, reloadTools: config.ReloadTools,
		standaloneBash: config.StandaloneBash, resolveStandaloneBash: config.ResolveStandaloneBash,
		bashCommandPrefix: config.BashCommandPrefix, resolveBashPrefix: config.ResolveBashCommandPrefix,
		toolExecutor: config.Tool, toolRegistry: toolRegistry, toolOrder: toolOrder,
		toolMetadata:   cloneToolMetadataForRegistry(config.ToolMetadata, toolRegistry),
		beforeToolCall: composeBeforeToolHooks(config.BeforeToolCall, config.Hooks.ToolCall),
		stream:         provider.CloneStreamOptions(config.Stream),
		resolveStream:  config.ResolveStreamOptions, validateAccess: config.ValidateModelAccess, validateSelect: config.ValidateModelSelection,
		allModels: cloneModels(config.AllModels), scopedModels: cloneScopedModels(config.ScopedModels), modelAvailable: config.ModelAvailable, resolveAvailableModels: config.ResolveAvailableModels,
		defaultThinking: config.DefaultThinkingLevel, resolveDefaultThinking: config.ResolveDefaultThinkingLevel, persistSettings: config.PersistSettings,
		hooks: config.Hooks, noModelMessage: config.NoModelSelectedMessage,
		retryPolicy: config.Retry, retryEnabled: retryEnabled, resolveRuntimeSettings: config.ResolveRuntimeSettings,
		contextWindow: config.ContextWindow, contextReserve: config.ContextReserve, keepRecentTokens: config.KeepRecentTokens,
		keepRecentSet: config.KeepRecentTokensSet, compactionEnabled: compactionEnabled,
		summarizer: config.Summarizer, resolveSummarizer: config.ResolveSummarizer,
		branchSummarizer: config.BranchSummarizer, resolveBranchSummarizer: config.ResolveBranchSummarizer,
		branchSummaryReserve: config.BranchSummaryReserveTokens,
	}
	s.afterToolCall = composeAfterToolHooks(config.AfterToolCall, s.toolResultTransform())
	s.appendModelControl = config.SessionManager.AppendModelControlChange
	s.appendThinkingControl = config.SessionManager.AppendThinkingLevelChange
	if s.defaultThinking == "" || !s.defaultThinking.Valid() {
		s.defaultThinking = provider.ThinkingMedium
	}
	s.settlementTimeout = config.SettlementTimeout
	if s.settlementTimeout <= 0 {
		s.settlementTimeout = defaultSettlementTimeout
	}
	loop, err := New(Config{
		Provider: config.Provider, InitialMessages: initialContext.AgentMessages(), Model: config.Model, ThinkingLevel: config.ThinkingLevel, Stream: config.Stream,
		SystemPrompt: config.SystemPrompt, Tool: s.toolsWithSessionContext(config.Tool), Tools: config.Tools, BeforeToolCall: s.beforeToolCall, AfterToolCall: s.afterToolCall,
		ToolExecution: config.ToolExecution, TransformContext: config.TransformContext, TransformAgentContext: contextHookTransform(config.Hooks.Context),
		ConvertToLLM: config.ConvertToLLM, GetAPIKey: config.GetAPIKey,
		MessageEnd:   s.messageEndTransform,
		SteeringMode: config.SteeringMode, FollowUpMode: config.FollowUpMode,
		Now:         config.Now,
		PrepareTurn: s.prepareTurn,
	})
	if err != nil {
		return nil, err
	}
	s.loop = loop
	unsubscribeEvents := loop.Subscribe(func(ctx context.Context, event AgentEvent) error {
		return s.handleLoopEvent(ctx, event)
	})
	unsubscribeControl := loop.SubscribeControl(s.handleLoopControlEvent)
	s.loopUnsubscribe = func() {
		unsubscribeEvents()
		unsubscribeControl()
	}
	if config.InitializeSessionState {
		hasExistingSession := len(initialContext.AgentMessages()) > 0
		hasThinkingEntry, err := sessionBranchHasThinkingEntry(config.SessionManager)
		if err != nil {
			s.loopUnsubscribe()
			return nil, fmt.Errorf("%w: inspect thinking level state: %w", ErrTranscriptCommit, err)
		}
		settlement, cancel := context.WithTimeout(context.Background(), s.settlementTimeout)
		defer cancel()
		if !hasExistingSession && hasModel {
			if _, err := config.SessionManager.AppendModelChange(settlement, config.Model.Provider(), config.Model.ID()); err != nil {
				s.loopUnsubscribe()
				return nil, fmt.Errorf("%w: initial model change: %w", ErrTranscriptCommit, err)
			}
		}
		if !hasExistingSession || !hasThinkingEntry {
			if _, err := config.SessionManager.AppendThinkingLevelChange(settlement, string(config.ThinkingLevel)); err != nil {
				s.loopUnsubscribe()
				return nil, fmt.Errorf("%w: initial thinking level change: %w", ErrTranscriptCommit, err)
			}
		}
	}
	if s.hooks.SessionStart != nil {
		startEvent := SessionStartHookEvent{Reason: SessionStartup}
		if config.SessionStartEvent != nil {
			startEvent = cloneSessionStartHookEvent(*config.SessionStartEvent)
		}
		if hookErr := callSessionStartHook(s.hooks.SessionStart, context.Background(), startEvent); hookErr != nil {
			// Lifecycle extension handlers are observational. Match ExtensionRunner:
			// report a handler failure, but retain the newly constructed durable
			// session and allow the agent to run.
			s.reportExtensionError(context.Background(), "session_start", 0, hookErr)
		}
	}
	return s, nil
}

func sessionBranchHasThinkingEntry(manager *session.SessionManager) (bool, error) {
	entries, err := manager.BranchPath("")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if _, ok := entry.Payload().(session.ThinkingLevelChangePayload); ok {
			return true, nil
		}
	}
	return false, nil
}

type noModelSelectedGuidanceError struct{ message string }

func (e noModelSelectedGuidanceError) Error() string { return e.message }
func (noModelSelectedGuidanceError) Unwrap() error   { return ErrNoModelSelected }

func (s *AgentSession) noModelSelectedError() error {
	if s == nil || s.noModelMessage == "" || s.noModelMessage == ErrNoModelSelected.Error() {
		return ErrNoModelSelected
	}
	return noModelSelectedGuidanceError{message: s.noModelMessage}
}

// requireModelAccess snapshots the selected model after top-level session-run
// admission. Product factories pair this check with ValidateModelSelection so
// replacements are validated before publication; every provider turn still
// resolves its current credentials independently in prepareTurn.
func (s *AgentSession) requireModelAccess(ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	selected, hasModel, _ := s.selectionSnapshot()
	s.mu.RLock()
	validate := s.validateAccess
	s.mu.RUnlock()
	if !hasModel {
		return s.noModelSelectedError()
	}
	if validate != nil {
		if err := validate(ctx, selected); err != nil {
			return err
		}
	}
	return nil
}

func (s *AgentSession) handleLoopEvent(ctx context.Context, event AgentEvent) error {
	if runtimeEvent, ok := event.(agentRuntimeEvent); ok {
		s.handleLoopRuntimeEvent(ctx, runtimeEvent)
	}
	if ended, ok := event.(MessageEndEvent); ok {
		if err := s.persistAgentMessage(ctx, ended.Message); err != nil {
			return err
		}
		if standard, ok := ended.Message.(agentmsg.LLM); ok && retrySucceededMessage(standard.Conversation()) {
			s.endRetrySeries(ctx, true, "")
		}
	}
	return nil
}

func (s *AgentSession) persistAgentMessage(ctx context.Context, message agentmsg.Message) error {
	if s == nil || s.sessionManager == nil || message == nil {
		return fmt.Errorf("%w: invalid session message", ErrTranscriptCommit)
	}
	settlement, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.settlementTimeout)
	defer cancel()
	if standard, ok := message.(agentmsg.LLM); ok {
		_, err := s.sessionManager.AppendLLMMessage(settlement, standard.Conversation())
		if err != nil {
			return fmt.Errorf("%w: %s message: %w", ErrTranscriptCommit, message.Role(), err)
		}
		return nil
	}
	if _, err := s.sessionManager.AppendMessage(settlement, message); err != nil {
		return fmt.Errorf("%w: %s message: %w", ErrTranscriptCommit, message.Role(), err)
	}
	return nil
}

func (s *AgentSession) handleLoopControlEvent(ctx context.Context, event AgentControlEvent) {
	if runtimeEvent, ok := event.(agentRuntimeEvent); ok {
		s.handleLoopRuntimeEvent(ctx, runtimeEvent)
	}
}

func (s *AgentSession) handleLoopRuntimeEvent(ctx context.Context, event agentRuntimeEvent) {
	if s == nil {
		return
	}
	s.mu.RLock()
	observers := make([]SessionObserver, 0, len(s.observers))
	for _, entry := range s.observers {
		if entry.observer != nil {
			observers = append(observers, entry.observer)
		}
	}
	s.mu.RUnlock()
	types := []string{}
	var message llm.ConversationMessage
	var agentMessage agentmsg.Message
	var terminal llm.AssistantTerminal
	var agentMessages []agentmsg.Message
	var willRetry bool
	var skipAssistantStartHook bool
	var queueUpdate SessionQueueUpdateEvent
	var hasQueueUpdate bool
	switch value := event.(type) {
	case AgentStartEvent:
		s.lifecycleMu.Lock()
		if s.run == nil {
			s.lifecycleMu.Unlock()
			// Manual compact remains a standalone low-level lifecycle.
			types = []string{"agent_start"}
			break
		}
		s.run.started = true
		s.run.assistantStarted = false
		s.run.assistantHookStarted = false
		s.run.committed = nil
		s.run.committedAgent = nil
		s.run.toolResults = nil
		s.lifecycleMu.Unlock()
		// Every low-level continuation is a new upstream agent loop and therefore
		// emits its own agent_start. AgentSession adds agent_settled only once after
		// the complete retry/compaction/queue series becomes idle.
		types = []string{"agent_start"}
	case TurnStartEvent:
		s.resetSessionTurn(event)
		types = []string{"turn_start"}
	case MessageStartEvent:
		agentMessage = agentmsg.CloneOne(value.Message)
		if agentMessage != nil && agentMessage.Role() == agentmsg.RoleAssistant {
			if s.beginAssistantMessage() {
				types = []string{"message_start"}
				skipAssistantStartHook = !s.beginAssistantHookMessage()
			}
		} else {
			types = []string{"message_start"}
		}
	case MessageUpdateEvent:
		agentMessage = agentmsg.CloneOne(value.Message)
		if s.beginAssistantMessage() {
			types = append(types, "message_start")
			s.beginAssistantHookMessage()
		}
		types = append(types, "message_update")
	case MessageEndEvent:
		agentMessage = agentmsg.CloneOne(value.Message)
		if standard, ok := value.Message.(agentmsg.LLM); ok {
			message = standard.Conversation()
		}
		s.recordCommitted(message, agentMessage, value.Model)
		types = []string{"message_end"}
	case ToolExecutionStartEvent:
		types = []string{"tool_execution_start"}
	case ToolExecutionUpdateEvent:
		types = []string{"tool_execution_update"}
	case ToolExecutionEndEvent:
		types = []string{"tool_execution_end"}
	case TurnEndEvent:
		if standard, ok := value.Message.(agentmsg.LLM); ok {
			terminal, _ = standard.Conversation().(llm.AssistantTerminal)
		}
		types = []string{"turn_end"}
	case AgentEndEvent:
		types = []string{"agent_end"}
		terminal = value.Terminal
		agentMessages = s.sessionCommittedAgentMessages()
		willRetry = s.willRetry(value.Terminal)
	case QueueUpdateEvent:
		types = []string{"queue_update"}
		queueUpdate = sessionQueueUpdateEventFromAgent(value)
		hasQueueUpdate = true
	}
	for _, kind := range types {
		hookMessage := agentMessage
		if kind == "message_start" && skipAssistantStartHook {
			hookMessage = nil
		}
		s.dispatchExtensionHook(ctx, kind, message, hookMessage, terminal, event)
		var emitted SessionEvent
		switch kind {
		case "agent_start":
			emitted = AgentStartEvent{RunID: agentEventRunID(event)}
		case "turn_start":
			emitted = TurnStartEvent{RunID: agentEventRunID(event), Turn: agentEventTurn(event)}
		case "message_start":
			emitted = MessageStartEvent{
				RunID: agentEventRunID(event), Turn: agentEventTurn(event),
				Message: agentmsg.CloneOne(agentMessage),
			}
		case "message_update":
			if value, ok := event.(MessageUpdateEvent); ok {
				emitted = value
			}
		case "message_end":
			if value, ok := event.(MessageEndEvent); ok {
				emitted = value
			}
		case "tool_execution_start":
			emitted, _ = event.(ToolExecutionStartEvent)
		case "tool_execution_update":
			emitted, _ = event.(ToolExecutionUpdateEvent)
		case "tool_execution_end":
			emitted, _ = event.(ToolExecutionEndEvent)
		case "turn_end":
			if value, ok := event.(TurnEndEvent); ok {
				emitted = value
			}
		case "agent_end":
			emitted = SessionAgentEndEvent{Messages: agentmsg.Clone(agentMessages), Terminal: terminal, WillRetry: willRetry}
		case "queue_update":
			if hasQueueUpdate {
				emitted = queueUpdate
			} else {
				emitted = s.sessionQueueUpdateEvent()
			}
		}
		if emitted != nil {
			s.emitToObservers(ctx, observers, emitted)
		}
	}
	// A successful retry ends only in handleLoopEvent after this pre-persist
	// extension/observer dispatch and the durable append both succeed. A final
	// failed low run ends here after its agent_end.
	if ended, ok := event.(AgentEndEvent); ok && !willRetry {
		s.endRetrySeries(ctx, false, retryFinalError(ended))
	}
}

// dispatchExtensionHook is observational for lifecycle events. For
// message_end it runs before SessionManager persistence, matching the original
// AgentSession event pipeline.
// Mutation/cancellation hooks run at their safe pre-boundaries (context and
// compaction); an error here cannot retroactively alter durable history.
func (s *AgentSession) dispatchExtensionHook(ctx context.Context, kind string, message llm.ConversationMessage, agentMessage agentmsg.Message, terminal llm.AssistantTerminal, event agentRuntimeEvent) {
	if s == nil {
		return
	}
	if s.hooks.Agent != nil {
		var lifecycle AgentLifecycleType
		switch kind {
		case "agent_start":
			lifecycle = AgentStartHookEvent
		case "agent_end":
			lifecycle = AgentEndHookEvent
		}
		if lifecycle != "" {
			_ = s.hooks.Agent(ctx, AgentLifecycleEvent{Type: lifecycle, Messages: s.sessionCommittedAgentMessages(), Terminal: terminal})
		}
	}
	if len(s.messageHooks()) != 0 && agentMessage != nil {
		var messageType MessageHookType
		var providerEvent llm.StreamEvent
		switch kind {
		case "message_start":
			messageType = MessageStartHookEvent
			if partial, ok := agentMessage.(agentmsg.AssistantPartial); ok {
				providerEvent = partial.ProviderEvent()
			}
		case "message_update":
			messageType = MessageUpdateHookEvent
			if update, ok := event.(MessageUpdateEvent); ok {
				providerEvent = update.AssistantMessageEvent.Event()
			}
		}
		if messageType != "" {
			for index, hook := range s.messageHooks() {
				_, err := callMessageHook(hook, ctx, MessageHookEvent{Type: messageType, Message: agentmsg.CloneOne(agentMessage), ProviderEvent: providerEvent})
				if err != nil {
					s.reportExtensionError(ctx, string(messageType), index, err)
				}
			}
		}
	}
	if s.hooks.Turn != nil {
		turnIndex := uint32(0)
		if turn := agentEventTurn(event); turn > 0 {
			turnIndex = turn - 1
		}
		switch kind {
		case "turn_start":
			// coding-agent adds the timestamp while adapting the low-level
			// turn_start event to its extension surface. Reuse the configured
			// Agent clock so tests and embedding hosts retain deterministic time
			// semantics; a clock failure leaves the observational field unset.
			timestamp, _ := s.loop.now()
			_ = s.hooks.Turn(ctx, TurnLifecycleEvent{
				Type: TurnStartHookEvent, TurnIndex: turnIndex, Timestamp: timestamp,
			})
		case "turn_end":
			var final agentmsg.Message
			if terminal != nil {
				final, _ = agentmsg.NewLLM(terminal)
			}
			results := make([]agentmsg.Message, 0, len(s.sessionTurnResults()))
			for _, result := range s.sessionTurnResults() {
				wrapped, err := agentmsg.NewLLM(result)
				if err == nil {
					results = append(results, wrapped)
				}
			}
			_ = s.hooks.Turn(ctx, TurnLifecycleEvent{
				Type: TurnEndHookEvent, TurnIndex: turnIndex,
				Message: agentmsg.CloneOne(final), ToolResults: agentmsg.Clone(results),
			})
		}
	}
	if s.hooks.ToolExecution != nil {
		toolEvent := ToolExecutionLifecycleEvent{}
		switch value := event.(type) {
		case ToolExecutionStartEvent:
			toolEvent.Type = ToolExecutionStartHookEvent
			toolEvent.ToolCallID, toolEvent.ToolName = value.ToolCallID, value.ToolName
			toolEvent.Arguments = bytes.Clone(value.Arguments)
		case ToolExecutionUpdateEvent:
			toolEvent.Type = ToolExecutionUpdateHookEvent
			toolEvent.ToolCallID, toolEvent.ToolName = value.ToolCallID, value.ToolName
			toolEvent.Arguments = bytes.Clone(value.Arguments)
			update := cloneToolUpdate(value.PartialResult)
			toolEvent.Update = &update
		case ToolExecutionEndEvent:
			toolEvent.Type = ToolExecutionEndHookEvent
			toolEvent.ToolCallID, toolEvent.ToolName = value.ToolCallID, value.ToolName
			toolEvent.Arguments = bytes.Clone(value.Arguments)
			toolEvent.IsError = value.IsError
			result := cloneToolOutput(value.Result)
			toolEvent.Result = &result
		}
		if toolEvent.Type != "" {
			_ = s.hooks.ToolExecution(ctx, toolEvent)
		}
	}
}

func cloneToolUpdate(value ToolUpdate) ToolUpdate {
	value.Content = append([]llm.ToolResultContentBlock(nil), value.Content...)
	value.Details, _ = cloneToolDetails(value.Details)
	value.AddedToolNames = append([]string(nil), value.AddedToolNames...)
	if value.Usage != nil {
		usage := *value.Usage
		value.Usage = &usage
	}
	return value
}

func cloneToolOutput(value ToolOutput) ToolOutput {
	if value.Content != nil {
		content := make([]llm.ToolResultContentBlock, len(value.Content))
		copy(content, value.Content)
		value.Content = content
	}
	value.Details, _ = cloneToolDetails(value.Details)
	value.AddedToolNames = append([]string(nil), value.AddedToolNames...)
	if value.Usage != nil {
		usage := *value.Usage
		value.Usage = &usage
	}
	return value
}

// cloneToolDetails validates before reflective cloning. Besides enforcing the
// documented JSON-like boundary, this prevents a cyclic extension value from
// reaching CloneJSONValue's recursive walk.
func cloneToolDetails(value any) (any, bool) {
	if value == nil {
		return nil, true
	}
	if _, err := json.Marshal(value); err != nil {
		return nil, false
	}
	return provider.CloneJSONValue(value), true
}

// ownToolOutput snapshots every valid caller-owned field as soon as Execute
// returns. An invalid Details value is retained only so the durable boundary
// can report the existing ErrInvariant; observer/hook clones omit it safely.
func ownToolOutput(value ToolOutput) ToolOutput {
	originalDetails := value.Details
	owned := cloneToolOutput(value)
	if originalDetails != nil && owned.Details == nil {
		owned.Details = originalDetails
	}
	return owned
}

func retrySucceededMessage(message llm.ConversationMessage) bool {
	terminal, ok := message.(llm.AssistantTerminal)
	if !ok {
		return false
	}
	return terminal.FinishReason() != llm.FinishError && terminal.FinishReason() != llm.FinishAborted
}

func retryFinalError(event AgentEndEvent) string {
	if event.Err != nil {
		return event.Err.Error()
	}
	return retryTerminalErrorMessage(event.Terminal)
}

func (s *AgentSession) resetSessionTurn(_ agentRuntimeEvent) {
	s.lifecycleMu.Lock()
	if s.run != nil {
		s.run.phase = PhaseProvider
		s.run.assistantStarted = false
		s.run.assistantHookStarted = false
		s.run.toolResults = nil
	}
	s.lifecycleMu.Unlock()
}

func (s *AgentSession) beginAssistantMessage() bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.run == nil || s.run.assistantStarted {
		return false
	}
	s.run.assistantStarted = true
	return true
}

func (s *AgentSession) beginAssistantHookMessage() bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.run == nil || s.run.assistantHookStarted {
		return false
	}
	s.run.assistantHookStarted = true
	return true
}

func (s *AgentSession) recordCommitted(message llm.ConversationMessage, agentMessage agentmsg.Message, model provider.Model) {
	if message == nil && agentMessage == nil {
		return
	}
	s.lifecycleMu.Lock()
	if s.run != nil {
		if agentMessage != nil {
			s.run.committedAgent = append(s.run.committedAgent, agentmsg.CloneOne(agentMessage))
		}
		if message != nil {
			s.run.committed = append(s.run.committed, message)
		}
		if message != nil && message.Role() == llm.RoleToolResult {
			s.run.toolResults = append(s.run.toolResults, message)
		}
		if message != nil && message.Role() == llm.RoleAssistant && model.ID() != "" {
			s.run.terminalModel = model
		}
	}
	s.lifecycleMu.Unlock()
}

func (s *AgentSession) sessionCommittedAgentMessages() []agentmsg.Message {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.run == nil {
		return nil
	}
	return agentmsg.Clone(s.run.committedAgent)
}

func (s *AgentSession) sessionTurnResults() []llm.ConversationMessage {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.run == nil {
		return nil
	}
	return append([]llm.ConversationMessage(nil), s.run.toolResults...)
}

func (s *AgentSession) sessionCommittedMessages() []llm.ConversationMessage {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.run == nil {
		return nil
	}
	return append([]llm.ConversationMessage(nil), s.run.committed...)
}

func (s *AgentSession) prepareTurn(ctx context.Context, turn TurnContext) (TurnSnapshot, error) {
	if s == nil {
		return TurnSnapshot{}, errors.New("nil agent session")
	}
	if err := s.rejectIfClosed(); err != nil {
		return TurnSnapshot{}, err
	}
	compacted := turn.Turn > 1 && s.compactBeforeNextAssistantResponse(ctx, turn)
	s.selectionMu.RLock()
	state, executor := s.loop.runtimeSnapshot()
	if !state.HasModel() {
		s.selectionMu.RUnlock()
		return TurnSnapshot{}, ErrNoModelSelected
	}
	s.mu.RLock()
	snapshot := TurnSnapshot{
		Model: state.Model(), ThinkingLevel: state.ThinkingLevel(), SystemPrompt: state.SystemPrompt(),
		Tool: executor, Tools: state.Tools(), Stream: provider.CloneStreamOptions(s.stream),
	}
	resolver := s.resolveStream
	s.mu.RUnlock()
	s.selectionMu.RUnlock()
	if compacted {
		snapshot.Messages = state.Messages()
	}
	s.lifecycleMu.Lock()
	if s.run != nil && s.run.extensionSystemPrompt != nil {
		snapshot.SystemPrompt = *s.run.extensionSystemPrompt
	}
	s.lifecycleMu.Unlock()
	if resolver != nil {
		resolved, err := resolver(ctx, snapshot.Model)
		if err != nil {
			return TurnSnapshot{}, fmt.Errorf("%w: resolve stream options: %w", ErrInvalidConfig, err)
		}
		// A resolver refreshes auth and other turn-scoped values. It cannot
		// replace the caller's complete stream contract: callbacks, thinking
		// budgets, attribution headers, metadata, and transport settings remain
		// present unless an explicit overlay field replaces that value.
		snapshot.Stream = provider.MergeStreamOptions(snapshot.Stream, resolved)
	}
	snapshot.Stream = provider.MergeStreamOptions(snapshot.Stream, provider.StreamOptions{
		OnPayload:  s.hooks.BeforeProviderRequest,
		OnHeaders:  s.hooks.BeforeProviderHeaders,
		OnResponse: s.hooks.AfterProviderResponse,
	})
	return snapshot, nil
}

func (s *AgentSession) State() SessionState {
	if s == nil {
		return SessionState{Active: State{phase: PhaseIdle}}
	}
	// Hold the documented lifecycle -> selection -> Agent lock order so the
	// returned active/idle projection cannot straddle session-run detachment.
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	run := s.run
	phase := PhaseIdle
	if run != nil {
		phase = run.phase
	}
	s.selectionMu.RLock()
	active := State{phase: PhaseIdle}
	if s.loop != nil {
		active = s.loop.State()
	}
	state := SessionState{
		Model: active.Model(), HasModel: active.HasModel(), ThinkingLevel: active.ThinkingLevel(),
		SystemPrompt: active.SystemPrompt(), Tools: active.Tools(), Active: active,
	}
	s.selectionMu.RUnlock()
	if run != nil {
		if active.Phase() == PhaseIdle {
			active.phase = phase
		}
		state.Active = active
	} else {
		state.Active.phase = PhaseIdle
	}
	return state
}

// Activity returns one lifecycleMu-consistent view of the session operation.
// AgentSession remains the sole owner of these fields; an application surface must sample this
// view instead of reconstructing streaming/compaction state from events.
func (s *AgentSession) Activity() SessionActivity {
	if s == nil {
		return SessionActivity{Phase: PhaseIdle}
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.run == nil {
		return SessionActivity{Phase: PhaseIdle}
	}
	return SessionActivity{
		Phase:        s.run.phase,
		IsStreaming:  s.run.agentRunActive,
		IsCompacting: s.run.compaction != nil || s.run.branchCancellation != nil,
		RetryAttempt: s.run.retryAttempt,
		RetryWaiting: s.run.retryCancel != nil,
	}
}

func (s *AgentSession) rejectIfClosed() error {
	if s == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	s.lifecycleMu.Lock()
	closed := s.closed || s.closing
	s.lifecycleMu.Unlock()
	if closed {
		return fmt.Errorf("%w: session is closed", ErrInvalidRun)
	}
	return nil
}

type retryControl struct {
	attempt, max uint32
	delay        time.Duration
	succeeded    bool
	errorMessage string
	finalError   string
}

var errRetryCancelled = errors.New("retry cancelled")

func (s *AgentSession) emitControl(ctx context.Context, kind string, retry ...retryControl) {
	if s == nil {
		return
	}
	s.mu.RLock()
	observers := make([]SessionObserver, 0, len(s.observers))
	for _, entry := range s.observers {
		if entry.observer != nil {
			observers = append(observers, entry.observer)
		}
	}
	s.mu.RUnlock()
	var retryEvent retryControl
	if len(retry) != 0 {
		retryEvent = retry[0]
	}
	var event SessionEvent
	switch kind {
	case "queue_update":
		queue := s.sessionQueueUpdateEvent()
		event = queue
	case "auto_retry_start":
		event = AutoRetryStartEvent{
			Attempt: retryEvent.attempt, MaxAttempts: retryEvent.max,
			Delay: retryEvent.delay, ErrorMessage: retryEvent.errorMessage,
		}
	case "auto_retry_end":
		event = AutoRetryEndEvent{Success: retryEvent.succeeded, Attempt: retryEvent.attempt, FinalError: retryEvent.finalError}
	}
	if event != nil {
		s.emitToObservers(ctx, observers, event)
	}
}

func (s *AgentSession) emitQueueUpdate(ctx context.Context, event SessionQueueUpdateEvent) {
	if s == nil {
		return
	}
	s.mu.RLock()
	observers := make([]SessionObserver, 0, len(s.observers))
	for _, entry := range s.observers {
		if entry.observer != nil {
			observers = append(observers, entry.observer)
		}
	}
	s.mu.RUnlock()
	s.emitToObservers(ctx, observers, event)
}

func (s *AgentSession) emitAgentSettled(ctx context.Context, messages []agentmsg.Message) {
	if s == nil {
		return
	}
	s.mu.RLock()
	observers := make([]SessionObserver, 0, len(s.observers))
	for _, entry := range s.observers {
		if entry.observer != nil {
			observers = append(observers, entry.observer)
		}
	}
	s.mu.RUnlock()
	if s.hooks.Agent != nil {
		_ = s.hooks.Agent(ctx, AgentLifecycleEvent{Type: AgentSettledHookEvent, Messages: agentmsg.Clone(messages)})
	}
	s.emitToObservers(ctx, observers, AgentSettledEvent{})
}

func (s *AgentSession) emitThinkingLevelChanged(ctx context.Context, level provider.ThinkingLevel) {
	if s == nil {
		return
	}
	s.mu.RLock()
	observers := make([]SessionObserver, 0, len(s.observers))
	for _, entry := range s.observers {
		if entry.observer != nil {
			observers = append(observers, entry.observer)
		}
	}
	s.mu.RUnlock()
	s.emitToObservers(ctx, observers, ThinkingLevelChangedEvent{Level: level})
}

func (s *AgentSession) Model() provider.Model {
	model, _, _ := s.selectionSnapshot()
	return model
}
func (s *AgentSession) HasModel() bool {
	_, hasModel, _ := s.selectionSnapshot()
	return hasModel
}
func (s *AgentSession) SelectedModel() (provider.Model, bool) {
	model, hasModel, _ := s.selectionSnapshot()
	return model, hasModel
}
func (s *AgentSession) ThinkingLevel() provider.ThinkingLevel {
	_, _, thinking := s.selectionSnapshot()
	return thinking
}
func (s *AgentSession) SystemPrompt() string {
	if s == nil || s.loop == nil {
		return ""
	}
	return s.loop.State().SystemPrompt()
}
func (s *AgentSession) Tools() []provider.ToolDefinition {
	if s == nil || s.loop == nil {
		return nil
	}
	return s.loop.State().Tools()
}

func (s *AgentSession) selectionSnapshot() (provider.Model, bool, provider.ThinkingLevel) {
	if s == nil || s.loop == nil {
		return provider.Model{}, false, ""
	}
	s.selectionMu.RLock()
	state := s.loop.State()
	s.selectionMu.RUnlock()
	return state.Model(), state.HasModel(), state.ThinkingLevel()
}
func (s *AgentSession) SessionManager() *session.SessionManager {
	if s == nil {
		return nil
	}
	return s.sessionManager
}

func (s *AgentSession) SessionName() (string, bool) {
	if s == nil || s.sessionManager == nil {
		return "", false
	}
	return s.sessionManager.SessionName()
}

// AppendCustomEntry is the extension-neutral counterpart to pi.appendEntry.
// The durable custom entry commits before the product event is published.
func (s *AgentSession) AppendCustomEntry(ctx context.Context, customType string, data json.RawMessage) (session.Entry, error) {
	if s == nil || s.sessionManager == nil {
		return session.Entry{}, fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	if err := s.rejectIfClosed(); err != nil {
		return session.Entry{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	entry, err := s.sessionManager.AppendCustomEntry(ctx, customType, bytes.Clone(data))
	if err != nil {
		return session.Entry{}, fmt.Errorf("%w: custom entry: %w", ErrTranscriptCommit, err)
	}
	s.mu.RLock()
	observers := make([]SessionObserver, 0, len(s.observers))
	for _, observer := range s.observers {
		if observer.observer != nil {
			observers = append(observers, observer.observer)
		}
	}
	s.mu.RUnlock()
	s.emitToObservers(ctx, observers, EntryAppendedEvent{Entry: entry})
	return entry, nil
}

// SetSessionName persists the sanitized session_info entry before publishing
// the product event, matching the original AgentSession ordering.
func (s *AgentSession) SetSessionName(ctx context.Context, name string) error {
	if s == nil || s.sessionManager == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	if err := s.rejectIfClosed(); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_, err := s.sessionManager.AppendSessionInfo(ctx, name)
	if err != nil {
		return fmt.Errorf("%w: session info: %w", ErrTranscriptCommit, err)
	}
	resolved, ok := s.sessionManager.SessionName()
	var eventName *string
	if ok {
		eventName = &resolved
	}
	s.mu.RLock()
	observers := make([]SessionObserver, 0, len(s.observers))
	for _, observer := range s.observers {
		if observer.observer != nil {
			observers = append(observers, observer.observer)
		}
	}
	s.mu.RUnlock()
	s.emitToObservers(ctx, observers, SessionInfoChangeEvent{Name: eventName})
	if hook := s.hooks.SessionInfoChanged; hook != nil {
		_ = hook(ctx, SessionInfoChangedEvent{Name: eventName})
	}
	return nil
}
func (s *AgentSession) SetSystemPrompt(prompt string) error {
	if s == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	if !utf8.ValidString(prompt) {
		return fmt.Errorf("%w: system prompt is not valid UTF-8", ErrInvalidConfig)
	}
	s.lifecycleMu.Lock()
	if s.closed || s.closing {
		s.lifecycleMu.Unlock()
		return fmt.Errorf("%w: session is closed", ErrInvalidRun)
	}
	if err := s.loop.SetSystemPrompt(prompt); err != nil {
		s.lifecycleMu.Unlock()
		return err
	}
	s.lifecycleMu.Unlock()
	return nil
}

func (s *AgentSession) SetTools(executor ToolExecutor, tools []provider.ToolDefinition) error {
	if s == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	return s.setTools(executor, tools)
}

func (s *AgentSession) setTools(executor ToolExecutor, tools []provider.ToolDefinition) error {
	if s == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	if len(tools) != 0 && isNilInterface(executor) {
		return fmt.Errorf("%w: advertised tools require a non-nil executor", ErrInvalidConfig)
	}
	selected, registry, order, err := buildToolCatalog(tools, tools, nil)
	if err != nil {
		return err
	}
	return s.publishToolRuntime(executor, selected, registry, order, nil, nil, false)
}

func (s *AgentSession) replaceToolRuntime(runtime ToolRuntime, activeNames []string) error {
	if len(runtime.Tools) != 0 && isNilInterface(runtime.Executor) {
		return fmt.Errorf("%w: advertised tools require a non-nil executor", ErrInvalidConfig)
	}
	if runtime.StandaloneBash != nil && isNilInterface(runtime.StandaloneBash) {
		return fmt.Errorf("%w: standalone bash executor is a typed nil", ErrInvalidConfig)
	}
	selected, registry, order, err := buildToolCatalog(nil, runtime.Tools, activeNames)
	if err != nil {
		return err
	}
	return s.publishToolRuntime(runtime.Executor, selected, registry, order, runtime.Metadata, runtime.StandaloneBash, true)
}

func (s *AgentSession) publishToolRuntime(
	executor ToolExecutor,
	selected []provider.ToolDefinition,
	registry map[string]provider.ToolDefinition,
	order []string,
	metadata map[string]ToolMetadata,
	standaloneBash StandaloneBashExecutor,
	replaceStandalone bool,
) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closed || s.closing {
		return fmt.Errorf("%w: session is closed", ErrInvalidRun)
	}
	s.selectionMu.Lock()
	defer s.selectionMu.Unlock()
	state := s.loop.State()
	prompt := state.SystemPrompt()
	options := s.systemPromptOptions()
	options.SelectedTools = toolDefinitionNames(selected)
	s.mu.RLock()
	resources := s.resources
	s.mu.RUnlock()
	var err error
	if resources != nil {
		prompt, options, err = resources.BuildSystemPrompt(options.SelectedTools)
		if err != nil {
			return fmt.Errorf("%w: rebuild system prompt: %w", ErrInvalidConfig, err)
		}
	}
	if state.HasModel() {
		if _, err := provider.NewRequestWithOptions(state.Model(), prompt, nil, provider.RequestOptions{Tools: selected, ThinkingLevel: state.ThinkingLevel()}); err != nil {
			return fmt.Errorf("%w: tools: %w", ErrInvalidConfig, err)
		}
	}
	if err := s.loop.setPromptAndTools(prompt, s.toolsWithSessionContext(executor), selected); err != nil {
		return err
	}
	s.mu.Lock()
	s.systemOptions = cloneBuildSystemPromptOptions(options)
	s.toolExecutor = executor
	s.toolRegistry = registry
	s.toolOrder = order
	s.toolMetadata = cloneToolMetadataForRegistry(metadata, registry)
	if replaceStandalone {
		s.standaloneBash = standaloneBash
	}
	s.mu.Unlock()
	return nil
}

// AllTools returns the complete configured registry in stable registration
// order. It is independent from the provider-visible active set.
func (s *AgentSession) AllTools() []provider.ToolDefinition {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	tools := make([]provider.ToolDefinition, 0, len(s.toolOrder))
	for _, name := range s.toolOrder {
		if definition, exists := s.toolRegistry[name]; exists {
			tools = append(tools, definition)
		}
	}
	s.mu.RUnlock()
	return tools
}

// AllToolInfo retains schema, per-tool prompt guidelines, and provenance in
// stable registration order. AllTools remains the provider-only compatibility
// view used by existing low-level callers.
func (s *AgentSession) AllToolInfo() []ToolInfo {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	tools := make([]ToolInfo, 0, len(s.toolOrder))
	for _, name := range s.toolOrder {
		definition, exists := s.toolRegistry[name]
		if !exists {
			continue
		}
		metadata, hasMetadata := s.toolMetadata[name]
		if !hasMetadata {
			metadata.SourceInfo = SystemPromptSourceInfo{
				Path: "<sdk:" + name + ">", Source: "sdk",
				Scope: SystemPromptSourceTemporary, Origin: SystemPromptSourceTopLevel,
			}
		}
		tools = append(tools, ToolInfo{
			Definition: definition, PromptGuidelines: append([]string(nil), metadata.PromptGuidelines...),
			SourceInfo: cloneSystemPromptSourceInfo(metadata.SourceInfo),
		})
	}
	s.mu.RUnlock()
	return tools
}

func cloneToolMetadataForRegistry(values map[string]ToolMetadata, registry map[string]provider.ToolDefinition) map[string]ToolMetadata {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]ToolMetadata, min(len(values), len(registry)))
	for name, value := range values {
		if _, exists := registry[name]; !exists {
			continue
		}
		result[name] = ToolMetadata{
			PromptGuidelines: append([]string(nil), value.PromptGuidelines...),
			SourceInfo:       cloneSystemPromptSourceInfo(value.SourceInfo),
		}
	}
	return result
}

func cloneSystemPromptSourceInfo(value SystemPromptSourceInfo) SystemPromptSourceInfo {
	value.BaseDir = cloneStringPointer(value.BaseDir)
	return value
}

func (s *AgentSession) ActiveToolNames() []string {
	if s == nil || s.loop == nil {
		return nil
	}
	return toolDefinitionNames(s.loop.State().Tools())
}

// SetActiveToolsByName mirrors pi's registry-based tool switch: unknown names
// are ignored, duplicates are collapsed, and the effective system prompt and
// provider-visible definitions are published as one turn snapshot.
func (s *AgentSession) SetActiveToolsByName(names []string) error {
	if s == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	return s.setActiveToolsByName(names)
}

func (s *AgentSession) setActiveToolsByName(names []string) error {
	if s == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closed || s.closing {
		return fmt.Errorf("%w: session is closed", ErrInvalidRun)
	}
	s.selectionMu.Lock()
	defer s.selectionMu.Unlock()
	s.mu.RLock()
	selected := selectToolDefinitions(s.toolRegistry, names)
	resources := s.resources
	executor := s.toolExecutor
	s.mu.RUnlock()
	state := s.loop.State()
	prompt := state.SystemPrompt()
	options := s.systemPromptOptions()
	options.SelectedTools = toolDefinitionNames(selected)
	var err error
	if resources != nil {
		prompt, options, err = resources.BuildSystemPrompt(options.SelectedTools)
		if err != nil {
			return fmt.Errorf("%w: rebuild system prompt: %w", ErrInvalidConfig, err)
		}
	}
	if state.HasModel() {
		if _, err := provider.NewRequestWithOptions(state.Model(), prompt, nil, provider.RequestOptions{Tools: selected, ThinkingLevel: state.ThinkingLevel()}); err != nil {
			return fmt.Errorf("%w: tools: %w", ErrInvalidConfig, err)
		}
	}
	if err := s.loop.setPromptAndTools(prompt, s.toolsWithSessionContext(executor), selected); err != nil {
		return err
	}
	s.mu.Lock()
	s.systemOptions = cloneBuildSystemPromptOptions(options)
	s.mu.Unlock()
	return nil
}

func (s *AgentSession) expandPromptInput(prompt string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	s.mu.RLock()
	resources := s.resources
	s.mu.RUnlock()
	if resources == nil {
		return prompt, nil
	}
	expanded, err := resources.ExpandPromptInput(prompt)
	if err != nil {
		return "", fmt.Errorf("%w: expand prompt resources: %w", ErrInvalidRun, err)
	}
	return expanded, nil
}

func (s *AgentSession) expandUserContentInput(content []llm.UserContentBlock) ([]llm.UserContentBlock, error) {
	expanded := append([]llm.UserContentBlock(nil), content...)
	for index, block := range expanded {
		text, ok := block.(llm.TextBlock)
		if !ok {
			continue
		}
		value, err := s.expandPromptInput(text.Text())
		if err != nil {
			return nil, err
		}
		replacement, err := llm.NewTextBlock(value)
		if err != nil {
			return nil, fmt.Errorf("%w: expanded prompt content: %w", ErrInvalidRun, err)
		}
		expanded[index] = replacement
		break
	}
	return expanded, nil
}

func (s *AgentSession) Run(ctx context.Context, prompt string) (Result, error) {
	return s.runTextWithOptions(ctx, prompt, PromptOptions{})
}
func (s *AgentSession) Prompt(ctx context.Context, prompt string) (Result, error) {
	return s.Run(ctx, prompt)
}

// RunContent is the rich-input counterpart to Run and follows the same
// admission, lifecycle, and persistence boundaries.
func (s *AgentSession) RunContent(ctx context.Context, content []llm.UserContentBlock) (Result, error) {
	return s.runContentWithOptions(ctx, content, PromptOptions{})
}
func (s *AgentSession) PromptContent(ctx context.Context, content []llm.UserContentBlock) (Result, error) {
	return s.RunContent(ctx, content)
}

// RunMessages is the multi-message prompt surface used by hosts that already
// operate on pi's AgentMessage union (for example pending next-turn custom
// messages). The supplied order remains intact and extension messages follow
// the complete prompt batch.
func (s *AgentSession) RunMessages(ctx context.Context, messages []agentmsg.Message) (Result, error) {
	if err := s.rejectIfClosed(); err != nil {
		return Result{}, err
	}
	if s.loop == nil {
		return Result{}, fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	return s.runSession(ctx, true, func() (sessionPromptInput, error) {
		if err := validateAgentMessageBatch(messages, "agent message prompt"); err != nil {
			return sessionPromptInput{}, err
		}
		initial := agentmsg.Clone(messages)
		prompt, images := promptTextAndImages(initial)
		return sessionPromptInput{Text: prompt, Messages: agentmsg.Clone(initial), Images: images}, nil
	}, nil, nil, func(run context.Context, input sessionPromptInput, extra []agentmsg.Message) (Result, error) {
		return s.loop.RunAgentMessages(run, append(agentmsg.Clone(input.Messages), extra...))
	})
}

func (s *AgentSession) PromptMessages(ctx context.Context, messages []agentmsg.Message) (Result, error) {
	return s.RunMessages(ctx, messages)
}

func promptTextAndImages(messages []agentmsg.Message) (string, []llm.ImageBlock) {
	var prompt string
	var images []llm.ImageBlock
	appendText := func(text string) {
		if prompt != "" {
			prompt += "\n"
		}
		prompt += text
	}
	for _, message := range messages {
		wrapped, ok := message.(agentmsg.LLM)
		if !ok {
			continue
		}
		switch value := wrapped.Conversation().(type) {
		case llm.UserTextMessage:
			for _, block := range value.Content() {
				appendText(block.Text())
			}
		case llm.UserContentMessage:
			for _, block := range value.Content() {
				switch block := block.(type) {
				case llm.TextBlock:
					appendText(block.Text())
				case llm.ImageBlock:
					images = append(images, block)
				}
			}
		}
	}
	return prompt, images
}

func (s *AgentSession) systemPromptOptions() BuildSystemPromptOptions {
	if s == nil {
		return BuildSystemPromptOptions{}
	}
	s.mu.RLock()
	options := cloneBuildSystemPromptOptions(s.systemOptions)
	s.mu.RUnlock()
	return options
}

type sessionPromptInput struct {
	Text     string
	Messages []agentmsg.Message
	Images   []llm.ImageBlock
}

// sessionRunTransition performs a durable context transition after prompt
// validation but before compaction and Agent hooks run. The returned rollback
// is used only when prompt admission fails before the Agent run begins.
type sessionRunTransition func(*sessionRun) (rollback func() error, err error)

func (s *AgentSession) runSession(
	ctx context.Context,
	prePromptCheck bool,
	prepare func() (sessionPromptInput, error),
	transition sessionRunTransition,
	beforeBegin func(),
	begin func(context.Context, sessionPromptInput, []agentmsg.Message) (Result, error),
) (result Result, runErr error) {
	var providerTurns, toolExecutions uint32
	accumulate := func(next Result) Result {
		providerTurns += next.providerTurns
		toolExecutions += next.toolExecutions
		next.providerTurns = providerTurns
		next.toolExecutions = toolExecutions
		return next
	}
	run, err := s.admitSessionRun(ctx, true)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		runErr = joinBashSettlementError(runErr, s.settleSessionRun(run))
	}()
	var rollback func() error
	transitionCommitted := false
	defer func() {
		if runErr != nil && !transitionCommitted && rollback != nil {
			runErr = errors.Join(runErr, rollback())
		}
	}()
	// A prior run normally flushes these in its settlement path. Retrying here
	// preserves coding-agent's explicit pre-prompt flush after a transient
	// transcript failure without allowing the new low Agent run to overtake it.
	if err := s.flushPendingBashMessages(run.ctx); err != nil {
		return Result{}, err
	}
	if err := s.requireModelAccess(run.ctx); err != nil {
		if cause := context.Cause(run.ctx); cause != nil {
			return Result{}, cause
		}
		return Result{}, err
	}
	if cause := context.Cause(run.ctx); cause != nil {
		return Result{}, cause
	}
	var input sessionPromptInput
	if prepare != nil {
		input, err = prepare()
		if err != nil {
			if cause := context.Cause(run.ctx); cause != nil {
				return Result{}, cause
			}
			return Result{}, err
		}
	}
	if cause := context.Cause(run.ctx); cause != nil {
		return Result{}, cause
	}
	if transition != nil {
		rollback, err = transition(run)
		if err != nil {
			return Result{}, err
		}
	}
	if prePromptCheck {
		s.checkPrePromptCompaction(run)
		if cause := context.Cause(run.ctx); cause != nil {
			return Result{}, cause
		}
		// coding-agent consumes deliverAs:"nextTurn" only after model/auth and
		// pre-prompt compaction succeed. Pending custom messages follow the user
		// message and precede before_agent_start injections.
		s.lifecycleMu.Lock()
		if s.run == run && len(s.pendingNextTurn) != 0 {
			input.Messages = append(input.Messages, agentmsg.Clone(s.pendingNextTurn)...)
			s.pendingNextTurn = nil
		}
		s.lifecycleMu.Unlock()
	}
	var extra []agentmsg.Message
	if hook := s.hooks.BeforeAgentStart; prePromptCheck && hook != nil {
		state := s.State()
		out, hookErr := hook(run.ctx, BeforeAgentStartEvent{
			Prompt: input.Text, Images: append([]llm.ImageBlock(nil), input.Images...), PromptMessages: agentmsg.Clone(input.Messages),
			SystemPrompt: state.SystemPrompt, SystemPromptOptions: s.systemPromptOptions(), Messages: s.loop.State().Messages(),
		})
		if cause := context.Cause(run.ctx); cause != nil {
			return Result{}, cause
		}
		if hookErr != nil {
			return Result{}, hookErr
		}
		if err := out.Cancel.Validate(); err != nil {
			return Result{}, err
		}
		if out.Cancel.Cancelled() {
			if out.Cancel.Reason == "" {
				return Result{}, ErrAgentAborted
			}
			return Result{}, fmt.Errorf("%w: %s", ErrAgentAborted, out.Cancel.Reason)
		}
		if out.SystemPrompt != nil {
			value := *out.SystemPrompt
			s.lifecycleMu.Lock()
			run.extensionSystemPrompt = &value
			s.lifecycleMu.Unlock()
		}
		extra = agentmsg.Clone(out.ExtraMessages)
	}

	if prePromptCheck {
		s.setSessionPhase(run, PhaseProvider)
	}
	transitionCommitted = true
	if beforeBegin != nil {
		beforeBegin()
	}
	result, runErr = s.runLowAgent(run, func() (Result, error) {
		return begin(run.ctx, input, extra)
	})
	result = accumulate(result)
	for runErr == nil {
		if cause := context.Cause(run.ctx); cause != nil {
			s.endRetrySeries(run.ctx, false, cause.Error())
			// Cancellation after admission is represented by a settled terminal
			// result, never a Go-level lifecycle error.
			return result, nil
		}
		retryEnabled, retryController := s.currentRetrySettings()
		retryAttempt := uint32(0)
		if retryEnabled && s.retryableResult(result) {
			s.lifecycleMu.Lock()
			if s.run == run && run.retryAttempt+1 < retryController.MaxAttempts() {
				run.retryAttempt++
				retryAttempt = run.retryAttempt
			}
			s.lifecycleMu.Unlock()
		}
		if retryAttempt != 0 {
			nextAttempt := retryAttempt + 1
			// Provider adapters already apply Retry-After to their own request
			// retries. coding-agent starts a separate Agent retry series here and
			// bases it only on settings.retry.baseDelayMs, avoiding a second
			// application of the server delay after provider exhaustion.
			delay := retryController.Delay(nextAttempt, nil)
			errorMessage := retryErrorMessage(result)
			maxRetries := retryBudget(retryController.MaxAttempts())
			s.beginRetrySeries(run, delay, errorMessage, maxRetries)
			s.setSessionPhase(run, PhaseRetryWait)
			s.emitControl(run.ctx, "auto_retry_start", retryControl{
				attempt: retryAttempt, max: maxRetries, delay: delay, errorMessage: errorMessage,
			})
			// The failed assistant turn is durable history but must not be sent
			// back to the provider when resending this attempt.
			s.removeLastFailureFromAgentState()
			waitCtx, cancelRetry := context.WithCancelCause(run.ctx)
			s.lifecycleMu.Lock()
			if s.run == run {
				run.retryCancel = cancelRetry
			}
			s.lifecycleMu.Unlock()
			waitErr := retryController.Wait(waitCtx, delay)
			waitCause := context.Cause(waitCtx)
			s.lifecycleMu.Lock()
			if s.run == run {
				run.retryCancel = nil
			}
			s.lifecycleMu.Unlock()
			cancelRetry(nil)
			if waitErr != nil {
				finalError := waitErr.Error()
				if errors.Is(waitCause, errRetryCancelled) {
					finalError = "Retry cancelled"
				}
				s.endRetrySeries(run.ctx, false, finalError)
				s.resetRetryState(run)
				// Public Abort/AbortRetry cancel only this delay. As in pi, a
				// cancelled retry still proceeds through overflow/threshold
				// compaction and queued-message continuation. Only cancellation
				// of the owning session context (caller cancellation or shutdown)
				// terminates the complete pipeline here.
				if context.Cause(run.ctx) != nil {
					return result, nil
				}
			} else {
				result, runErr = s.runLowAgent(run, func() (Result, error) {
					return s.loop.Continue(run.ctx)
				})
				result = accumulate(result)
				continue
			}
		}
		if s.checkPostRunCompaction(run, result) {
			result, runErr = s.runLowAgent(run, func() (Result, error) {
				return s.loop.Continue(run.ctx)
			})
			result = accumulate(result)
			continue
		}
		s.resetRetryState(run)
		// agent_end observers run synchronously before the low run returns. A
		// newly queued message is therefore visible here and gets its own low
		// continuation while the top-level admission remains held.
		s.lifecycleMu.Lock()
		if s.run == run {
			run.acceptingQueues = false
		}
		s.lifecycleMu.Unlock()
		if !s.loop.HasQueuedMessages() {
			return result, runErr
		}
		s.lifecycleMu.Lock()
		if s.run == run {
			run.acceptingQueues = true
		}
		s.lifecycleMu.Unlock()
		result, runErr = s.runLowAgent(run, func() (Result, error) {
			return s.loop.Continue(run.ctx)
		})
		result = accumulate(result)
	}
	return result, runErr
}

func retryErrorMessage(result Result) string {
	terminal, ok := result.Terminal()
	if !ok {
		return ""
	}
	return retryTerminalErrorMessage(terminal)
}

func retryTerminalErrorMessage(terminal llm.AssistantTerminal) string {
	if failure, ok := terminal.(llm.AssistantFailureMessage); ok {
		if providerFailure := providerFailureFromTerminalForSession(Result{terminal: terminal}); providerFailure != nil {
			if message, exists := providerFailure.RetryMessage(); exists {
				return message
			}
		}
		return failure.ErrorMessage()
	}
	return ""
}

func (s *AgentSession) beginRetrySeries(run *sessionRun, delay time.Duration, errorMessage string, maxRetries uint32) {
	s.lifecycleMu.Lock()
	if s.run == run {
		run.retrySeries = true
		run.retryDelay = delay
		run.retryError = errorMessage
		run.retryMax = maxRetries
	}
	s.lifecycleMu.Unlock()
}

// endRetrySeries emits at most once. Successful dispatch calls this from the
// assistant commit event; failed dispatch calls it after the final agent_end.
func (s *AgentSession) endRetrySeries(ctx context.Context, succeeded bool, finalError string) {
	s.lifecycleMu.Lock()
	run := s.run
	if run == nil || !run.retrySeries {
		s.lifecycleMu.Unlock()
		return
	}
	payload := retryControl{
		attempt: run.retryAttempt, max: run.retryMax, delay: run.retryDelay,
		succeeded: succeeded, errorMessage: run.retryError, finalError: finalError,
	}
	run.retrySeries = false
	s.lifecycleMu.Unlock()
	s.emitControl(ctx, "auto_retry_end", payload)
}

func (s *AgentSession) resetRetryState(run *sessionRun) {
	s.lifecycleMu.Lock()
	if s.run == run {
		run.retryAttempt = 0
		run.retryDelay = 0
		run.retryError = ""
		run.retryMax = 0
		run.retryCancel = nil
	}
	s.lifecycleMu.Unlock()
}

// maxRetries uses the product-facing retry budget: the initial request is not
// a retry. Provider RetryController counts the initial request in MaxAttempts.
func (s *AgentSession) maxRetries() uint32 {
	if s == nil {
		return 0
	}
	_, controller := s.currentRetrySettings()
	return retryBudget(controller.MaxAttempts())
}

func retryBudget(maxAttempts uint32) uint32 {
	if maxAttempts == 0 {
		return 0
	}
	return maxAttempts - 1
}

func (s *AgentSession) checkPrePromptCompaction(run *sessionRun) {
	messages, err := s.agentStateLLMMessages()
	if err != nil {
		return
	}
	if len(messages) == 0 {
		return
	}
	terminal, ok := messages[len(messages)-1].(llm.AssistantTerminal)
	if !ok {
		return
	}
	provenance := terminal.AssistantProvenance()
	currentModel := s.State().Model
	if provenance.Provider != currentModel.Provider() || provenance.Model != currentModel.ID() {
		return
	}
	// Pre-prompt policy deliberately never continues: the pending user prompt
	// is about to create the next low run itself.
	_ = s.checkCompaction(run, terminal, false)
}

// compactBeforeNextAssistantResponse checks the complete context at the
// request preparation boundary. The caller includes rebuilt Agent messages
// in the request snapshot only when a compaction has committed.
func (s *AgentSession) compactBeforeNextAssistantResponse(ctx context.Context, turn TurnContext) bool {
	if s == nil || context.Cause(ctx) != nil || s.sessionManager == nil || !s.compactionAvailable() || !s.AutoCompactionEnabled() {
		return false
	}
	state := s.State()
	window, reserve := s.compactionLimitsFor(state.Model)
	if window == 0 {
		return false
	}
	estimate, err := session.EstimateAgentContextTokens(turn.Messages)
	if err != nil || !compactionThresholdExceeded(estimate.Tokens, window, reserve) {
		return false
	}

	s.lifecycleMu.Lock()
	run := s.run
	s.lifecycleMu.Unlock()
	if run == nil {
		return false
	}
	defer s.setSessionPhase(run, PhaseProvider)
	return s.runCompaction(run, CompactionThreshold, false, "")
}

// checkPostRunCompaction returns true only when overflow recovery compacted
// successfully and the caller must Continue from the Agent messages rebuilt
// from SessionManager. Threshold/successful-over-window compaction never
// fabricates a provider continuation.
func (s *AgentSession) checkPostRunCompaction(run *sessionRun, result Result) bool {
	terminal, ok := result.Terminal()
	if !ok {
		return false
	}
	return s.checkCompaction(run, terminal, true)
}

func (s *AgentSession) checkCompaction(run *sessionRun, terminal llm.AssistantTerminal, skipAborted bool) bool {
	if s == nil || s.sessionManager == nil || !s.compactionAvailable() || !s.AutoCompactionEnabled() || terminal == nil {
		return false
	}
	if skipAborted && terminal.FinishReason() == llm.FinishAborted {
		return false
	}
	compactionBoundary, hasCompactionBoundary := s.latestCompactionBoundary()
	if hasCompactionBoundary && !terminal.Timestamp().After(compactionBoundary) {
		return false
	}
	terminalModel := s.terminalRunModel(run)
	currentModel := s.State().Model
	if terminalModel.ID() != "" && !sameModelIdentity(terminalModel, currentModel) {
		return false
	}
	if terminalModel.ID() == "" {
		terminalModel = currentModel
	}
	window, reserve := s.compactionLimitsFor(terminalModel)
	if window == 0 {
		return false
	}
	if failure := providerFailureFromTerminalForSession(Result{terminal: terminal}); failure != nil && failure.Kind() == provider.FailureContextOverflow {
		s.lifecycleMu.Lock()
		already := run.overflowCompacted
		if !already && s.run == run {
			run.overflowCompacted = true
		}
		s.lifecycleMu.Unlock()
		if already {
			s.emitCompaction(run.ctx, "compaction_end", CompactionContextOverflow, nil, false, false,
				"Context overflow recovery failed after one compact-and-retry attempt. Try reducing context or switching to a larger-context model.")
			return false
		}
		// Preserve the failed assistant durably, but never include it in the
		// retry context. Compaction rebuilds Agent messages from the durable
		// branch on success.
		s.removeLastFailureFromAgentState()
		if s.runCompaction(run, CompactionContextOverflow, true, "") {
			return true
		}
		return false
	}
	if terminal.FinishReason() != llm.FinishError && terminal.Usage().TotalTokens() > window {
		_ = s.runCompaction(run, CompactionContextOverflow, false, "")
		return false
	}

	directContextTokens := terminal.Usage().TotalTokens()
	contextTokens := directContextTokens
	if terminal.FinishReason() == llm.FinishError || directContextTokens == 0 {
		messages := s.loop.State().Messages()
		estimate, err := session.EstimateAgentContextTokens(messages)
		if err != nil || estimate.LastUsageIndex < 0 {
			return false
		}
		usageMessage := messages[estimate.LastUsageIndex]
		if hasCompactionBoundary && !usageMessage.Timestamp().After(compactionBoundary) {
			return false
		}
		contextTokens = estimate.Tokens
	}
	if !compactionThresholdExceeded(contextTokens, window, reserve) {
		return false
	}
	s.lifecycleMu.Lock()
	already := run.thresholdCompactionAttempted
	if !already && s.run == run {
		run.thresholdCompactionAttempted = true
	}
	s.lifecycleMu.Unlock()
	if already {
		return false
	}
	_ = s.runCompaction(run, CompactionThreshold, false, "")
	return false
}

func (s *AgentSession) latestCompactionBoundary() (time.Time, bool) {
	if s == nil || s.sessionManager == nil {
		return time.Time{}, false
	}
	branch, err := s.sessionManager.BranchPath("")
	if err != nil {
		return time.Time{}, false
	}
	entry, ok := session.LatestCompactionEntry(branch)
	if !ok {
		return time.Time{}, false
	}
	return entry.Timestamp(), true
}

func compactionThresholdExceeded(tokens, contextWindow, reserveTokens uint64) bool {
	if contextWindow <= reserveTokens {
		return tokens > 0
	}
	return tokens > contextWindow-reserveTokens
}

func (s *AgentSession) terminalRunModel(run *sessionRun) provider.Model {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.run != run {
		return provider.Model{}
	}
	return run.terminalModel
}

func sameModelIdentity(left, right provider.Model) bool {
	return left.Provider() == right.Provider() && left.ID() == right.ID()
}

func (s *AgentSession) compactionLimitsFor(model provider.Model) (uint64, uint64) {
	s.mu.RLock()
	window, reserve := s.contextWindow, s.contextReserve
	s.mu.RUnlock()
	if model.ContextWindow() != 0 {
		window = model.ContextWindow()
	}
	return window, reserve
}

func (s *AgentSession) compactionAvailable() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	available := s.summarizer != nil || s.resolveSummarizer != nil
	s.mu.RUnlock()
	return available
}

func (s *AgentSession) beginCompaction(run *sessionRun) (*runCancellation, bool) {
	if s == nil || run == nil {
		return nil, false
	}
	ctx, cancel := context.WithCancelCause(run.ctx)
	domain := &runCancellation{ctx: ctx, cancel: cancel}
	s.lifecycleMu.Lock()
	if s.run != run || run.compaction != nil {
		s.lifecycleMu.Unlock()
		cancel(context.Canceled)
		return nil, false
	}
	run.compaction = domain
	s.lifecycleMu.Unlock()
	return domain, true
}

func (s *AgentSession) endCompaction(run *sessionRun, domain *runCancellation) {
	if domain == nil {
		return
	}
	s.lifecycleMu.Lock()
	if s.run == run && run.compaction == domain {
		run.compaction = nil
	}
	s.lifecycleMu.Unlock()
	domain.cancel(context.Canceled)
}

func (s *AgentSession) beginBranchSummary(run *sessionRun) (*runCancellation, bool) {
	if s == nil || run == nil {
		return nil, false
	}
	ctx, cancel := context.WithCancelCause(run.ctx)
	domain := &runCancellation{ctx: ctx, cancel: cancel}
	s.lifecycleMu.Lock()
	if s.run != run || run.branchCancellation != nil {
		s.lifecycleMu.Unlock()
		cancel(context.Canceled)
		return nil, false
	}
	run.branchSummary = true
	run.branchCancellation = domain
	s.lifecycleMu.Unlock()
	return domain, true
}

func (s *AgentSession) endBranchSummary(run *sessionRun, domain *runCancellation) {
	if domain == nil {
		return
	}
	s.lifecycleMu.Lock()
	if s.run == run && run.branchCancellation == domain {
		run.branchCancellation = nil
		run.branchSummary = false
	}
	s.lifecycleMu.Unlock()
	domain.cancel(context.Canceled)
}

// AbortCompaction cancels only the current manual or automatic compaction.
// Agent execution, retry, and branch-summary cancellation remain independent.
func (s *AgentSession) AbortCompaction() {
	if s == nil {
		return
	}
	s.lifecycleMu.Lock()
	var cancel context.CancelCauseFunc
	if s.run != nil && s.run.compaction != nil {
		cancel = s.run.compaction.cancel
	}
	s.lifecycleMu.Unlock()
	if cancel != nil {
		cancel(errCompactionCancelled)
	}
}

// runCompaction returns true only on a committed compaction. Automatic
// failures are surfaced as compaction_end and leave the surrounding request's
// settled result intact.
func (s *AgentSession) runCompaction(run *sessionRun, reason CompactionReason, willRetry bool, instructions string) bool {
	if s == nil || s.sessionManager == nil || !s.compactionAvailable() {
		return false
	}
	domain, ok := s.beginCompaction(run)
	if !ok {
		return false
	}
	defer s.endCompaction(run, domain)
	input, summarizer, err := s.prepareCompaction(domain.ctx, instructions)
	if err != nil {
		return false
	}
	s.setSessionPhase(run, PhaseCompacting)
	s.emitCompaction(domain.ctx, "compaction_start", reason, nil, false, willRetry, "")
	result, err := s.compactPrepared(run, domain.ctx, reason, willRetry, instructions, input, summarizer)
	if err != nil {
		aborted := context.Cause(domain.ctx) != nil || errors.Is(err, session.ErrAppendCanceled) || errors.Is(err, errExtensionCompactionCancelled)
		errorMessage := ""
		if !aborted {
			prefix := "Auto-compaction failed: "
			if reason == CompactionContextOverflow {
				prefix = "Context overflow recovery failed: "
			}
			errorMessage = prefix + safeCompactionEventError(err)
		}
		s.emitCompaction(domain.ctx, "compaction_end", reason, nil, aborted, false, errorMessage)
		return false
	}
	if willRetry {
		// A retained-tail policy may keep the durable overflow failure beside the
		// new checkpoint. It remains in history, but never belongs to resend
		// context.
		s.removeLastFailureFromAgentState()
	}
	s.afterCompaction(domain.ctx, result, reason, willRetry)
	s.emitCompaction(domain.ctx, "compaction_end", reason, &result, false, willRetry, "")
	return true
}

var (
	errCompactionCancelled          = errors.New("Compaction cancelled")
	errExtensionCompactionCancelled = errors.New("extension cancelled compaction")
)

func (s *AgentSession) compactTranscript(run *sessionRun, ctx context.Context, reason CompactionReason, willRetry bool, instructions string) (session.CompactResult, error) {
	input, summarizer, err := s.prepareCompaction(ctx, instructions)
	if err != nil {
		return session.CompactResult{}, err
	}
	return s.compactPrepared(run, ctx, reason, willRetry, instructions, input, summarizer)
}

func (s *AgentSession) prepareCompaction(ctx context.Context, instructions string) (session.SummaryInput, session.Summarizer, error) {
	summarizer, err := s.resolveCompactionSummarizer(ctx)
	if err != nil {
		return session.SummaryInput{}, nil, err
	}
	compactionEnabled := s.AutoCompactionEnabled()
	input, err := s.sessionManager.PrepareCompactionWithOptions(ctx, session.PrepareCompactionOptions{
		KeepRecentTokens: s.keepRecentTokens, KeepRecentTokensSet: s.keepRecentSet,
		ReserveTokens: s.contextReserve, ReserveTokensSet: true, Instructions: instructions,
		Enabled: compactionEnabled, EnabledSet: true,
	})
	if err != nil {
		return session.SummaryInput{}, nil, err
	}
	return input, summarizer, nil
}

func (s *AgentSession) compactPrepared(run *sessionRun, ctx context.Context, reason CompactionReason, willRetry bool, instructions string, input session.SummaryInput, resolved session.Summarizer) (session.CompactResult, error) {
	base := sessionObservedSummarizer{session: s, run: run, reason: reason, base: resolved}
	summarizer := extensionCompactionSummarizer{session: s, reason: reason, willRetry: willRetry, instructions: instructions, base: base}
	output, err := summarizer.Summarize(ctx, input)
	if cause := context.Cause(ctx); cause != nil {
		if err != nil {
			return session.CompactResult{}, fmt.Errorf("%w: %w", session.ErrAppendCanceled, errors.Join(cause, err))
		}
		return session.CompactResult{}, fmt.Errorf("%w: %w", session.ErrAppendCanceled, cause)
	}
	if err != nil {
		return session.CompactResult{}, fmt.Errorf("%w: %w", session.ErrSummaryFailed, err)
	}
	result, err := s.sessionManager.CommitCompaction(ctx, input, output)
	if err != nil {
		return session.CompactResult{}, err
	}
	if err := s.reloadAgentMessagesFromSession(); err != nil {
		return session.CompactResult{}, err
	}
	return result, nil
}

func (s *AgentSession) resolveCompactionSummarizer(ctx context.Context) (session.Summarizer, error) {
	if s == nil {
		return nil, ErrCompactionUnavailable
	}
	s.selectionMu.RLock()
	state := s.loop.State()
	s.mu.RLock()
	stream := provider.CloneStreamOptions(s.stream)
	resolveStream := s.resolveStream
	resolveSummarizer := s.resolveSummarizer
	static := s.summarizer
	validate := s.validateAccess
	s.mu.RUnlock()
	s.selectionMu.RUnlock()
	hasModel, selected, thinking := state.HasModel(), state.Model(), state.ThinkingLevel()
	runtimeSettings := s.resolvedRuntimeSettings()
	retry := runtimeSettings.Retry
	if !runtimeSettings.AutoRetryEnabled {
		retry.MaxAttempts = 1
	}
	if !hasModel {
		return nil, s.noModelSelectedError()
	}
	if resolveSummarizer == nil {
		if validate != nil {
			if err := validate(ctx, selected); err != nil {
				return nil, err
			}
		}
		if static == nil {
			return nil, ErrCompactionUnavailable
		}
		return static, nil
	}
	if resolveStream != nil {
		resolved, err := resolveStream(ctx, selected)
		if err != nil {
			return nil, err
		}
		stream = provider.MergeStreamOptions(stream, resolved)
	} else if validate != nil {
		if err := validate(ctx, selected); err != nil {
			return nil, err
		}
	}
	stream = provider.MergeStreamOptions(stream, provider.StreamOptions{
		OnPayload: s.hooks.BeforeProviderRequest, OnHeaders: s.hooks.BeforeProviderHeaders, OnResponse: s.hooks.AfterProviderResponse,
	})
	maxTokens := s.contextReserve
	stream.MaxTokens = &maxTokens
	resolved, err := resolveSummarizer(ctx, SummarizerResolveRequest{
		Model: selected, ThinkingLevel: thinking, Stream: stream, Retry: retry,
	})
	if err != nil {
		return nil, err
	}
	if resolved == nil || isNilInterface(resolved) {
		return nil, ErrCompactionUnavailable
	}
	return resolved, nil
}

func (s *AgentSession) agentStateLLMMessages() ([]llm.ConversationMessage, error) {
	if s == nil || s.loop == nil {
		return nil, fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	return agentmsg.ConvertToLLM(s.loop.State().Messages())
}

func (s *AgentSession) removeLastFailureFromAgentState() {
	if s == nil || s.loop == nil {
		return
	}
	messages := s.loop.State().Messages()
	if len(messages) == 0 {
		return
	}
	standard, ok := messages[len(messages)-1].(agentmsg.LLM)
	if !ok {
		return
	}
	if _, failed := standard.Conversation().(llm.AssistantFailureMessage); !failed {
		return
	}
	_ = s.loop.SetMessages(messages[:len(messages)-1])
}

func (s *AgentSession) reloadAgentMessagesFromSession() error {
	if s == nil || s.loop == nil || s.sessionManager == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	return s.loop.SetMessages(s.sessionManager.BuildContext().AgentMessages())
}

// ReloadMessagesFromSession rebuilds the in-memory agent state from the
// manager's selected branch. Runtime calls this after an explicit new-session
// setup callback has populated the manager.
func (s *AgentSession) ReloadMessagesFromSession() error {
	if s == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closed || s.closing {
		return fmt.Errorf("%w: session is closed", ErrInvalidRun)
	}
	if s.run != nil || s.standaloneMutation {
		return fmt.Errorf("%w: cannot reload messages while the agent is running", ErrInvalidRun)
	}
	return s.reloadAgentMessagesFromSession()
}

func (s *AgentSession) afterCompaction(ctx context.Context, result session.CompactResult, reason CompactionReason, willRetry bool) {
	if hook := s.hooks.SessionCompact; hook != nil {
		firstKept, tokensBefore := result.Input.FirstKeptEntryID, result.Input.TokensBefore
		if result.Output.FromExtension {
			firstKept, tokensBefore = result.Output.FirstKeptEntryID, result.Output.TokensBefore
		}
		estimated := result.EstimatedTokensAfter
		_ = hook(ctx, SessionCompactEvent{
			CompactionEntry: result.Entry,
			Result: ExtensionCompactionResult{
				Summary: result.Output.Text, FirstKeptEntryID: firstKept, TokensBefore: tokensBefore,
				EstimatedTokensAfter: &estimated, Usage: result.Output.Usage, Details: bytes.Clone(result.Output.Details),
			},
			FromExtension: result.Output.FromExtension, Reason: reason, WillRetry: willRetry,
		})
	}
}

type extensionCompactionSummarizer struct {
	session      *AgentSession
	reason       CompactionReason
	willRetry    bool
	instructions string
	base         session.Summarizer
}

func (s extensionCompactionSummarizer) Summarize(ctx context.Context, input session.SummaryInput) (session.SummaryOutput, error) {
	hook := s.session.hooks.SessionBeforeCompact
	if hook == nil {
		return s.base.Summarize(ctx, input)
	}
	branch := []session.Entry(nil)
	if s.session.sessionManager != nil {
		branch, _ = s.session.sessionManager.BranchPath("")
	}
	eventInput := input
	eventInput.Messages = append([]llm.ConversationMessage(nil), input.Messages...)
	eventInput.MessagesToSummarize = agentmsg.Clone(input.MessagesToSummarize)
	eventInput.TurnPrefixMessages = agentmsg.Clone(input.TurnPrefixMessages)
	eventInput.RetainedTail = append([]llm.ConversationMessage(nil), input.RetainedTail...)
	eventInput.FileOperations.Read = append([]string(nil), input.FileOperations.Read...)
	eventInput.FileOperations.Written = append([]string(nil), input.FileOperations.Written...)
	eventInput.FileOperations.Edited = append([]string(nil), input.FileOperations.Edited...)
	result, err := hook(ctx, SessionBeforeCompactEvent{
		Preparation: eventInput, BranchEntries: append([]session.Entry(nil), branch...),
		CustomInstructions: optionalString(s.instructions), Reason: s.reason, WillRetry: s.willRetry,
	})
	if err != nil {
		// pi's extension runner reports handler errors and continues with the
		// default compactor. Hooks are the transport-neutral extension boundary
		// here, so a failing handler must not settle or mutate compaction state.
		return s.base.Summarize(ctx, input)
	}
	if err := result.Cancel.Validate(); err != nil {
		return session.SummaryOutput{}, err
	}
	if result.Cancel.Cancelled() {
		return session.SummaryOutput{}, errExtensionCompactionCancelled
	}
	if result.Compaction == nil {
		return s.base.Summarize(ctx, input)
	}
	extension := result.Compaction
	return session.SummaryOutput{
		Text: extension.Summary, FirstKeptEntryID: extension.FirstKeptEntryID,
		TokensBefore: extension.TokensBefore, EstimatedTokensAfter: cloneUint64(extension.EstimatedTokensAfter),
		Usage: cloneCompactionUsage(extension.Usage), Details: bytes.Clone(extension.Details), FromExtension: true,
	}, nil
}

func cloneUint64(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneCompactionUsage(value *session.CompactionUsage) *session.CompactionUsage {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	copy := value
	return &copy
}

type sessionObservedSummarizer struct {
	session *AgentSession
	run     *sessionRun
	reason  CompactionReason
	base    session.Summarizer
}

type summarizerWithRetryObserver interface {
	SummarizeWithRetryObserver(context.Context, session.SummaryInput, provider.RetryObserver) (session.SummaryOutput, error)
}

func (s sessionObservedSummarizer) Summarize(ctx context.Context, input session.SummaryInput) (session.SummaryOutput, error) {
	observable, ok := s.base.(summarizerWithRetryObserver)
	if !ok {
		return s.base.Summarize(ctx, input)
	}
	return observable.SummarizeWithRetryObserver(ctx, input, func(_ context.Context, retry provider.RetryEvent) {
		s.session.emitSummarizationRetry(ctx, s.reason, retry)
	})
}

func (s *AgentSession) emitSummarizationRetry(ctx context.Context, reason CompactionReason, retry provider.RetryEvent) {
	s.emitSummarizationRetryFrom(ctx, reason, "compaction", retry)
}

func (s *AgentSession) emitSummarizationRetryFrom(ctx context.Context, reason CompactionReason, source string, retry provider.RetryEvent) {
	kind := "summarization_retry_scheduled"
	switch retry.Kind {
	case provider.RetryAttempt:
		kind = "summarization_retry_attempt_start"
	case provider.RetryFinished:
		kind = "summarization_retry_finished"
	}
	s.emitCompactionRetry(ctx, kind, reason, source, retry)
}

func safeCompactionEventError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNoModelSelected):
		return err.Error()
	case errors.Is(err, ErrModelAccess), errors.Is(err, ErrCompactionUnavailable):
		return err.Error()
	case errors.Is(err, session.ErrAlreadyCompacted):
		return "Already compacted"
	case errors.Is(err, session.ErrNothingToCompact):
		return "Nothing to compact (session too small)"
	case errors.Is(err, session.ErrAppendCanceled):
		return session.ErrAppendCanceled.Error()
	case errors.Is(err, session.ErrCompactionConflict):
		return session.ErrCompactionConflict.Error()
	case errors.Is(err, session.ErrSummaryFailed):
		return session.ErrSummaryFailed.Error()
	default:
		return "session compaction failed"
	}
}

func (s *AgentSession) emitCompaction(ctx context.Context, kind string, reason CompactionReason, result *session.CompactResult, aborted, willRetry bool, errorMessage string) {
	if s == nil {
		return
	}
	s.mu.RLock()
	observers := make([]SessionObserver, 0, len(s.observers))
	for _, entry := range s.observers {
		if entry.observer != nil {
			observers = append(observers, entry.observer)
		}
	}
	s.mu.RUnlock()
	var event SessionEvent
	switch kind {
	case "compaction_start":
		event = CompactionStartEvent{Reason: reason, WillRetry: willRetry}
	case "compaction_end":
		var eventErr error
		if errorMessage != "" {
			if errorMessage == ErrNoModelSelected.Error() || errorMessage == s.noModelMessage ||
				errorMessage == "Compaction failed: "+ErrNoModelSelected.Error() || errorMessage == "Compaction failed: "+s.noModelMessage {
				eventErr = noModelSelectedGuidanceError{message: errorMessage}
			} else {
				eventErr = errors.New(errorMessage)
			}
		}
		event = CompactionEndEvent{
			Reason: reason, Result: result, Aborted: aborted,
			WillRetry: willRetry, ErrorMessage: errorMessage, Err: eventErr,
		}
	}
	if event != nil {
		s.emitToObservers(ctx, observers, event)
	}
}

func (s *AgentSession) emitCompactionRetry(ctx context.Context, kind string, reason CompactionReason, source string, retry provider.RetryEvent) {
	if s == nil {
		return
	}
	s.mu.RLock()
	observers := make([]SessionObserver, 0, len(s.observers))
	for _, entry := range s.observers {
		if entry.observer != nil {
			observers = append(observers, entry.observer)
		}
	}
	s.mu.RUnlock()
	maxRetries := s.maxRetries()
	if retry.MaxAttempts != 0 {
		maxRetries = retryBudget(retry.MaxAttempts)
	}
	var event SessionEvent
	switch kind {
	case "summarization_retry_scheduled":
		event = SessionSummarizationRetryScheduledEvent{
			Attempt: retryBudget(retry.Attempt), MaxAttempts: maxRetries,
			Delay: retry.Delay, ErrorMessage: retry.ErrorMessage, Reason: reason,
			FailureKind: retry.FailureKind, HTTPStatus: retry.HTTPStatus,
		}
	case "summarization_retry_attempt_start":
		event = SessionSummarizationRetryAttemptEvent{Source: source, Reason: reason}
	case "summarization_retry_finished":
		event = SessionSummarizationRetryFinishedEvent{
			Reason: reason, Attempt: retryBudget(retry.Attempt), FailureKind: retry.FailureKind,
			HTTPStatus: retry.HTTPStatus, Succeeded: retry.Succeeded,
			FinishReason: retry.FinishReason, FinalError: retry.FinalError,
		}
	}
	if event != nil {
		s.emitToObservers(ctx, observers, event)
	}
}

// emitToObservers gives every observer a fresh event. Session observers are
// user callbacks, so a mutation by one must never affect another observer or
// the session's retained state.
func (s *AgentSession) emitToObservers(ctx context.Context, observers []SessionObserver, event SessionEvent) {
	for _, observer := range observers {
		observer(ctx, cloneSessionEvent(event))
	}
}

func cloneSessionEvent(event SessionEvent) SessionEvent {
	switch value := event.(type) {
	case AgentStartEvent, TurnStartEvent, TurnEndEvent, MessageStartEvent, MessageUpdateEvent,
		MessageEndEvent, ToolExecutionStartEvent, ToolExecutionUpdateEvent, ToolExecutionEndEvent:
		agentEvent, ok := value.(AgentEvent)
		if !ok {
			return nil
		}
		cloned := cloneAgentEvent(agentEvent)
		if sessionEvent, ok := cloned.(SessionEvent); ok {
			return sessionEvent
		}
	case CompactionStartEvent, CompactionEndEvent:
		controlEvent, ok := value.(AgentControlEvent)
		if !ok {
			return nil
		}
		cloned := cloneAgentControlEvent(controlEvent)
		if sessionEvent, ok := cloned.(SessionEvent); ok {
			return sessionEvent
		}
	case SessionAgentEndEvent:
		value.Messages = agentmsg.Clone(value.Messages)
		return value
	case AgentSettledEvent, ThinkingLevelChangedEvent,
		AutoRetryStartEvent, AutoRetryEndEvent,
		SessionSummarizationRetryScheduledEvent, SessionSummarizationRetryAttemptEvent,
		SessionSummarizationRetryFinishedEvent, EntryAppendedEvent:
		return value
	case SessionQueueUpdateEvent:
		// coding-agent publishes queue_update arrays even when a side is empty.
		// Preserve that wire distinction while still handing each observer an
		// owned slice; append-to-nil would silently turn [] into null.
		value.Steering = append([]string{}, value.Steering...)
		value.FollowUp = append([]string{}, value.FollowUp...)
		value.SteeringMessages = append([]llm.ConversationMessage{}, value.SteeringMessages...)
		value.FollowUpMessages = append([]llm.ConversationMessage{}, value.FollowUpMessages...)
		return value
	case SessionInfoChangeEvent:
		if value.Name != nil {
			name := *value.Name
			value.Name = &name
		}
		return value
	case BashExecutionUpdateEvent:
		if value.ID != nil {
			id := *value.ID
			value.ID = &id
		}
		return value
	}
	return nil
}

// CloneSessionEvent gives transport-neutral hosts an owned copy of an event
// without exposing AgentSession's mutable observer bookkeeping.
func CloneSessionEvent(event SessionEvent) SessionEvent {
	return cloneSessionEvent(event)
}

func (s *AgentSession) sessionRunStarted(run *sessionRun) bool {
	s.lifecycleMu.Lock()
	started := s.run == run && run.started
	s.lifecycleMu.Unlock()
	return started
}

func (s *AgentSession) sessionRunCommittedAgentMessages(run *sessionRun) []agentmsg.Message {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.run != run {
		return nil
	}
	return agentmsg.Clone(run.committedAgent)
}

func (s *AgentSession) admitSessionRun(ctx context.Context, agentPipeline bool) (*sessionRun, error) {
	if s == nil || ctx == nil || context.Cause(ctx) != nil {
		return nil, fmt.Errorf("%w: invalid session context", ErrInvalidRun)
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closed || s.closing {
		return nil, fmt.Errorf("%w: session is closed", ErrInvalidRun)
	}
	if s.run != nil || s.standaloneMutation {
		return nil, ErrBusy
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	run := &sessionRun{
		ctx: runCtx, cancel: cancel, done: make(chan struct{}), phase: PhaseProvider, acceptingQueues: true,
		agentPipeline: agentPipeline,
	}
	if s.idleWait == nil {
		s.idleWait = make(chan struct{})
	}
	s.run = run
	return run, nil
}

func (s *AgentSession) setSessionPhase(run *sessionRun, phase Phase) {
	s.lifecycleMu.Lock()
	if s.run == run {
		run.phase = phase
	}
	s.lifecycleMu.Unlock()
}

func (s *AgentSession) finishSessionRun(run *sessionRun) {
	if run == nil {
		return
	}
	s.lifecycleMu.Lock()
	if s.run != run {
		s.lifecycleMu.Unlock()
		return
	}
	s.run = nil
	s.lifecycleMu.Unlock()
	s.completeSessionRun(run)
	s.resolveSessionIdle()
}

func (s *AgentSession) completeSessionRun(run *sessionRun) {
	if run == nil {
		return
	}
	run.finishOnce.Do(func() {
		run.cancel(context.Canceled)
		close(run.done)
	})
}

func (s *AgentSession) endSessionSettlement(run *sessionRun) {
	s.completeSessionRun(run)
	s.lifecycleMu.Lock()
	if s.settlingCallbacks > 0 {
		s.settlingCallbacks--
	}
	s.lifecycleMu.Unlock()
	s.resolveSessionIdle()
}

// resolveSessionIdle closes at most one shared generation. The final check is
// intentionally performed after per-run cleanup: if another goroutine admits
// a run in that window, it reuses idleWait and keeps all existing waiters
// attached to the new work instead of observing a transient idle edge.
func (s *AgentSession) resolveSessionIdle() {
	var idle chan struct{}
	s.lifecycleMu.Lock()
	if s.run == nil && !s.standaloneMutation && s.settlingCallbacks == 0 && s.idleWait != nil {
		idle = s.idleWait
		s.idleWait = nil
	}
	s.lifecycleMu.Unlock()
	if idle != nil {
		close(idle)
	}
}

// settleSessionRun publishes idle before agent_settled, matching the original
// AgentSession contract. Waiters that observed the old run are released only
// after all synchronous settled callbacks return, while a callback itself may
// immediately start a new prompt against the now-idle session.
func (s *AgentSession) settleSessionRun(run *sessionRun) error {
	if run == nil {
		return nil
	}
	s.setSessionPhase(run, PhaseSettling)
	started := s.sessionRunStarted(run)
	messages := s.sessionRunCommittedAgentMessages(run)
	// Keep the Bash completion gate across the final pending-message drain and
	// the isStreaming -> idle transition. The TypeScript implementation does
	// both synchronously in _runAgentPrompt's finally block; without this gate a
	// concurrently completing Bash command could enqueue after the final drain
	// and remain stranded until another prompt.
	s.bashRecordMu.Lock()
	flushErr := s.flushPendingBashMessagesLocked(run.ctx)
	s.lifecycleMu.Lock()
	if s.run != run {
		s.lifecycleMu.Unlock()
		s.bashRecordMu.Unlock()
		return flushErr
	}
	run.agentRunActive = false
	s.run = nil
	s.settlingCallbacks++
	s.lifecycleMu.Unlock()
	s.bashRecordMu.Unlock()
	defer s.endSessionSettlement(run)
	if started {
		s.emitAgentSettled(run.ctx, messages)
	}
	return flushErr
}

func providerFailureFromTerminalForSession(result Result) *provider.ProviderFailure {
	terminal, ok := result.Terminal()
	if !ok || terminal.FinishReason() != llm.FinishError {
		return nil
	}
	failure, ok := terminal.(llm.AssistantFailureMessage)
	if !ok {
		return nil
	}
	var providerFailure *provider.ProviderFailure
	if !errors.As(failure.Failure().Cause(), &providerFailure) {
		return nil
	}
	return providerFailure
}
func (s *AgentSession) retryableResult(result Result) bool {
	return provider.IsTransientFailure(providerFailureFromTerminalForSession(result))
}
func (s *AgentSession) willRetry(terminal llm.AssistantTerminal) bool {
	if terminal == nil || terminal.FinishReason() != llm.FinishError {
		return false
	}
	result := Result{terminal: terminal}
	enabled, retryController := s.currentRetrySettings()
	s.lifecycleMu.Lock()
	run := s.run
	will := enabled && run != nil && run.retryAttempt+1 < retryController.MaxAttempts() && s.retryableResult(result)
	s.lifecycleMu.Unlock()
	return will
}
func (s *AgentSession) Continue(ctx context.Context) (Result, error) {
	if err := s.rejectIfClosed(); err != nil {
		return Result{}, err
	}
	if s.loop == nil {
		return Result{}, fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	return s.runSession(ctx, false, nil, nil, nil, func(run context.Context, _ sessionPromptInput, _ []agentmsg.Message) (Result, error) {
		return s.loop.Continue(run)
	})
}
func (s *AgentSession) Steer(prompt string) error {
	return s.enqueueText(prompt, true)
}
func (s *AgentSession) SteerContent(content []llm.UserContentBlock) error {
	return s.enqueueContent(content, true)
}
func (s *AgentSession) FollowUp(prompt string) error {
	return s.enqueueText(prompt, false)
}
func (s *AgentSession) FollowUpContent(content []llm.UserContentBlock) error {
	return s.enqueueContent(content, false)
}
func (s *AgentSession) SteerAgentMessage(message agentmsg.Message) error {
	return s.enqueueAgentMessage(message, true)
}
func (s *AgentSession) FollowUpAgentMessage(message agentmsg.Message) error {
	return s.enqueueAgentMessage(message, false)
}

func (s *AgentSession) enqueueText(prompt string, steering bool) error {
	if s == nil || s.loop == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	if err := s.rejectQueuedExtensionCommand(prompt); err != nil {
		return err
	}
	expanded, err := s.expandPromptInput(prompt)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidQueueMessage, err)
	}
	timestamp, err := s.loop.now() // injected clock stays outside lifecycleMu
	if err != nil {
		return err
	}
	text, err := llm.NewTextBlock(expanded)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidQueueMessage, err)
	}
	message, err := llm.NewUserContentMessage([]llm.UserContentBlock{text}, timestamp)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidQueueMessage, err)
	}
	return s.enqueueMessage(message, steering)
}

func (s *AgentSession) enqueueContent(content []llm.UserContentBlock, steering bool) error {
	if s == nil || s.loop == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	prompt, _ := promptContentTextAndImages(content)
	if err := s.rejectQueuedExtensionCommand(prompt); err != nil {
		return err
	}
	expanded, err := s.expandUserContentInput(content)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidQueueMessage, err)
	}
	timestamp, err := s.loop.now() // injected clock stays outside lifecycleMu
	if err != nil {
		return err
	}
	message, err := llm.NewUserContentMessage(expanded, timestamp)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidQueueMessage, err)
	}
	return s.enqueueMessage(message, steering)
}

func (s *AgentSession) enqueueMessage(message llm.ConversationMessage, steering bool) error {
	wrapped, err := agentmsg.NewLLM(message)
	if err != nil {
		return err
	}
	return s.enqueueAgentMessage(wrapped, steering)
}

func (s *AgentSession) enqueueAgentMessage(message agentmsg.Message, steering bool) error {
	if s == nil || s.loop == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	s.lifecycleMu.Lock()
	if s.closed || s.closing {
		s.lifecycleMu.Unlock()
		return fmt.Errorf("%w: session is closed", ErrInvalidRun)
	}
	if s.run != nil && !s.run.acceptingQueues {
		s.lifecycleMu.Unlock()
		return ErrBusy
	}
	// enqueueMessage only takes Agent.mu and invokes no clock/provider/tool or
	// observer callback, so it is safe to make close/admission atomic here.
	err := s.loop.enqueueAgentMessage(message, steering)
	s.lifecycleMu.Unlock()
	if err == nil {
		s.emitControl(context.Background(), "queue_update")
	}
	return err
}
func (s *AgentSession) Queues() (steering, followUp []llm.UserTextMessage) {
	if s == nil || s.loop == nil {
		return nil, nil
	}
	return s.loop.Queues()
}
func (s *AgentSession) RichQueues() (steering, followUp []llm.ConversationMessage) {
	if s == nil || s.loop == nil {
		return nil, nil
	}
	return s.loop.RichQueues()
}

// PendingQueue returns both the original text projection and the lossless rich
// messages currently waiting for steering or follow-up delivery.
func (s *AgentSession) PendingQueue() QueueState {
	if s == nil || s.loop == nil {
		return QueueState{}
	}
	steering, followUp := s.RichQueues()
	return newQueueState(steering, followUp)
}

func (s *AgentSession) PendingMessageCount() int {
	queue := s.PendingQueue()
	return len(queue.SteeringMessages) + len(queue.FollowUpMessages)
}
func (s *AgentSession) SteeringMode() QueueMode {
	if s == nil || s.loop == nil {
		return 0
	}
	return s.loop.SteeringMode()
}
func (s *AgentSession) FollowUpMode() QueueMode {
	if s == nil || s.loop == nil {
		return 0
	}
	return s.loop.FollowUpMode()
}

func (s *AgentSession) sessionQueueUpdateEvent() SessionQueueUpdateEvent {
	steering, followUp := s.RichQueues()
	return newSessionQueueUpdateEvent(steering, followUp)
}

func sessionQueueUpdateEventFromAgent(value QueueUpdateEvent) SessionQueueUpdateEvent {
	steering, _ := agentmsg.ConvertToLLM(value.SteeringMessages)
	followUp, _ := agentmsg.ConvertToLLM(value.FollowUpMessages)
	return newSessionQueueUpdateEvent(steering, followUp)
}

func newSessionQueueUpdateEvent(steering, followUp []llm.ConversationMessage) SessionQueueUpdateEvent {
	queue := newQueueState(steering, followUp)
	return SessionQueueUpdateEvent{
		Steering: queue.Steering, FollowUp: queue.FollowUp,
		SteeringMessages: queue.SteeringMessages, FollowUpMessages: queue.FollowUpMessages,
	}
}

func newQueueState(steering, followUp []llm.ConversationMessage) QueueState {
	queue := QueueState{
		SteeringMessages: append([]llm.ConversationMessage(nil), steering...),
		FollowUpMessages: append([]llm.ConversationMessage(nil), followUp...),
		Steering:         make([]string, len(steering)),
		FollowUp:         make([]string, len(followUp)),
	}
	for index, message := range steering {
		queue.Steering[index] = queuedMessageText(message)
	}
	for index, message := range followUp {
		queue.FollowUp[index] = queuedMessageText(message)
	}
	return queue
}

func queuedMessageText(message llm.ConversationMessage) string {
	var builder strings.Builder
	switch value := message.(type) {
	case llm.UserTextMessage:
		for _, block := range value.Content() {
			builder.WriteString(block.Text())
		}
	case llm.UserContentMessage:
		for _, block := range value.Content() {
			if text, ok := block.(llm.TextBlock); ok {
				builder.WriteString(text.Text())
			}
		}
	}
	return builder.String()
}
func (s *AgentSession) ClearSteeringQueue() {
	s.clearQueues(func() { s.loop.ClearSteeringQueue() })
}
func (s *AgentSession) ClearFollowUpQueue() {
	s.clearQueues(func() { s.loop.ClearFollowUpQueue() })
}
func (s *AgentSession) ClearAllQueues() {
	s.clearQueues(func() { s.loop.ClearAllQueues() })
}

// ClearQueue is coding-agent's complete queue recall operation. The Agent
// snapshots and clears queued and already-staged delivery messages under one
// lock, preventing a concurrent turn drain from losing a recalled message.
// Like the original, this always emits an empty queue_update, including when
// there was nothing to clear.
func (s *AgentSession) ClearQueue() QueueState {
	if s == nil || s.loop == nil {
		return QueueState{}
	}
	s.lifecycleMu.Lock()
	if s.closed || s.closing {
		s.lifecycleMu.Unlock()
		return QueueState{}
	}
	steeringMessages, followUpMessages := s.loop.clearAllQueues()
	s.lifecycleMu.Unlock()
	steering, _ := agentmsg.ConvertToLLM(steeringMessages)
	followUp, _ := agentmsg.ConvertToLLM(followUpMessages)
	cleared := newQueueState(steering, followUp)
	s.emitQueueUpdate(context.Background(), newSessionQueueUpdateEvent(nil, nil))
	return cleared
}

func (s *AgentSession) clearQueues(clear func()) {
	if s == nil || s.loop == nil {
		return
	}
	s.lifecycleMu.Lock()
	if s.closed || s.closing {
		s.lifecycleMu.Unlock()
		return
	}
	beforeSteering, beforeFollowUp := s.RichQueues()
	clear()
	afterSteering, afterFollowUp := s.RichQueues()
	s.lifecycleMu.Unlock()
	if !sameMessages(beforeSteering, afterSteering) || !sameMessages(beforeFollowUp, afterFollowUp) {
		s.emitControl(context.Background(), "queue_update")
	}
}
func (s *AgentSession) Abort(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.lifecycleMu.Lock()
	run := s.run
	var idle chan struct{}
	var cancelRetry context.CancelCauseFunc
	if run != nil && run.agentPipeline {
		idle = s.idleWait
		cancelRetry = run.retryCancel
	} else {
		run = nil
	}
	s.lifecycleMu.Unlock()
	if run == nil {
		return nil
	}
	// Match coding-agent's independent cancellation domains: abort an active
	// retry delay and the low-level Agent request/tool run, but leave compaction
	// and the top-level session pipeline alive so post-run compaction and queued
	// continuation still execute.
	_ = s.loop.Abort(ctx)
	if cancelRetry != nil {
		// Cancel the retry only after sampling/aborting the currently active
		// low Agent run. This preserves JavaScript's single-turn ordering: the
		// awakened retry path cannot race ahead and have its queued continuation
		// mistaken for the run the caller intended to abort.
		cancelRetry(errRetryCancelled)
	}
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}
func (s *AgentSession) WaitForIdle(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.lifecycleMu.Lock()
	if s.run == nil && !s.standaloneMutation {
		s.lifecycleMu.Unlock()
		return nil
	}
	idle := s.idleWait
	s.lifecycleMu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}
func (s *AgentSession) Subscribe(observer SessionObserver) func() {
	if s == nil || observer == nil {
		return func() {}
	}
	s.lifecycleMu.Lock()
	if s.closed || s.closing {
		s.lifecycleMu.Unlock()
		return func() {}
	}
	s.mu.Lock()
	s.nextObserver++
	id := s.nextObserver
	s.observers = append(s.observers, sessionObserverEntry{id: id, observer: observer})
	s.mu.Unlock()
	s.lifecycleMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			for index := range s.observers {
				if s.observers[index].id == id {
					copy(s.observers[index:], s.observers[index+1:])
					s.observers[len(s.observers)-1] = sessionObserverEntry{}
					s.observers = s.observers[:len(s.observers)-1]
					break
				}
			}
			s.mu.Unlock()
		})
	}
}

// SessionShutdownOptions names the real lifecycle boundary used by the
// transport-neutral runtime. BeforeInvalidate runs synchronously after the
// session_shutdown hook and before the session is made stale.
type SessionShutdownOptions struct {
	Event            SessionShutdownHookEvent
	BeforeInvalidate func()
}

// PrepareSessionSwitch emits session_before_switch through the current
// session's hook set. It intentionally runs before the runtime opens or creates
// replacement resources.
func (s *AgentSession) PrepareSessionSwitch(ctx context.Context, event SessionBeforeSwitchEvent) (SessionBeforeSwitchResult, error) {
	if err := s.rejectIfClosed(); err != nil {
		return SessionBeforeSwitchResult{}, err
	}
	if s.hooks.SessionBeforeSwitch == nil {
		return SessionBeforeSwitchResult{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return s.hooks.SessionBeforeSwitch(ctx, event)
}

// PrepareSessionFork emits session_before_fork before entry validation, matching
// the original runtime's cancellation boundary.
func (s *AgentSession) PrepareSessionFork(ctx context.Context, event SessionBeforeForkEvent) (SessionBeforeForkResult, error) {
	if err := s.rejectIfClosed(); err != nil {
		return SessionBeforeForkResult{}, err
	}
	if s.hooks.SessionBeforeFork == nil {
		return SessionBeforeForkResult{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return s.hooks.SessionBeforeFork(ctx, event)
}

// Shutdown settles active work, emits the requested shutdown event, invokes
// the final synchronous invalidation callback, then disposes the owned manager
// and event subscriptions. It is safe to call repeatedly after success.
func (s *AgentSession) Shutdown(ctx context.Context, options SessionShutdownOptions) (shutdownErr error) {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.lifecycleMu.Lock()
	if active := s.shutdown; active != nil {
		s.lifecycleMu.Unlock()
		select {
		case <-active.done:
			return active.err
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	if s.closed {
		s.lifecycleMu.Unlock()
		return nil
	}
	attempt := &sessionShutdownAttempt{done: make(chan struct{})}
	s.shutdown = attempt
	defer func() {
		s.lifecycleMu.Lock()
		attempt.err = shutdownErr
		if s.shutdown == attempt {
			s.shutdown = nil
		}
		close(attempt.done)
		s.lifecycleMu.Unlock()
	}()
	s.closing = true
	run := s.run
	reloadDone := s.reloadDone
	var idle chan struct{}
	var cancelRetry context.CancelCauseFunc
	busy := run != nil || s.standaloneMutation
	if busy {
		idle = s.idleWait
	}
	if run != nil {
		cancelRetry = run.retryCancel
		run.cancel(ErrAgentAborted)
	}
	s.lifecycleMu.Unlock()
	// Shutdown is the owning-lifecycle cancellation boundary. Unlike public
	// Abort it must stop every session phase, including compaction and branch
	// summarization, before invalidating the manager.
	bashDone := s.abortBashExecutions()
	if cancelRetry != nil {
		cancelRetry(errRetryCancelled)
	}
	_ = s.loop.Abort(ctx)
	if busy {
		select {
		case <-idle:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	if reloadDone != nil {
		select {
		case <-reloadDone:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	if bashDone != nil {
		select {
		case <-bashDone:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	if err := s.flushPendingBashMessages(ctx); err != nil {
		s.lifecycleMu.Lock()
		s.closing = false
		s.lifecycleMu.Unlock()
		return err
	}
	if s.hooks.SessionShutdown != nil {
		if hookErr := callSessionShutdownHook(s.hooks.SessionShutdown, ctx, cloneSessionShutdownHookEvent(options.Event)); hookErr != nil {
			// session_shutdown follows the same ExtensionRunner error isolation as
			// reload. A failing extension must not strand a settled session, block
			// runtime replacement, or leave the owned manager writable.
			s.reportExtensionError(ctx, "session_shutdown", 0, hookErr)
		}
	}
	if options.BeforeInvalidate != nil {
		options.BeforeInvalidate()
	}
	if managerErr := s.sessionManager.Close(); managerErr != nil {
		return managerErr
	}
	s.lifecycleMu.Lock()
	s.closed = true
	s.closing = false
	s.lifecycleMu.Unlock()
	s.mu.Lock()
	unsubscribe := s.loopUnsubscribe
	s.loopUnsubscribe = nil
	s.observers = nil
	s.mu.Unlock()
	if unsubscribe != nil {
		unsubscribe()
	}
	return nil
}

// Close is the process-level compatibility path and therefore emits quit.
func (s *AgentSession) Close(ctx context.Context) error {
	return s.Shutdown(ctx, SessionShutdownOptions{Event: SessionShutdownHookEvent{Reason: ShutdownQuit}})
}

func cloneSessionStartHookEvent(event SessionStartHookEvent) SessionStartHookEvent {
	event.PreviousSessionFile = cloneStringPointer(event.PreviousSessionFile)
	return event
}

func cloneSessionShutdownHookEvent(event SessionShutdownHookEvent) SessionShutdownHookEvent {
	event.TargetSessionFile = cloneStringPointer(event.TargetSessionFile)
	return event
}

func cloneHeaderMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	copy := make(map[string]string, len(values))
	for key, value := range values {
		copy[key] = value
	}
	return copy
}
func (s *AgentSession) Compact(ctx context.Context, instructions string) (session.CompactResult, error) {
	if s == nil {
		return session.CompactResult{}, fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	if err := s.rejectIfClosed(); err != nil {
		return session.CompactResult{}, err
	}
	if s.sessionManager == nil {
		return session.CompactResult{}, ErrCompactionUnavailable
	}
	run, err := s.admitSessionRun(ctx, false)
	if err != nil {
		return session.CompactResult{}, err
	}
	defer s.finishSessionRun(run)
	domain, ok := s.beginCompaction(run)
	if !ok {
		return session.CompactResult{}, ErrBusy
	}
	defer s.endCompaction(run, domain)
	s.setSessionPhase(run, PhaseCompacting)
	s.emitCompaction(domain.ctx, "compaction_start", CompactionManual, nil, false, false, "")
	result, err := s.compactTranscript(run, domain.ctx, CompactionManual, false, instructions)
	if err != nil {
		aborted := context.Cause(domain.ctx) != nil || errors.Is(err, session.ErrAppendCanceled) || errors.Is(err, errExtensionCompactionCancelled)
		errorMessage := ""
		if !aborted {
			errorMessage = "Compaction failed: " + safeCompactionEventError(err)
		}
		s.emitCompaction(domain.ctx, "compaction_end", CompactionManual, nil, aborted, false, errorMessage)
		if errors.Is(err, errExtensionCompactionCancelled) {
			return session.CompactResult{}, ErrAgentAborted
		}
		return session.CompactResult{}, err
	}
	s.afterCompaction(domain.ctx, result, CompactionManual, false)
	s.emitCompaction(domain.ctx, "compaction_end", CompactionManual, &result, false, false, "")
	return result, nil
}

// NavigateTreeOptions mirrors pi's AgentSession.navigateTree options. Pointer
// fields preserve the distinction between an omitted extension override and
// an explicitly empty value.
type NavigateTreeOptions struct {
	Summarize           bool
	CustomInstructions  *string
	ReplaceInstructions *bool
	Label               *string
}

type NavigateTreeResult struct {
	EditorText   *string
	Cancelled    bool
	Aborted      bool
	SummaryEntry *session.Entry
}

type branchSummarizerWithRetryObserver interface {
	SummarizeBranchWithRetryObserver(context.Context, session.BranchSummaryInput, provider.RetryObserver) (session.BranchSummaryOutput, error)
}

func (s *AgentSession) resolveTreeSummarizer(ctx context.Context) (session.BranchSummarizer, provider.Model, error) {
	if s == nil {
		return nil, provider.Model{}, ErrBranchSummaryUnavailable
	}
	s.selectionMu.RLock()
	state := s.loop.State()
	s.mu.RLock()
	stream := provider.CloneStreamOptions(s.stream)
	resolveStream := s.resolveStream
	resolve := s.resolveBranchSummarizer
	static := s.branchSummarizer
	resolveCompaction := s.resolveSummarizer
	staticCompaction := s.summarizer
	validate := s.validateAccess
	s.mu.RUnlock()
	s.selectionMu.RUnlock()
	hasModel, selected, thinking := state.HasModel(), state.Model(), state.ThinkingLevel()
	runtimeSettings := s.resolvedRuntimeSettings()
	retry := runtimeSettings.Retry
	if !runtimeSettings.AutoRetryEnabled {
		retry.MaxAttempts = 1
	}
	if resolve == nil && static != nil && !isNilInterface(static) {
		if !hasModel {
			return nil, provider.Model{}, s.noModelSelectedError()
		}
		if validate != nil {
			if err := validate(ctx, selected); err != nil {
				return nil, provider.Model{}, err
			}
		}
		return static, selected, nil
	}
	if resolve == nil && resolveCompaction != nil {
		resolve = func(resolveCtx context.Context, request SummarizerResolveRequest) (session.BranchSummarizer, error) {
			value, err := resolveCompaction(resolveCtx, request)
			if err != nil {
				return nil, err
			}
			branch, ok := value.(session.BranchSummarizer)
			if !ok {
				return nil, ErrBranchSummaryUnavailable
			}
			return branch, nil
		}
	}
	if resolve == nil {
		static, _ = staticCompaction.(session.BranchSummarizer)
	}
	if !hasModel {
		return nil, provider.Model{}, s.noModelSelectedError()
	}
	if resolve == nil {
		if validate != nil {
			if err := validate(ctx, selected); err != nil {
				return nil, provider.Model{}, err
			}
		}
		if static == nil || isNilInterface(static) {
			return nil, provider.Model{}, ErrBranchSummaryUnavailable
		}
		return static, selected, nil
	}
	if resolveStream != nil {
		resolved, err := resolveStream(ctx, selected)
		if err != nil {
			return nil, provider.Model{}, err
		}
		stream = provider.MergeStreamOptions(stream, resolved)
	} else if validate != nil {
		if err := validate(ctx, selected); err != nil {
			return nil, provider.Model{}, err
		}
	}
	stream = provider.MergeStreamOptions(stream, provider.StreamOptions{
		OnPayload: s.hooks.BeforeProviderRequest, OnHeaders: s.hooks.BeforeProviderHeaders, OnResponse: s.hooks.AfterProviderResponse,
	})
	maxTokens := session.BranchSummaryMaxOutputTokens
	stream.MaxTokens = &maxTokens
	resolved, err := resolve(ctx, SummarizerResolveRequest{Model: selected, ThinkingLevel: thinking, Stream: stream, Retry: retry})
	if err != nil {
		return nil, provider.Model{}, err
	}
	if resolved == nil || isNilInterface(resolved) {
		return nil, provider.Model{}, ErrBranchSummaryUnavailable
	}
	return resolved, selected, nil
}

func (s *AgentSession) hasSelectedModel() bool {
	_, hasModel, _ := s.selectionSnapshot()
	return hasModel
}

type treeNavigationBusyError struct{}

func (treeNavigationBusyError) Error() string {
	return "Wait for the current response to finish before navigating the session tree."
}
func (treeNavigationBusyError) Unwrap() error { return ErrBusy }

type treeSummaryModelError struct{}

func (treeSummaryModelError) Error() string { return "No model available for summarization" }
func (treeSummaryModelError) Unwrap() error { return ErrNoModelSelected }

type treeEntryNotFoundError struct{ id string }

func (e treeEntryNotFoundError) Error() string { return fmt.Sprintf("Entry %s not found", e.id) }
func (e treeEntryNotFoundError) Unwrap() error { return session.ErrEntryNotFound }

// NavigateTree faithfully performs pi's tree navigation: it summarizes the
// abandoned suffix when requested, places user/custom targets in the editor,
// persists extension provenance, and only publishes the new Agent state after
// the durable tree operation succeeds.
func (s *AgentSession) NavigateTree(ctx context.Context, targetID string, options NavigateTreeOptions) (NavigateTreeResult, error) {
	if ctx == nil {
		return NavigateTreeResult{}, fmt.Errorf("%w: nil tree context", ErrInvalidRun)
	}
	if err := s.rejectIfClosed(); err != nil {
		return NavigateTreeResult{}, err
	}
	run, err := s.admitSessionRun(ctx, false)
	if err != nil {
		if errors.Is(err, ErrBusy) {
			return NavigateTreeResult{}, treeNavigationBusyError{}
		}
		return NavigateTreeResult{}, err
	}
	defer s.finishSessionRun(run)
	return s.navigateTreeWithRun(run, targetID, options, false)
}

func (s *AgentSession) navigateTreeWithRun(
	run *sessionRun,
	targetID string,
	options NavigateTreeOptions,
	reopenEditableTarget bool,
) (NavigateTreeResult, error) {
	manager := s.sessionManager
	oldLeaf, _ := manager.LeafID()
	if oldLeaf == targetID && !reopenEditableTarget {
		return NavigateTreeResult{}, nil
	}
	if options.Summarize && !s.hasSelectedModel() {
		return NavigateTreeResult{}, treeSummaryModelError{}
	}
	target, ok := manager.Entry(targetID)
	if !ok {
		return NavigateTreeResult{}, treeEntryNotFoundError{id: targetID}
	}
	oldPath, _ := manager.BranchPath("")
	targetPath, err := manager.BranchPath(targetID)
	if err != nil {
		return NavigateTreeResult{}, err
	}
	collected := session.CollectEntriesForBranchSummary(oldPath, targetPath)
	customInstructions := cloneStringPointer(options.CustomInstructions)
	replaceInstructions := cloneBoolPointer(options.ReplaceInstructions)
	label := cloneStringPointer(options.Label)
	preparation := TreePreparation{
		TargetID: targetID, OldLeafID: optionalString(oldLeaf), CommonAncestorID: cloneStringPointer(collected.CommonAncestorID),
		EntriesToSummarize: cloneSessionEntries(collected.Entries), UserWantsSummary: options.Summarize,
		CustomInstructions: cloneStringPointer(customInstructions), ReplaceInstructions: cloneBoolPointer(replaceInstructions), Label: cloneStringPointer(label),
	}
	// pi installs the branch-summary abort controller immediately before the
	// first extension/summarizer await. Publish the equivalent phase at that
	// same boundary, after all synchronous target validation is complete.
	s.setSessionPhase(run, PhaseCompacting)
	branch, ok := s.beginBranchSummary(run)
	if !ok {
		return NavigateTreeResult{}, ErrBusy
	}
	defer s.endBranchSummary(run, branch)
	branchCtx := branch.ctx
	var extensionSummary *TreeSummary
	if hook := s.hooks.SessionBeforeTree; hook != nil {
		result, hookErr := hook(branchCtx, SessionBeforeTreeEvent{Preparation: preparation})
		if hookErr == nil {
			if err := result.Cancel.Validate(); err != nil {
				return NavigateTreeResult{}, err
			}
			if result.Cancel.Cancelled() {
				return NavigateTreeResult{Cancelled: true}, nil
			}
			if options.Summarize && result.Summary != nil {
				value := *result.Summary
				value.Details = bytes.Clone(value.Details)
				value.Usage = cloneCompactionUsage(value.Usage)
				extensionSummary = &value
			}
			if result.CustomInstructions != nil {
				customInstructions = cloneStringPointer(result.CustomInstructions)
			}
			if result.ReplaceInstructions != nil {
				replaceInstructions = cloneBoolPointer(result.ReplaceInstructions)
			}
			if result.Label != nil {
				label = cloneStringPointer(result.Label)
			}
		}
	}

	summaryText := ""
	var summaryDetails json.RawMessage
	var summaryUsage *session.CompactionUsage
	fromExtension := false
	if extensionSummary != nil {
		summaryText, summaryDetails = extensionSummary.Summary, bytes.Clone(extensionSummary.Details)
		summaryUsage, fromExtension = cloneCompactionUsage(extensionSummary.Usage), true
	} else if options.Summarize && len(collected.Entries) > 0 {
		resolved, selected, resolveErr := s.resolveTreeSummarizer(branchCtx)
		if resolveErr != nil {
			if context.Cause(branchCtx) != nil {
				return NavigateTreeResult{Cancelled: true, Aborted: true}, nil
			}
			return NavigateTreeResult{}, resolveErr
		}
		s.mu.RLock()
		reserve := s.branchSummaryReserve
		s.mu.RUnlock()
		custom := ""
		if customInstructions != nil {
			custom = *customInstructions
		}
		replace := replaceInstructions != nil && *replaceInstructions
		input, buildErr := session.BuildBranchSummaryInput(collected.Entries, selected.ContextWindow(), reserve, custom, replace)
		if buildErr != nil {
			return NavigateTreeResult{}, buildErr
		}
		if len(input.Messages) == 0 {
			summaryText = "No content to summarize"
			summaryDetails = json.RawMessage(`{"readFiles":[],"modifiedFiles":[]}`)
		} else {
			var output session.BranchSummaryOutput
			if observable, ok := resolved.(branchSummarizerWithRetryObserver); ok {
				output, err = observable.SummarizeBranchWithRetryObserver(branchCtx, input, func(_ context.Context, retry provider.RetryEvent) {
					s.emitSummarizationRetryFrom(branchCtx, CompactionBranchSummary, "branchSummary", retry)
				})
			} else {
				output, err = resolved.SummarizeBranch(branchCtx, input)
			}
			if err != nil {
				if context.Cause(branchCtx) != nil {
					return NavigateTreeResult{Cancelled: true, Aborted: true}, nil
				}
				return NavigateTreeResult{}, err
			}
			if output.Aborted || context.Cause(branchCtx) != nil {
				return NavigateTreeResult{Cancelled: true, Aborted: true}, nil
			}
			if output.Error != "" {
				return NavigateTreeResult{}, errors.New(output.Error)
			}
			summaryText, summaryDetails, err = session.FinalizeBranchSummary(output.Text, input.FileOps)
			if err != nil {
				return NavigateTreeResult{}, err
			}
			summaryUsage = cloneCompactionUsage(output.Usage)
		}
	}

	var newLeafID *string
	var editorText *string
	if text, editable := session.BranchEditorText(target); editable {
		if parent, hasParent := target.ParentID(); hasParent {
			newLeafID = optionalString(parent)
		}
		editorText = optionalStringPreserveEmpty(text)
	} else {
		newLeafID = optionalString(targetID)
	}
	var summaryEntry *session.Entry
	if summaryText != "" {
		flag := fromExtension
		entry, commitErr := manager.BranchWithSummary(branchCtx, newLeafID, summaryText, summaryDetails, &flag, summaryUsage)
		if commitErr != nil {
			if context.Cause(branchCtx) != nil {
				return NavigateTreeResult{Cancelled: true, Aborted: true}, nil
			}
			return NavigateTreeResult{}, commitErr
		}
		summaryEntry = &entry
		if label != nil && *label != "" {
			if _, err := manager.AppendLabelChange(context.WithoutCancel(branchCtx), entry.ID(), label); err != nil {
				return NavigateTreeResult{}, err
			}
		}
	} else {
		if _, err := manager.NavigateTreePosition(branchCtx, newLeafID, targetID, label); err != nil {
			if context.Cause(branchCtx) != nil {
				return NavigateTreeResult{Cancelled: true, Aborted: true}, nil
			}
			return NavigateTreeResult{}, err
		}
	}
	if err := s.reloadAgentMessagesFromSession(); err != nil {
		return NavigateTreeResult{}, err
	}
	if hook := s.hooks.SessionTree; hook != nil {
		newLeaf, hasNewLeaf := manager.LeafID()
		var actualNewLeaf *string
		if hasNewLeaf {
			actualNewLeaf = optionalString(newLeaf)
		}
		var fromHook *bool
		if summaryText != "" {
			fromHook = &fromExtension
		}
		_ = hook(branchCtx, SessionTreeEvent{NewLeafID: actualNewLeaf, OldLeafID: optionalString(oldLeaf), SummaryEntry: summaryEntry, FromExtension: fromHook})
	}
	return NavigateTreeResult{EditorText: editorText, SummaryEntry: summaryEntry}, nil
}

// EditAndResendWithOptions starts a new branch by replacing one historical user
// turn. The branch stays unchanged until the edited prompt has passed normal
// prompt validation and acquired the single AgentSession run admission. If a
// later preflight stage rejects the prompt, the original leaf is restored before
// the session becomes idle again.
func (s *AgentSession) EditAndResendWithOptions(
	ctx context.Context,
	targetID string,
	prompt string,
	options PromptOptions,
) (Result, error) {
	if s == nil || s.sessionManager == nil {
		return Result{}, fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	target, ok := s.sessionManager.Entry(targetID)
	if !ok {
		return Result{}, treeEntryNotFoundError{id: targetID}
	}
	images, editable := editableUserMessageImages(target)
	if !editable {
		return Result{}, fmt.Errorf("%w: entry %s is not an editable user message", ErrInvalidRun, targetID)
	}

	// Historical messages are resent literally. In particular, a leading slash
	// remains message content instead of unexpectedly invoking a current command.
	expand := false
	options.ExpandPromptTemplates = &expand
	options.StreamingBehavior = ""
	options.Images = images
	return s.runTextWithOptionsAndTransition(ctx, prompt, options, func(run *sessionRun) (func() error, error) {
		oldLeafID, hasOldLeaf := s.sessionManager.LeafID()
		var oldLeaf *string
		if hasOldLeaf {
			oldLeaf = optionalString(oldLeafID)
		}
		rollback := func() error {
			return s.restoreSelectedTreeLeaf(context.WithoutCancel(run.ctx), oldLeaf)
		}
		navigation, err := s.navigateTreeWithRun(run, targetID, NavigateTreeOptions{}, true)
		if err != nil {
			return rollback, err
		}
		if navigation.Cancelled {
			if navigation.Aborted {
				return rollback, fmt.Errorf("%w: edit-and-resend navigation aborted", ErrAgentAborted)
			}
			return rollback, fmt.Errorf("%w: edit-and-resend navigation cancelled", ErrAgentAborted)
		}
		if navigation.EditorText == nil {
			return rollback, fmt.Errorf("%w: entry %s is not editable", ErrInvalidRun, targetID)
		}
		return rollback, nil
	})
}

func editableUserMessageImages(entry session.Entry) ([]llm.ImageBlock, bool) {
	message, ok := entry.Message()
	if !ok || message.Role() != llm.RoleUser {
		return nil, false
	}
	switch message := message.(type) {
	case llm.UserTextMessage:
		return nil, true
	case llm.UserContentMessage:
		var images []llm.ImageBlock
		for _, block := range message.Content() {
			if image, ok := block.(llm.ImageBlock); ok {
				images = append(images, image)
			}
		}
		return images, true
	default:
		return nil, false
	}
}

func (s *AgentSession) restoreSelectedTreeLeaf(ctx context.Context, leafID *string) error {
	manager := s.sessionManager
	currentLeafID, hasCurrentLeaf := manager.LeafID()
	if (leafID == nil && !hasCurrentLeaf) || (leafID != nil && hasCurrentLeaf && currentLeafID == *leafID) {
		return nil
	}
	if _, err := manager.NavigateTreePosition(ctx, leafID, "", nil); err != nil {
		return err
	}
	if err := s.reloadAgentMessagesFromSession(); err != nil {
		return err
	}
	if hook := s.hooks.SessionTree; hook != nil {
		var currentLeaf *string
		if hasCurrentLeaf {
			currentLeaf = optionalString(currentLeafID)
		}
		_ = hook(ctx, SessionTreeEvent{NewLeafID: cloneStringPointer(leafID), OldLeafID: currentLeaf})
	}
	return nil
}

func optionalStringPreserveEmpty(value string) *string { return &value }

func cloneBoolPointer(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneSessionEntries(entries []session.Entry) []session.Entry {
	result := make([]session.Entry, len(entries))
	for index := range entries {
		result[index] = entries[index]
	}
	return result
}

// AbortBranchSummary cancels only an active navigateTree lifecycle.
func (s *AgentSession) AbortBranchSummary() {
	if s == nil {
		return
	}
	s.lifecycleMu.Lock()
	var cancel context.CancelCauseFunc
	if s.run != nil && s.run.branchSummary && s.run.branchCancellation != nil {
		cancel = s.run.branchCancellation.cancel
	}
	s.lifecycleMu.Unlock()
	if cancel != nil {
		cancel(ErrAgentAborted)
	}
}

// SelectLeaf exposes the current Session tree navigation boundary without
// teaching Agent about JSONL. Extensions may cancel before selection and are
// notified after the new branch becomes active.
func (s *AgentSession) SelectLeaf(ctx context.Context, id string) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil tree context", ErrInvalidRun)
	}
	if err := s.rejectIfClosed(); err != nil {
		return err
	}
	manager := s.sessionManager
	run, err := s.admitSessionRun(ctx, false)
	if err != nil {
		return err
	}
	defer s.finishSessionRun(run)
	old, _ := manager.LeafID()
	if old == id {
		return nil
	}
	targetPath, err := manager.BranchPath(id)
	if err != nil {
		return err
	}
	oldPath, _ := manager.BranchPath("")
	commonAncestor, entriesToSummarize := treeNavigationDelta(oldPath, targetPath)
	preparation := TreePreparation{
		TargetID: id, OldLeafID: optionalString(old), CommonAncestorID: optionalString(commonAncestor),
		EntriesToSummarize: entriesToSummarize, UserWantsSummary: false,
	}
	var label string
	if hook := s.hooks.SessionBeforeTree; hook != nil {
		result, err := hook(run.ctx, SessionBeforeTreeEvent{Preparation: preparation})
		if err != nil {
			return err
		}
		if err := result.Cancel.Validate(); err != nil {
			return err
		}
		if result.Cancel.Cancelled() {
			return ErrAgentAborted
		}
		// SelectLeaf is the non-summarizing navigation surface. Summary and
		// instruction overrides remain accurately typed for the future full tree
		// runtime, while label is meaningful for this operation today.
		if result.Label != nil {
			label = *result.Label
		}
	}
	if err := manager.Branch(id); err != nil {
		return err
	}
	if label != "" {
		if _, err := manager.AppendLabelChange(run.ctx, id, &label); err != nil {
			return err
		}
	}
	if err := s.reloadAgentMessagesFromSession(); err != nil {
		return err
	}
	if hook := s.hooks.SessionTree; hook != nil {
		newLeaf, _ := manager.LeafID()
		_ = hook(run.ctx, SessionTreeEvent{OldLeafID: optionalString(old), NewLeafID: optionalString(newLeaf)})
	}
	return nil
}

// ResetLeaf selects the virtual position before all entries and refreshes the
// Agent context from SessionManager. The next prompt becomes a new root.
func (s *AgentSession) ResetLeaf(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil tree context", ErrInvalidRun)
	}
	if err := s.rejectIfClosed(); err != nil {
		return err
	}
	run, err := s.admitSessionRun(ctx, false)
	if err != nil {
		return err
	}
	defer s.finishSessionRun(run)
	old, _ := s.sessionManager.LeafID()
	if err := s.sessionManager.ResetLeaf(); err != nil {
		return err
	}
	if err := s.reloadAgentMessagesFromSession(); err != nil {
		return err
	}
	if hook := s.hooks.SessionTree; hook != nil {
		_ = hook(run.ctx, SessionTreeEvent{OldLeafID: optionalString(old)})
	}
	return nil
}

// CreateBranchedSession asks SessionManager to replace its active durable
// session with the selected path, then refreshes Agent state. Runtime remains
// responsible for replacing the whole AgentSession when product UX requires a
// separate lifecycle object.
func (s *AgentSession) CreateBranchedSession(ctx context.Context, leafID string) (string, bool, error) {
	if ctx == nil {
		return "", false, fmt.Errorf("%w: nil branch context", ErrInvalidRun)
	}
	if err := s.rejectIfClosed(); err != nil {
		return "", false, err
	}
	run, err := s.admitSessionRun(ctx, false)
	if err != nil {
		return "", false, err
	}
	defer s.finishSessionRun(run)
	path, persisted, err := s.sessionManager.CreateBranchedSession(run.ctx, leafID)
	if err != nil {
		return "", false, err
	}
	if err := s.reloadAgentMessagesFromSession(); err != nil {
		return "", false, err
	}
	return path, persisted, nil
}

func treeNavigationDelta(oldPath, targetPath []session.Entry) (string, []session.Entry) {
	oldIndexes := make(map[string]int, len(oldPath))
	for index, entry := range oldPath {
		oldIndexes[entry.ID()] = index
	}
	commonIndex := -1
	commonID := ""
	for index := len(targetPath) - 1; index >= 0; index-- {
		if oldIndex, ok := oldIndexes[targetPath[index].ID()]; ok {
			commonIndex, commonID = oldIndex, targetPath[index].ID()
			break
		}
	}
	entries := append([]session.Entry(nil), oldPath[commonIndex+1:]...)
	return commonID, entries
}
