package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/auth"
	modelcatalog "github.com/cat3399/pi-go/internal/model"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/resource"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
	"github.com/cat3399/pi-go/internal/session"
	"github.com/cat3399/pi-go/internal/tool"
)

const (
	openAIProviderID     = provider.OpenAIProviderID
	openAIResponsesAPI   = provider.OpenAIResponsesAPI
	openAICompletionsAPI = provider.OpenAICompletionsAPI
	agentDirEnvironment  = "PI_CODING_AGENT_DIR"
)

// ProductionConfig contains process-owned inputs and deterministic adapter
// seams. Ordinary users select only the documented CLI/config sources; these
// fields do not create hidden release flags or provider fallbacks.
type ProductionConfig struct {
	WorkingDir  string
	AgentDir    string
	DocsDir     string
	Environment []string

	OpenAIHTTPClient provider.HTTPDoer
	OpenAIClock      provider.Clock
	// Provider-specific clients fall back to OpenAIHTTPClient when omitted so
	// existing embedders can keep one transport seam while tests can isolate
	// dialects independently.
	AzureOpenAIHTTPClient provider.HTTPDoer
	AzureOpenAIClock      provider.Clock
	OpenAICodexHTTPClient provider.HTTPDoer
	OpenAICodexClock      provider.Clock
	AnthropicHTTPClient   provider.HTTPDoer
	AnthropicClock        provider.Clock
	// OpenAIOAuthHTTPClient/BaseURL configure the independent OpenAI Codex
	// OAuth token service. Empty values use auth.openai.com.
	OpenAIOAuthHTTPClient *http.Client
	OpenAIOAuthBaseURL    string
	OpenAIOAuthClock      func() time.Time
	// Anthropic OAuth seams cover Claude Pro/Max token exchange. Login UI is
	// outside production assembly, but stored credentials refresh here.
	AnthropicOAuthHTTPClient *http.Client
	AnthropicOAuthTokenURL   string
	AnthropicOAuthClock      func() time.Time

	SessionID         string
	SessionNow        session.Clock
	NewSessionEntryID session.IDGenerator
	AgentNow          func() time.Time
	SettlementTimeout time.Duration
	// Hooks is the transport-neutral extension contract wired into the same
	// AgentSession path used by injected application dependencies. Loading or
	// discovering extensions remains outside production assembly.
	Hooks agent.Hooks

	BashRunner            tool.Runner
	BashShellPath         string
	BashArtifactDirectory string
	BashMaxOutputLines    int
	BashMaxOutputBytes    int
}

// ProductionPaths is the normalized filesystem scope shared by long-lived
// product surfaces before they open any AgentSession. Resolving these paths is
// read-only; it does not create an agent directory or session.
type ProductionPaths struct {
	WorkingDir string
	AgentDir   string
}

// ResolveProductionPaths exposes production's canonical cwd/agent-dir rules to
// in-process surfaces such as WebUI. This prevents each surface from inventing
// its own PI_CODING_AGENT_DIR or relative-path behavior.
func ResolveProductionPaths(config ProductionConfig) (ProductionPaths, error) {
	workingDir, err := resolveWorkingDirectory(config.WorkingDir)
	if err != nil {
		return ProductionPaths{}, fmt.Errorf("%w: %w", ErrInvalidProductionConfig, err)
	}
	environment := config.Environment
	if environment == nil {
		environment = os.Environ()
	}
	agentDir, err := resolveProductionAgentDir(config.AgentDir, workingDir, environmentMap(environment))
	if err != nil {
		return ProductionPaths{}, err
	}
	return ProductionPaths{WorkingDir: workingDir, AgentDir: agentDir}, nil
}

// RunProduction admits product-facing model/auth flags, resolves the fixed
// OpenAI production configuration, and then executes the exact same lifecycle
// path as Run. Static argument/path assembly happens first; cwd-bound services
// are deliberately constructed only after the session manager is selected.
func RunProduction(
	ctx context.Context,
	config ProductionConfig,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	return runApplication(ctx, args, stdout, stderr, func(
		ctx context.Context,
		parsed options,
	) (runtimeDependencies, error) {
		return assembleProductionRuntime(ctx, config, parsed)
	})
}

func assembleProductionRuntime(
	ctx context.Context,
	config ProductionConfig,
	parsed options,
) (runtimeDependencies, error) {
	if cause := context.Cause(ctx); cause != nil {
		return runtimeDependencies{}, fmt.Errorf("production assembly cancelled: %w", cause)
	}
	paths, err := ResolveProductionPaths(config)
	if err != nil {
		return runtimeDependencies{}, err
	}
	workingDir := paths.WorkingDir
	environment := config.Environment
	if environment == nil {
		environment = os.Environ()
	} else {
		environment = append([]string(nil), environment...)
	}
	ambientEnvironment := environmentMap(environment)
	agentDir := paths.AgentDir
	sessionClock := config.SessionNow
	if sessionClock == nil {
		sessionClock = time.Now
	}
	createdAt := sessionClock()
	if createdAt.IsZero() {
		return runtimeDependencies{}, fmt.Errorf("%w: default session timestamp is zero", ErrInvalidProductionConfig)
	}
	sessionID := config.SessionID
	if sessionID == "" {
		sessionID, err = session.NewSessionID(createdAt)
		if err != nil {
			return runtimeDependencies{}, fmt.Errorf("%w: generate default session ID: %w", ErrInvalidProductionConfig, err)
		}
	}
	if err := validateProductionSessionID(sessionID); err != nil {
		return runtimeDependencies{}, err
	}
	defaultPath := productionSessionPathFactory(agentDir, createdAt, sessionID)
	docsDir, err := resolveProductionDocsDir(config.DocsDir)
	if err != nil {
		return runtimeDependencies{}, err
	}
	plan := productionRuntimePlan{
		config: config, parsed: parsed, agentDir: agentDir, docsDir: docsDir,
		environment: environment, ambientEnvironment: ambientEnvironment,
	}
	return runtimeDependencies{
		workingDir: workingDir, agentDir: agentDir, defaultSessionPath: defaultPath,
		sessionID: sessionID, sessionNow: sessionClock, sessionCreateTime: createdAt,
		newSessionEntryID: config.NewSessionEntryID, factory: plan.create,
	}, nil
}

type productionRuntimePlan struct {
	config             ProductionConfig
	parsed             options
	agentDir           string
	docsDir            string
	environment        []string
	ambientEnvironment map[string]string
}

func (p productionRuntimePlan) toolRuntimeOptions(cwd string, settings modelcatalog.Settings) (productionToolRuntimeOptions, error) {
	shellPath := p.config.BashShellPath
	if shellPath == "" {
		shellPath = settings.ShellPath
	}
	resolvedShellPath, err := resolveProductionShellPath(shellPath)
	if err != nil {
		return productionToolRuntimeOptions{}, err
	}
	autoResizeImages := settings.Images.AutoResizeOrDefault()
	return productionToolRuntimeOptions{
		Bash: tool.BashOptions{
			WorkingDir: cwd, Environment: append([]string(nil), p.environment...), Runner: p.config.BashRunner,
			ShellPath: resolvedShellPath, CommandPrefix: settings.ShellCommandPrefix,
			ArtifactDirectory: p.config.BashArtifactDirectory,
			MaxOutputLines:    p.config.BashMaxOutputLines, MaxOutputBytes: p.config.BashMaxOutputBytes,
		},
		Filesystem: tool.FilesystemOptions{WorkingDir: cwd, AutoResizeImages: &autoResizeImages},
	}, nil
}

func (p productionRuntimePlan) buildToolRuntime(
	cwd string,
	settings modelcatalog.Settings,
) (agent.ToolExecutor, []provider.ToolDefinition, []resource.Tool, agent.StandaloneBashExecutor, error) {
	options, err := p.toolRuntimeOptions(cwd, settings)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return buildProductionToolRuntime(options)
}

func (p productionRuntimePlan) buildStandaloneBash(cwd string, settings modelcatalog.Settings) (agent.StandaloneBashExecutor, error) {
	options, err := p.toolRuntimeOptions(cwd, settings)
	if err != nil {
		return nil, err
	}
	bash, err := tool.NewBash(options.Bash)
	if err != nil {
		return nil, err
	}
	return agent.NewBashExecutor(bash)
}

func resolveProductionShellPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("%w: expand shell path: %w", ErrInvalidProductionConfig, err)
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

func (p productionRuntimePlan) create(ctx context.Context, options agentruntime.CreateOptions) (agentruntime.CreateResult, error) {
	cwd := options.SessionManager.Cwd()
	bootstrapToolOptions, err := p.toolRuntimeOptions(cwd, modelcatalog.Settings{})
	if err != nil {
		return agentruntime.CreateResult{}, err
	}
	_, _, resourceTools, _, err := buildProductionToolRuntime(bootstrapToolOptions)
	if err != nil {
		return agentruntime.CreateResult{}, fmt.Errorf("%w: initialize session tool metadata: %w", ErrInvalidProductionConfig, err)
	}
	activeToolNames := defaultActiveToolNames()
	bootstrapResources, err := resource.New(resource.Config{
		CWD: cwd, AgentDir: p.agentDir,
		Tools: resourceTools, SelectedTools: activeToolNames,
	})
	if err != nil {
		return agentruntime.CreateResult{}, fmt.Errorf("%w: initialize trusted prompt assets: %w", ErrInvalidProductionConfig, err)
	}
	if err := bootstrapResources.Reload(ctx); err != nil {
		return agentruntime.CreateResult{}, fmt.Errorf("%w: load trusted prompt assets: %w", ErrInvalidProductionConfig, err)
	}
	resourceSnapshot, err := bootstrapResources.Snapshot()
	if err != nil {
		return agentruntime.CreateResult{}, fmt.Errorf("%w: trusted prompt assets unavailable", ErrInvalidProductionConfig)
	}
	adapters, err := newProductionProviderAdapters(p.config)
	if err != nil {
		return agentruntime.CreateResult{}, err
	}
	authResolver, err := newProductionProviderAuthResolver(p.agentDir, p.ambientEnvironment, p.config)
	if err != nil {
		return agentruntime.CreateResult{}, err
	}
	catalog, err := modelcatalog.NewRuntime(modelcatalog.Options{
		AgentDir: p.agentDir, WorkingDir: cwd, ProjectTrusted: resourceSnapshot.Trusted,
		Adapters: adapters, AuthResolver: authResolver,
	})
	if err != nil {
		if strings.Contains(err.Error(), "models.json") {
			return agentruntime.CreateResult{}, fmt.Errorf("%w: parse models.json", ErrInvalidProductionConfig)
		}
		return agentruntime.CreateResult{}, fmt.Errorf("%w: %w", ErrInvalidProductionConfig, err)
	}
	snapshot := catalog.Snapshot()
	executor, definitions, resourceTools, standaloneBash, err := p.buildToolRuntime(cwd, snapshot.Settings)
	if err != nil {
		return agentruntime.CreateResult{}, fmt.Errorf("%w: initialize session tool runtime: %w", ErrInvalidProductionConfig, err)
	}
	resources, err := resource.New(resource.Config{
		CWD: cwd, AgentDir: p.agentDir,
		Tools: resourceTools, SelectedTools: activeToolNames,
		SkillPaths:  append([]string(nil), snapshot.Settings.Skills...),
		PromptPaths: append([]string(nil), snapshot.Settings.Prompts...),
	})
	if err != nil {
		return agentruntime.CreateResult{}, fmt.Errorf("%w: initialize configured prompt assets: %w", ErrInvalidProductionConfig, err)
	}
	if err := resources.Reload(ctx); err != nil {
		return agentruntime.CreateResult{}, fmt.Errorf("%w: load configured prompt assets: %w", ErrInvalidProductionConfig, err)
	}
	resourceSnapshot, err = resources.Snapshot()
	if err != nil {
		return agentruntime.CreateResult{}, fmt.Errorf("%w: configured prompt assets unavailable", ErrInvalidProductionConfig)
	}
	activeDefinitions := selectProductionToolDefinitions(definitions, activeToolNames)
	availability := catalog.Availability()
	var explicit *modelcatalog.Model
	var explicitThinking *provider.ThinkingLevel
	var diagnostics []agentruntime.Diagnostic
	if catalogError := catalog.Error(); catalogError != nil {
		diagnostics = append(diagnostics, agentruntime.Diagnostic{
			Kind: agentruntime.DiagnosticError, Message: "Model configuration: " + catalogError.Error(),
		})
	}
	if p.parsed.hasAPIKey && p.parsed.modelID == "" {
		diagnostics = append(diagnostics, agentruntime.Diagnostic{
			Kind:    agentruntime.DiagnosticError,
			Message: "--api-key requires a model to be specified via --model, --provider/--model, or --models",
		})
	}
	if p.parsed.providerID != "" && p.parsed.modelID == "" {
		return agentruntime.CreateResult{}, fmt.Errorf("%w: provider requires a model", ErrInvalidArguments)
	}
	if p.parsed.modelID != "" {
		resolved := modelcatalog.ResolveCLIModel(modelcatalog.CLIModelOptions{
			Provider: p.parsed.providerID, Model: p.parsed.modelID, AllModels: snapshot.Models,
			HasConfiguredAuth: availability.HasConfiguredAuth, HasConfiguredModelAuth: availability.HasConfiguredModelAuth,
		})
		if resolved.Warning != "" {
			diagnostics = append(diagnostics, agentruntime.Diagnostic{Kind: agentruntime.DiagnosticWarning, Message: resolved.Warning})
		}
		if resolved.Error != "" || resolved.Model == nil {
			return agentruntime.CreateResult{}, fmt.Errorf("%w: %s", ErrInvalidArguments, resolved.Error)
		}
		if !availability.SupportsRoute(*resolved.Model) {
			return agentruntime.CreateResult{}, fmt.Errorf("%w: selected provider/API is not supported by this production assembly", ErrUnsupportedProductionValue)
		}
		explicit = resolved.Model
		explicitThinking = resolved.ThinkingLevel
	}
	if p.parsed.hasAPIKey && explicit != nil {
		if err := catalog.SetRuntimeAPIKey(explicit.Provider, p.parsed.apiKey); err != nil {
			return agentruntime.CreateResult{}, productionAuthError(err)
		}
	}
	providersToCheck := snapshot.Providers
	if explicit != nil {
		providersToCheck = []string{explicit.Provider}
	}
	for _, providerID := range providersToCheck {
		if _, err := catalog.CheckAuth(ctx, providerID); err != nil {
			return agentruntime.CreateResult{}, productionAuthError(err)
		}
	}
	// Selection is best-effort across unrelated providers. A broken credential
	// for one provider must not hide a CLI-overridden provider; explicit startup
	// checks above still surface errors for the provider actually being used.
	availableModels := modelcatalog.FilterAvailableModels(snapshot.Models, availability)
	scope := modelcatalog.ResolveModelScope(snapshot.Settings.EnabledModels, availableModels)
	for _, diagnostic := range scope.Diagnostics {
		diagnostics = append(diagnostics, agentruntime.Diagnostic{Kind: agentruntime.DiagnosticWarning, Message: diagnostic.Message})
	}
	services := &agentruntime.Services{
		CWD: cwd, AgentDir: p.agentDir, ModelRuntime: catalog, ResourceService: resources,
		ResolveResourcePaths: func() ([]string, []string) {
			settings := catalog.Snapshot().Settings
			return append([]string(nil), settings.Skills...), append([]string(nil), settings.Prompts...)
		},
		AuthRuntime: authResolver.runtime, Provider: catalog, Tool: executor,
		Tools: append([]provider.ToolDefinition(nil), definitions...), StandaloneBash: standaloneBash,
		ReloadTools: func(_ context.Context) (agent.ToolRuntime, error) {
			reloadedExecutor, reloadedDefinitions, reloadedResources, reloadedStandalone, reloadErr := p.buildToolRuntime(cwd, catalog.Snapshot().Settings)
			if reloadErr != nil {
				return agent.ToolRuntime{}, reloadErr
			}
			return agent.ToolRuntime{
				Executor: reloadedExecutor, Tools: reloadedDefinitions,
				Metadata: productionToolMetadata(reloadedResources), StandaloneBash: reloadedStandalone,
			}, nil
		},
	}
	stream, err := productionProviderStreamOptions(snapshot.Settings, options.SessionManager.SessionID())
	if err != nil {
		return agentruntime.CreateResult{}, err
	}
	compactionEnabled := snapshot.Settings.Compaction.EnabledOrDefault()
	retryPolicy, err := productionRetryPolicy(snapshot.Settings.Retry)
	if err != nil {
		return agentruntime.CreateResult{}, err
	}
	runtimeHooks := p.config.Hooks
	for _, diagnostic := range diagnostics {
		if diagnostic.Kind == agentruntime.DiagnosticError {
			// main.ts reports blocking runtime diagnostics before a mode binds
			// extension lifecycle hooks. Keep startup and disposal hook-free.
			runtimeHooks = agent.Hooks{}
			break
		}
	}
	requirePromptAccess := func(accessCtx context.Context, selected provider.Model) error {
		resolved, authErr := catalog.GetAuth(accessCtx, selected, modelcatalog.AuthOverrides{})
		if authErr != nil {
			return authErr
		}
		if resolved == nil {
			return &agent.ModelAccessError{Message: agentruntime.FormatNoAPIKeyFoundMessage(selected.Provider(), p.docsDir)}
		}
		return nil
	}
	created, err := agentruntime.CreateAgentSession(ctx, agentruntime.SessionFactoryOptions{
		Services: services, Provider: catalog, SessionManager: options.SessionManager,
		AllModels: snapshot.Models, Availability: availability, ExplicitModel: explicit,
		ExplicitThinkingLevel: explicitThinking, ScopedModels: scope.ScopedModels, Settings: snapshot.Settings,
		BaseConfig: agent.SessionConfig{
			SystemPrompt: resourceSnapshot.SystemPrompt, Tool: executor, Tools: activeDefinitions,
			AllTools: definitions, ActiveToolNames: activeToolNames,
			ToolMetadata: productionToolMetadata(resourceTools), Stream: stream,
			ResolveBashCommandPrefix: func() string { return catalog.Snapshot().Settings.ShellCommandPrefix },
			ResolveStandaloneBash: func(resolveCtx context.Context) (agent.StandaloneBashExecutor, error) {
				if cause := context.Cause(resolveCtx); cause != nil {
					return nil, cause
				}
				return p.buildStandaloneBash(cwd, catalog.Snapshot().Settings)
			},
			CompactionEnabled: &compactionEnabled,
			ContextReserve:    snapshot.Settings.Compaction.ReserveTokensOrDefault(),
			KeepRecentTokens:  snapshot.Settings.Compaction.KeepRecentTokensOrDefault(),
			ContextReserveSet: true, KeepRecentTokensSet: true,
			BranchSummaryReserveTokens: snapshot.Settings.BranchSummary.ReserveTokensOrDefault(), BranchSummaryReserveSet: true,
			Retry: retryPolicy,
			ResolveStreamOptions: func(turnCtx context.Context, _ provider.Model) (provider.StreamOptions, error) {
				// SettingsManager is live in upstream: transport and provider retry/
				// timeout values are read for every actual provider call, not frozen
				// when the session is created. Authentication is resolved by Models
				// after AgentSession applies later explicit turn/request overlays.
				if cause := context.Cause(turnCtx); cause != nil {
					return provider.StreamOptions{}, cause
				}
				turnStream, streamErr := productionProviderStreamOptions(catalog.Snapshot().Settings, "")
				if streamErr != nil {
					return provider.StreamOptions{}, streamErr
				}
				return turnStream, nil
			},
			ResolveSummarizer: func(resolveCtx context.Context, request agent.SummarizerResolveRequest) (session.Summarizer, error) {
				if accessErr := requirePromptAccess(resolveCtx, request.Model); accessErr != nil {
					return nil, accessErr
				}
				return provider.NewContextSummarizerWithOptions(catalog, request.Model, p.config.AgentNow, provider.ContextSummarizerOptions{
					ThinkingLevel: request.ThinkingLevel,
					Stream:        request.Stream,
					Retry:         request.Retry,
				})
			},
			ResolveBranchSummarizer: func(resolveCtx context.Context, request agent.SummarizerResolveRequest) (session.BranchSummarizer, error) {
				if accessErr := requirePromptAccess(resolveCtx, request.Model); accessErr != nil {
					return nil, accessErr
				}
				return provider.NewContextSummarizerWithOptions(catalog, request.Model, p.config.AgentNow, provider.ContextSummarizerOptions{
					ThinkingLevel: request.ThinkingLevel,
					Stream:        request.Stream,
					Retry:         request.Retry,
				})
			},
			ValidateModelAccess: func(accessCtx context.Context, selected provider.Model) error {
				return requirePromptAccess(accessCtx, selected)
			},
			ValidateModelSelection: func(accessCtx context.Context, selected provider.Model) error {
				resolved, authErr := catalog.GetAuth(accessCtx, selected, modelcatalog.AuthOverrides{})
				if authErr != nil {
					return authErr
				}
				if resolved == nil {
					return &agent.ModelAccessError{Message: "No API key for " + selected.Provider() + "/" + selected.ID()}
				}
				return nil
			},
			Hooks: runtimeHooks, Now: p.config.AgentNow, SettlementTimeout: p.config.SettlementTimeout,
		},
		SessionStartEvent: options.SessionStartEvent, Diagnostics: diagnostics, DocsDir: p.docsDir,
	})
	return created, err
}

func productionRetryPolicy(settings modelcatalog.RetrySettings) (agent.RetryPolicy, error) {
	maxRetries := settings.MaxRetriesOrDefault()
	if maxRetries > uint64(^uint32(0)-1) {
		return agent.RetryPolicy{}, fmt.Errorf("%w: retry.maxRetries is too large", ErrInvalidProductionConfig)
	}
	baseDelayMS := settings.BaseDelayMSOrDefault()
	if baseDelayMS > uint64(time.Duration(1<<63-1)/time.Millisecond) {
		return agent.RetryPolicy{}, fmt.Errorf("%w: retry.baseDelayMs is too large", ErrInvalidProductionConfig)
	}
	return agent.RetryPolicy{MaxAttempts: uint32(maxRetries) + 1, InitialDelay: time.Duration(baseDelayMS) * time.Millisecond}, nil
}

func productionProviderStreamOptions(settings modelcatalog.Settings, sessionID string) (provider.StreamOptions, error) {
	httpIdleTimeoutMS := settings.HTTPIdleTimeoutMSOrDefault()
	// Provider SDKs treat timeout=0 as immediate expiry; upstream maps the
	// user-facing disabled value to max int32 milliseconds.
	effectiveTimeoutMS := httpIdleTimeoutMS
	if effectiveTimeoutMS == 0 {
		effectiveTimeoutMS = uint64(1<<31 - 1)
	}
	if settings.Retry.Provider.TimeoutMS != nil {
		effectiveTimeoutMS = *settings.Retry.Provider.TimeoutMS
	}
	options := provider.StreamOptions{SessionID: sessionID, Transport: settings.TransportOrDefault(), TimeoutMS: &effectiveTimeoutMS}
	if settings.WebsocketConnectTimeoutMS != nil {
		value := *settings.WebsocketConnectTimeoutMS
		options.WebsocketConnectTimeoutMS = &value
	}
	if settings.Retry.Provider.MaxRetries != nil {
		if *settings.Retry.Provider.MaxRetries > uint64(^uint32(0)) {
			return provider.StreamOptions{}, fmt.Errorf("%w: retry.provider.maxRetries is too large", ErrInvalidProductionConfig)
		}
		value := uint32(*settings.Retry.Provider.MaxRetries)
		options.MaxRetries = &value
	}
	maxRetryDelayMS := settings.Retry.Provider.MaxRetryDelayMSOrDefault()
	options.MaxRetryDelayMS = &maxRetryDelayMS
	return options, nil
}

func clockBeginningWith(first time.Time, subsequent session.Clock) session.Clock {
	var delivered atomic.Bool
	return func() time.Time {
		if delivered.CompareAndSwap(false, true) {
			return first
		}
		return subsequent()
	}
}

func newProductionProviderAdapters(config ProductionConfig) (map[string]provider.Streamer, error) {
	azureClient := config.AzureOpenAIHTTPClient
	if azureClient == nil {
		azureClient = config.OpenAIHTTPClient
	}
	azureClock := config.AzureOpenAIClock
	if azureClock == nil {
		azureClock = config.OpenAIClock
	}
	codexClient := config.OpenAICodexHTTPClient
	if codexClient == nil {
		codexClient = config.OpenAIHTTPClient
	}
	codeClock := config.OpenAICodexClock
	if codeClock == nil {
		codeClock = config.OpenAIClock
	}
	anthropicClient := config.AnthropicHTTPClient
	if anthropicClient == nil {
		anthropicClient = config.OpenAIHTTPClient
	}
	anthropicClock := config.AnthropicClock
	if anthropicClock == nil {
		anthropicClock = config.OpenAIClock
	}
	responses, err := provider.NewOpenAIResponsesProvider(provider.OpenAIResponsesConfig{Client: config.OpenAIHTTPClient, Clock: config.OpenAIClock})
	if err != nil {
		return nil, fmt.Errorf("%w: initialize OpenAI Responses provider: %w", ErrInvalidProductionConfig, err)
	}
	azure, err := provider.NewAzureOpenAIResponsesProvider(provider.AzureOpenAIResponsesConfig{Client: azureClient, Clock: azureClock})
	if err != nil {
		return nil, fmt.Errorf("%w: initialize Azure OpenAI Responses provider: %w", ErrInvalidProductionConfig, err)
	}
	completions, err := provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{Client: config.OpenAIHTTPClient, Clock: config.OpenAIClock})
	if err != nil {
		return nil, fmt.Errorf("%w: initialize OpenAI Chat Completions provider: %w", ErrInvalidProductionConfig, err)
	}
	codex, err := provider.NewOpenAICodexResponsesProvider(provider.OpenAICodexResponsesConfig{Client: codexClient, Clock: codeClock})
	if err != nil {
		return nil, fmt.Errorf("%w: initialize OpenAI Codex provider: %w", ErrInvalidProductionConfig, err)
	}
	anthropic, err := provider.NewAnthropicProvider(provider.AnthropicConfig{Client: anthropicClient, Clock: anthropicClock})
	if err != nil {
		return nil, fmt.Errorf("%w: initialize Anthropic provider: %w", ErrInvalidProductionConfig, err)
	}
	return map[string]provider.Streamer{
		provider.OpenAIResponsesAPI: responses, provider.AzureOpenAIResponsesAPI: azure,
		provider.OpenAICompletionsAPI:    completions,
		provider.OpenAICodexResponsesAPI: codex, provider.AnthropicMessagesAPI: anthropic,
	}, nil
}

type productionProviderAuthResolver struct {
	runtime            *auth.Runtime
	ambientEnvironment map[string]string
	codexOAuth         *auth.OpenAICodexOAuth
	codexClock         func() time.Time
	anthropicOAuth     *auth.AnthropicOAuth
	anthropicClock     func() time.Time
}

func newProductionProviderAuthResolver(agentDir string, ambientEnvironment map[string]string, config ProductionConfig) (*productionProviderAuthResolver, error) {
	store, err := auth.NewStore(auth.Options{Path: filepath.Join(agentDir, "auth.json")})
	if err != nil {
		return nil, fmt.Errorf("%w: initialize auth storage", ErrInvalidProductionConfig)
	}
	codexOAuth, err := auth.NewOpenAICodexOAuth(auth.OpenAICodexOAuthConfig{
		HTTPClient: config.OpenAIOAuthHTTPClient, AuthBaseURL: config.OpenAIOAuthBaseURL, Clock: config.OpenAIOAuthClock,
	})
	if err != nil {
		return nil, productionAuthError(err)
	}
	anthropicOAuth, err := auth.NewAnthropicOAuth(auth.AnthropicOAuthConfig{
		HTTPClient: config.AnthropicOAuthHTTPClient, TokenURL: config.AnthropicOAuthTokenURL,
		Clock: config.AnthropicOAuthClock, CallbackHost: ambientEnvironment["PI_OAUTH_CALLBACK_HOST"],
	})
	if err != nil {
		return nil, productionAuthError(err)
	}
	return &productionProviderAuthResolver{
		runtime: auth.NewRuntime(store), ambientEnvironment: cloneStringMap(ambientEnvironment),
		codexOAuth: codexOAuth, codexClock: config.OpenAIOAuthClock,
		anthropicOAuth: anthropicOAuth, anthropicClock: config.AnthropicOAuthClock,
	}, nil
}

func (r *productionProviderAuthResolver) Check(ctx context.Context, configured modelcatalog.ProviderConfig) (*modelcatalog.AuthCheck, error) {
	providerID := configured.ID
	credential, exists, err := r.runtime.Read(ctx, providerID)
	if err != nil {
		return nil, err
	}
	if exists {
		switch credential.Type {
		case "api_key":
			return &modelcatalog.AuthCheck{Source: "stored credential", Type: "api_key"}, nil
		case "oauth":
			if providerID == auth.OpenAICodexProviderID || providerID == auth.AnthropicProviderID {
				return &modelcatalog.AuthCheck{Source: "OAuth", Type: "oauth"}, nil
			}
			return nil, nil
		default:
			return nil, &auth.Error{Kind: auth.KindUnsupported, Operation: "check stored credential", Provider: providerID, Cause: auth.ErrCredentialType}
		}
	}
	if configured.ConfiguredAPIKey != nil {
		if auth.ValueConfigured(*configured.ConfiguredAPIKey, nil, r.ambientEnvironment) {
			return &modelcatalog.AuthCheck{Source: "configured API key", Type: "api_key"}, nil
		}
		return nil, nil
	}
	for _, name := range configured.APIKeyEnvironment {
		if strings.TrimSpace(r.ambientEnvironment[name]) != "" {
			return &modelcatalog.AuthCheck{Source: name, Type: "api_key"}, nil
		}
	}
	if configured.Keyless {
		return &modelcatalog.AuthCheck{Source: "keyless provider", Type: "api_key"}, nil
	}
	return nil, nil
}

func (r *productionProviderAuthResolver) ReadCredential(ctx context.Context, configured modelcatalog.ProviderConfig) (*modelcatalog.ProviderCredential, error) {
	credential, exists, err := r.runtime.Read(ctx, configured.ID)
	if err != nil || !exists {
		return nil, err
	}
	switch credential.Type {
	case "api_key":
		return &modelcatalog.ProviderCredential{Type: "api_key", Key: credential.Key, Env: cloneStringMap(credential.Env)}, nil
	case "oauth":
		extra := make(map[string]json.RawMessage, len(credential.OAuth.Extra))
		for key, raw := range credential.OAuth.Extra {
			extra[key] = append(json.RawMessage(nil), raw...)
		}
		return &modelcatalog.ProviderCredential{
			Type: "oauth", Access: credential.OAuth.Access, Refresh: credential.OAuth.Refresh,
			Expires: credential.OAuth.Expires, AccountID: credential.OAuth.AccountID, Extra: extra,
		}, nil
	default:
		return nil, &auth.Error{Kind: auth.KindUnsupported, Operation: "read stored credential", Provider: configured.ID, Cause: auth.ErrCredentialType}
	}
}

func (r *productionProviderAuthResolver) CheckModel(ctx context.Context, providerConfig modelcatalog.ProviderConfig, candidate modelcatalog.Model) (*modelcatalog.AuthCheck, error) {
	configured, err := r.Check(ctx, providerConfig)
	if err != nil || configured != nil {
		return configured, err
	}
	if providerConfig.AuthHeader != nil && *providerConfig.AuthHeader {
		return nil, nil
	}
	selected, err := candidate.Ref()
	if err != nil {
		return nil, err
	}
	providerHeaders, err := auth.ResolveHeaders(ctx, providerConfig.Headers, "provider "+candidate.Provider, nil, r.ambientEnvironment)
	if err != nil {
		return nil, err
	}
	modelHeaders, err := auth.ResolveHeaders(ctx, selected.Headers(), "model "+candidate.Provider+"/"+candidate.ID, nil, r.ambientEnvironment)
	if err != nil {
		return nil, err
	}
	if authHeadersAuthorizeModel(candidate.API, mergeProductionHeaders(providerHeaders, modelHeaders)) {
		return &modelcatalog.AuthCheck{Source: "configured headers", Type: "api_key"}, nil
	}
	return nil, nil
}

func (r *productionProviderAuthResolver) Resolve(ctx context.Context, configured modelcatalog.ProviderConfig, selected provider.Model, overrides modelcatalog.AuthOverrides) (*modelcatalog.AuthResult, error) {
	var result auth.OpenAIAuthResult
	var err error
	providerID := selected.Provider()
	if providerID == "" {
		providerID = configured.ID
	}
	api := selected.API()
	if api == "" {
		api = configured.API
	}
	ambient := cloneStringMap(r.ambientEnvironment)
	if ambient == nil {
		ambient = make(map[string]string)
	}
	for name, value := range overrides.Env {
		ambient[name] = value
	}
	var explicit *string
	if overrides.APIKey != "" {
		value := overrides.APIKey
		explicit = &value
	}
	switch providerID {
	case auth.OpenAIProviderID:
		result, err = auth.ResolveOpenAIAuth(ctx, r.runtime, explicit, configured.ConfiguredAPIKey, ambient, auth.OpenAIResolveOptions{})
	case auth.OpenAICodexProviderID:
		result, err = auth.ResolveOpenAICodexAuth(ctx, r.runtime, explicit, configured.ConfiguredAPIKey, ambient, auth.OpenAIResolveOptions{OAuth: r.codexOAuth, Clock: r.codexClock})
	case auth.AnthropicProviderID:
		result, err = auth.ResolveAnthropicAuth(ctx, r.runtime, explicit, configured.ConfiguredAPIKey, ambient, auth.AnthropicResolveOptions{OAuth: r.anthropicOAuth, Clock: r.anthropicClock})
	default:
		configuredKey := configured.ConfiguredAPIKey
		if configuredKey == nil {
			for _, name := range configured.APIKeyEnvironment {
				if ambient[name] != "" {
					value := "$" + name
					configuredKey = &value
					break
				}
			}
		}
		result, err = auth.ResolveProviderAPIKey(ctx, r.runtime, providerID, explicit, configuredKey, ambient)
	}
	if err != nil {
		var typed *auth.Error
		if auth.IsKind(err, auth.KindNotConfigured) && errors.As(err, &typed) && isMissingProviderCredentialOperation(typed.Operation) {
			// pi's model registry still resolves provider/model headers when the
			// provider credential resolver has no API key. This is required for
			// gateways whose Authorization (or cf-aig-authorization) header owns
			// authentication independently of an SDK-style apiKey.
			result = auth.OpenAIAuthResult{}
			err = nil
		} else {
			return nil, productionAuthError(err)
		}
	}
	if result.APIKey != "" {
		key, validationErr := validateResolvedAPIKey(result.APIKey, "resolved "+providerID+" API key")
		if validationErr != nil {
			return nil, validationErr
		}
		result.APIKey = key
	}
	headerEnvironment := cloneStringMap(result.Env)
	if headerEnvironment == nil {
		headerEnvironment = make(map[string]string)
	}
	for name, value := range overrides.Env {
		headerEnvironment[name] = value
	}
	providerHeaders, headerErr := auth.ResolveHeaders(ctx, configured.Headers, "provider "+providerID, headerEnvironment, ambient)
	if headerErr != nil {
		return nil, productionAuthError(headerErr)
	}
	result.Headers = mergeProductionHeaders(result.Headers, providerHeaders)
	if configured.AuthHeader != nil && *configured.AuthHeader {
		if result.APIKey == "" {
			return nil, productionAuthError(&auth.Error{Kind: auth.KindNotConfigured, Operation: "resolve authHeader API key", Provider: providerID})
		}
		result.Headers = mergeProductionHeaders(result.Headers, map[string]string{"Authorization": "Bearer " + result.APIKey})
	}
	if selected.Provider() != "" {
		modelHeaders, headerErr := auth.ResolveHeaders(ctx, selected.Headers(), "model "+providerID+"/"+selected.ID(), headerEnvironment, ambient)
		if headerErr != nil {
			return nil, productionAuthError(headerErr)
		}
		result.Headers = mergeProductionHeaders(result.Headers, modelHeaders)
	}
	if result.APIKey == "" && !configured.Keyless && !authHeadersAuthorizeModel(api, result.Headers) {
		return nil, nil
	}
	authType := "api_key"
	if result.Source == "OAuth" {
		authType = "oauth"
	}
	return &modelcatalog.AuthResult{
		APIKey: result.APIKey, Headers: result.Headers, Env: productionProviderEnv(headerEnvironment, ambient),
		Source: result.Source, Type: authType,
	}, nil
}

func (r *productionProviderAuthResolver) SetRuntimeAPIKey(providerID, apiKey string) error {
	return r.runtime.SetAPIKey(providerID, apiKey)
}

func (r *productionProviderAuthResolver) RemoveRuntimeAPIKey(providerID string) {
	r.runtime.RemoveAPIKey(providerID)
}

func (r *productionProviderAuthResolver) Logout(ctx context.Context, providerID string) error {
	return r.runtime.Delete(ctx, providerID)
}

func isMissingProviderCredentialOperation(operation string) bool {
	switch operation {
	case "resolve OpenAI API key", "resolve OpenAI Codex credential", "resolve Anthropic credential", "resolve provider API key":
		return true
	default:
		return false
	}
}

func mergeProductionHeaders(groups ...map[string]string) map[string]string {
	result := map[string]string{}
	for _, group := range groups {
		for name, value := range group {
			for existing := range result {
				if strings.EqualFold(existing, name) {
					delete(result, existing)
				}
			}
			result[name] = value
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func authHeadersAuthorizeModel(api string, headers map[string]string) bool {
	for name, value := range headers {
		if strings.TrimSpace(value) == "" {
			continue
		}
		switch api {
		case modelcatalog.AnthropicMessagesAPI:
			if strings.EqualFold(name, "authorization") || strings.EqualFold(name, "x-api-key") || strings.EqualFold(name, "cf-aig-authorization") {
				return true
			}
		case modelcatalog.OpenAIResponsesAPI, modelcatalog.OpenAICompletionsAPI:
			if strings.EqualFold(name, "authorization") || strings.EqualFold(name, "cf-aig-authorization") {
				return true
			}
		case modelcatalog.AzureOpenAIResponsesAPI:
			if strings.EqualFold(name, "api-key") {
				return true
			}
		}
	}
	return false
}

func productionProviderEnv(authEnv, ambient map[string]string) map[string]string {
	result := cloneStringMap(authEnv)
	if result == nil {
		result = make(map[string]string)
	}
	for _, name := range []string{
		"PI_CACHE_RETENTION",
		"AZURE_OPENAI_API_VERSION",
		"AZURE_OPENAI_BASE_URL",
		"AZURE_OPENAI_RESOURCE_NAME",
		"AZURE_OPENAI_DEPLOYMENT_NAME_MAP",
	} {
		if _, explicit := result[name]; !explicit && ambient[name] != "" {
			result[name] = ambient[name]
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// productionAuthError maps service categories to the existing product error
// vocabulary without including the rejected value, raw file bytes, or a key.
func productionAuthError(err error) error {
	var typed *auth.Error
	_ = errors.As(err, &typed)
	providerID := "provider"
	if typed != nil && typed.Provider != "" {
		providerID = typed.Provider
	}
	description := providerID + " credential"
	if typed != nil && strings.Contains(typed.Operation, "stored") {
		description = "stored " + providerID + " credential"
	} else if typed != nil && strings.Contains(typed.Operation, "configured") {
		description = "configured " + providerID + " credential"
	}
	switch {
	case errors.Is(err, auth.ErrCredentialType):
		return fmt.Errorf("%w: auth.json credential type is unsupported for %s", ErrUnsupportedProductionValue, providerID)
	case errors.Is(err, auth.ErrCommandFailed):
		return fmt.Errorf("%w: cannot resolve command-backed %s", ErrInvalidProductionConfig, description)
	case errors.Is(err, auth.ErrPersistentAuthUnavailable):
		return fmt.Errorf("%w: persistent auth storage is unavailable on this platform", ErrUnsupportedProductionValue)
	case auth.IsKind(err, auth.KindNotConfigured):
		return fmt.Errorf("%w: %s references missing environment variable", ErrInvalidProductionConfig, description)
	case auth.IsKind(err, auth.KindPermission):
		return fmt.Errorf("%w: auth.json credential file permissions are unsafe", ErrInvalidProductionConfig)
	case auth.IsKind(err, auth.KindMalformed):
		return fmt.Errorf("%w: auth.json is malformed", ErrInvalidProductionConfig)
	case auth.IsKind(err, auth.KindCancelled):
		return fmt.Errorf("%w: resolve %s cancelled", ErrInvalidProductionConfig, description)
	case auth.IsKind(err, auth.KindTimeout):
		return fmt.Errorf("%w: resolve %s timed out", ErrInvalidProductionConfig, description)
	case auth.IsKind(err, auth.KindOAuth):
		return fmt.Errorf("%w: OAuth failed for %s", ErrInvalidProductionConfig, providerID)
	default:
		return fmt.Errorf("%w: cannot resolve %s", ErrInvalidProductionConfig, description)
	}
}

func validateResolvedAPIKey(value string, description string) (string, error) {
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%w: %s is empty or invalid", ErrInvalidProductionConfig, description)
	}
	if strings.ContainsFunc(value, unicode.IsControl) {
		return "", fmt.Errorf("%w: %s contains a control character", ErrInvalidProductionConfig, description)
	}
	return value, nil
}

func resolveProductionAgentDir(
	explicit string,
	workingDir string,
	environment map[string]string,
) (string, error) {
	path := explicit
	if path == "" {
		path = environment[agentDirEnvironment]
	}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("%w: resolve home directory: %w", ErrInvalidProductionConfig, err)
		}
		path = filepath.Join(home, ".pi", "agent")
	} else if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("%w: expand agent directory: %w", ErrInvalidProductionConfig, err)
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, path[2:])
		}
	}
	if !utf8.ValidString(path) || strings.TrimSpace(path) == "" || strings.IndexByte(path, 0) >= 0 {
		return "", fmt.Errorf("%w: agent directory must be a non-empty valid path", ErrInvalidProductionConfig)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workingDir, path)
	}
	return filepath.Clean(path), nil
}

func resolveProductionDocsDir(explicit string) (string, error) {
	path := explicit
	if path == "" {
		executable, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("%w: resolve executable for docs directory: %w", ErrInvalidProductionConfig, err)
		}
		path = filepath.Join(filepath.Dir(executable), "docs")
	}
	if !utf8.ValidString(path) || strings.TrimSpace(path) == "" || strings.IndexByte(path, 0) >= 0 {
		return "", fmt.Errorf("%w: docs directory must be a non-empty valid path", ErrInvalidProductionConfig)
	}
	resolved, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: resolve docs directory: %w", ErrInvalidProductionConfig, err)
	}
	return filepath.Clean(resolved), nil
}

func validateProductionSessionID(value string) error {
	if err := session.ValidateSessionID(value); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidProductionConfig, err)
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '-' || character == '_' || character == '.' {
			continue
		}
		return fmt.Errorf("%w: session ID cannot be used in a default filename", ErrInvalidProductionConfig)
	}
	return nil
}

func productionSessionPathFactory(agentDir string, createdAt time.Time, sessionID string) SessionPathFactory {
	fileTimestamp := createdAt.UTC().Truncate(time.Millisecond).Format("2006-01-02T15-04-05.000Z")
	fileTimestamp = strings.ReplaceAll(fileTimestamp, ".", "-")
	fileName := fileTimestamp + "_" + sessionID + ".jsonl"
	return func(workingDir string) (string, error) {
		resolvedWorkingDir, err := filepath.Abs(workingDir)
		if err != nil {
			return "", err
		}
		encoded := filepath.Clean(resolvedWorkingDir)
		if len(encoded) > 0 && (encoded[0] == '/' || encoded[0] == '\\') {
			encoded = encoded[1:]
		}
		encoded = strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(encoded)
		projectDirectory := "--" + encoded + "--"
		return filepath.Join(agentDir, "sessions", projectDirectory, fileName), nil
	}
}
