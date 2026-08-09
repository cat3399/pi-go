package app

import (
	"context"
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
	defaultOpenAIModel   = "gpt-5.5"
	agentDirEnvironment  = "PI_CODING_AGENT_DIR"
	openAIKeyEnvironment = "OPENAI_API_KEY"
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
	workingDir, err := resolveWorkingDirectory(config.WorkingDir)
	if err != nil {
		return runtimeDependencies{}, fmt.Errorf("%w: %w", ErrInvalidProductionConfig, err)
	}
	environment := config.Environment
	if environment == nil {
		environment = os.Environ()
	} else {
		environment = append([]string(nil), environment...)
	}
	ambientEnvironment := environmentMap(environment)
	agentDir, err := resolveProductionAgentDir(config.AgentDir, workingDir, ambientEnvironment)
	if err != nil {
		return runtimeDependencies{}, err
	}
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

func (p productionRuntimePlan) create(ctx context.Context, options agentruntime.CreateOptions) (agentruntime.CreateResult, error) {
	cwd := options.SessionManager.Cwd()
	toolOptions := tool.BashOptions{
		WorkingDir: cwd, Environment: append([]string(nil), p.environment...), Runner: p.config.BashRunner,
		ShellPath: p.config.BashShellPath, ArtifactDirectory: p.config.BashArtifactDirectory,
		MaxOutputLines: p.config.BashMaxOutputLines, MaxOutputBytes: p.config.BashMaxOutputBytes,
	}
	executor, definitions, resourceTools, err := buildProductionToolRuntime(toolOptions)
	if err != nil {
		return agentruntime.CreateResult{}, fmt.Errorf("%w: initialize session tool runtime: %w", ErrInvalidProductionConfig, err)
	}
	activeToolNames := defaultActiveToolNames()
	activeDefinitions := selectProductionToolDefinitions(definitions, activeToolNames)
	resources, err := resource.New(resource.Config{
		CWD: cwd, AgentDir: p.agentDir,
		Tools: resourceTools, SelectedTools: activeToolNames,
	})
	if err != nil {
		return agentruntime.CreateResult{}, fmt.Errorf("%w: initialize trusted prompt assets: %w", ErrInvalidProductionConfig, err)
	}
	if err := resources.Reload(ctx); err != nil {
		return agentruntime.CreateResult{}, fmt.Errorf("%w: load trusted prompt assets: %w", ErrInvalidProductionConfig, err)
	}
	resourceSnapshot, err := resources.Snapshot()
	if err != nil {
		return agentruntime.CreateResult{}, fmt.Errorf("%w: trusted prompt assets unavailable", ErrInvalidProductionConfig)
	}
	catalog, err := modelcatalog.NewRuntime(modelcatalog.Options{AgentDir: p.agentDir, WorkingDir: cwd, ProjectTrusted: resourceSnapshot.Trusted})
	if err != nil {
		if strings.Contains(err.Error(), "models.json") {
			return agentruntime.CreateResult{}, fmt.Errorf("%w: parse models.json", ErrInvalidProductionConfig)
		}
		return agentruntime.CreateResult{}, fmt.Errorf("%w: %w", ErrInvalidProductionConfig, err)
	}
	snapshot := catalog.Snapshot()
	router, err := newProductionProviderRouter(snapshot, p.config)
	if err != nil {
		return agentruntime.CreateResult{}, err
	}
	authResolver, err := newProductionProviderAuthResolver(p.agentDir, catalog, p.ambientEnvironment, p.config)
	if err != nil {
		return agentruntime.CreateResult{}, err
	}
	availability := modelcatalog.Availability{
		HasConfiguredAuth: func(providerID string) bool {
			configured, checkErr := authResolver.check(context.Background(), providerID)
			return checkErr == nil && configured
		},
		HasConfiguredModelAuth: func(candidate modelcatalog.Model) bool {
			configured, checkErr := authResolver.checkModel(context.Background(), candidate)
			return checkErr == nil && configured
		},
		SupportsRoute: func(candidate modelcatalog.Model) bool {
			if catalog.ValidateRoute(candidate) != nil {
				return false
			}
			ref, refErr := candidate.Ref()
			return refErr == nil && router.SupportsModel(ref)
		},
	}
	var explicit *modelcatalog.Model
	var explicitThinking *provider.ThinkingLevel
	var diagnostics []agentruntime.Diagnostic
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
		if err := authResolver.runtime.SetAPIKey(explicit.Provider, p.parsed.apiKey); err != nil {
			return agentruntime.CreateResult{}, productionAuthError(err)
		}
	}
	providersToCheck := snapshot.Providers
	if explicit != nil {
		providersToCheck = []string{explicit.Provider}
	}
	for _, providerID := range providersToCheck {
		if _, err := authResolver.check(ctx, providerID); err != nil {
			return agentruntime.CreateResult{}, productionAuthError(err)
		}
	}
	availableModels := modelcatalog.FilterAvailableModels(snapshot.Models, availability)
	scope := modelcatalog.ResolveModelScope(snapshot.Settings.EnabledModels, availableModels)
	for _, diagnostic := range scope.Diagnostics {
		diagnostics = append(diagnostics, agentruntime.Diagnostic{Kind: agentruntime.DiagnosticWarning, Message: diagnostic.Message})
	}
	services := &agentruntime.Services{
		CWD: cwd, AgentDir: p.agentDir, ModelRuntime: catalog, ResourceService: resources,
		AuthRuntime: authResolver.runtime, Provider: router, Tool: executor, Tools: append([]provider.ToolDefinition(nil), definitions...),
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
	created, err := agentruntime.CreateAgentSession(ctx, agentruntime.SessionFactoryOptions{
		Services: services, Provider: router, SessionManager: options.SessionManager,
		AllModels: snapshot.Models, Availability: availability, ExplicitModel: explicit,
		ExplicitThinkingLevel: explicitThinking, ScopedModels: scope.ScopedModels, Settings: snapshot.Settings,
		BaseConfig: agent.SessionConfig{
			SystemPrompt: resourceSnapshot.SystemPrompt, Tool: executor, Tools: activeDefinitions,
			AllTools: definitions, ActiveToolNames: activeToolNames, Stream: stream,
			CompactionEnabled: &compactionEnabled,
			ContextReserve:    snapshot.Settings.Compaction.ReserveTokensOrDefault(),
			KeepRecentTokens:  snapshot.Settings.Compaction.KeepRecentTokensOrDefault(),
			ContextReserveSet: true, KeepRecentTokensSet: true,
			BranchSummaryReserveTokens: snapshot.Settings.BranchSummary.ReserveTokensOrDefault(), BranchSummaryReserveSet: true,
			Retry: retryPolicy,
			ResolveStreamOptions: func(turnCtx context.Context, selected provider.Model) (provider.StreamOptions, error) {
				// SettingsManager is live in upstream: transport and provider retry/
				// timeout values are read for every actual provider call, not frozen
				// when the session is created. AgentSession applies later explicit
				// turn/request overlays after this resolver.
				turnStream, streamErr := productionProviderStreamOptions(catalog.Snapshot().Settings, "")
				if streamErr != nil {
					return provider.StreamOptions{}, streamErr
				}
				resolved, err := authResolver.requirePromptAccess(turnCtx, selected, p.docsDir)
				if err != nil {
					return provider.StreamOptions{}, err
				}
				return provider.MergeStreamOptions(turnStream, provider.StreamOptions{
					APIKey: resolved.APIKey, Headers: resolved.Headers, Env: productionProviderEnv(resolved.Env, p.ambientEnvironment),
				}), nil
			},
			ResolveSummarizer: func(_ context.Context, request agent.SummarizerResolveRequest) (session.Summarizer, error) {
				return provider.NewContextSummarizerWithOptions(router, request.Model, p.config.AgentNow, provider.ContextSummarizerOptions{
					ThinkingLevel: request.ThinkingLevel,
					Stream:        request.Stream,
					Retry:         request.Retry,
				})
			},
			ResolveBranchSummarizer: func(_ context.Context, request agent.SummarizerResolveRequest) (session.BranchSummarizer, error) {
				return provider.NewContextSummarizerWithOptions(router, request.Model, p.config.AgentNow, provider.ContextSummarizerOptions{
					ThinkingLevel: request.ThinkingLevel,
					Stream:        request.Stream,
					Retry:         request.Retry,
				})
			},
			ValidateModelAccess: func(accessCtx context.Context, selected provider.Model) error {
				_, err := authResolver.requirePromptAccess(accessCtx, selected, p.docsDir)
				return err
			},
			ValidateModelSelection: func(accessCtx context.Context, selected provider.Model) error {
				_, err := authResolver.requireSelectionAccess(accessCtx, selected)
				return err
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

func newProductionProviderRouter(snapshot modelcatalog.Snapshot, config ProductionConfig) (*provider.Router, error) {
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
	registrations := make([]provider.ProviderRegistration, 0, len(snapshot.Providers))
	for _, providerID := range snapshot.Providers {
		registrations = append(registrations, provider.ProviderRegistration{ID: providerID, Adapters: map[string]provider.Provider{
			provider.OpenAIResponsesAPI: responses, provider.OpenAICompletionsAPI: completions,
			provider.OpenAICodexResponsesAPI: codex, provider.AnthropicMessagesAPI: anthropic,
		}})
	}
	router, err := provider.NewModelRouter(registrations)
	if err != nil {
		return nil, fmt.Errorf("%w: initialize provider router: %w", ErrInvalidProductionConfig, err)
	}
	return router, nil
}

type productionProviderAuthResolver struct {
	runtime            *auth.Runtime
	catalog            *modelcatalog.Runtime
	ambientEnvironment map[string]string
	codexOAuth         *auth.OpenAICodexOAuth
	codexClock         func() time.Time
	anthropicOAuth     *auth.AnthropicOAuth
	anthropicClock     func() time.Time
}

func newProductionProviderAuthResolver(agentDir string, catalog *modelcatalog.Runtime, ambientEnvironment map[string]string, config ProductionConfig) (*productionProviderAuthResolver, error) {
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
		runtime: auth.NewRuntime(store), catalog: catalog, ambientEnvironment: cloneStringMap(ambientEnvironment),
		codexOAuth: codexOAuth, codexClock: config.OpenAIOAuthClock,
		anthropicOAuth: anthropicOAuth, anthropicClock: config.AnthropicOAuthClock,
	}, nil
}

func (r *productionProviderAuthResolver) providerConfig(providerID string) modelcatalog.ProviderConfig {
	configured, _ := r.catalog.Provider(providerID)
	return configured
}

func (r *productionProviderAuthResolver) check(ctx context.Context, providerID string) (bool, error) {
	credential, exists, err := r.runtime.Read(ctx, providerID)
	if err != nil {
		return false, err
	}
	if exists {
		switch credential.Type {
		case "api_key":
			return true, nil
		case "oauth":
			return strings.EqualFold(providerID, auth.OpenAICodexProviderID) || strings.EqualFold(providerID, auth.AnthropicProviderID), nil
		default:
			return false, &auth.Error{Kind: auth.KindUnsupported, Operation: "check stored credential", Provider: providerID, Cause: auth.ErrCredentialType}
		}
	}
	configured := r.providerConfig(providerID)
	if configured.ConfiguredAPIKey != nil {
		return auth.ValueConfigured(*configured.ConfiguredAPIKey, nil, r.ambientEnvironment), nil
	}
	switch {
	case strings.EqualFold(providerID, auth.OpenAIProviderID):
		return strings.TrimSpace(r.ambientEnvironment[openAIKeyEnvironment]) != "", nil
	case strings.EqualFold(providerID, auth.AnthropicProviderID):
		for _, name := range []string{auth.AnthropicAuthTokenEnvironment, auth.AnthropicOAuthTokenEnvironment, auth.AnthropicAPIKeyEnvironment} {
			if strings.TrimSpace(r.ambientEnvironment[name]) != "" {
				return true, nil
			}
		}
	}
	return false, nil
}

func (r *productionProviderAuthResolver) checkModel(ctx context.Context, candidate modelcatalog.Model) (bool, error) {
	configured, err := r.check(ctx, candidate.Provider)
	if err != nil || configured {
		return configured, err
	}
	providerConfig := r.providerConfig(candidate.Provider)
	if providerConfig.AuthHeader != nil && *providerConfig.AuthHeader {
		return false, nil
	}
	selected, err := candidate.Ref()
	if err != nil {
		return false, err
	}
	providerHeaders, err := auth.ResolveHeaders(ctx, providerConfig.Headers, "provider "+candidate.Provider, nil, r.ambientEnvironment)
	if err != nil {
		return false, err
	}
	modelHeaders, err := auth.ResolveHeaders(ctx, selected.Headers(), "model "+candidate.Provider+"/"+candidate.ID, nil, r.ambientEnvironment)
	if err != nil {
		return false, err
	}
	return authHeadersAuthorizeModel(candidate.API, mergeProductionHeaders(providerHeaders, modelHeaders)), nil
}

func (r *productionProviderAuthResolver) resolve(ctx context.Context, selected provider.Model) (auth.OpenAIAuthResult, bool, error) {
	configured := r.providerConfig(selected.Provider())
	var result auth.OpenAIAuthResult
	var err error
	switch {
	case strings.EqualFold(selected.Provider(), auth.OpenAIProviderID):
		result, err = auth.ResolveOpenAIAuth(ctx, r.runtime, nil, configured.ConfiguredAPIKey, r.ambientEnvironment, auth.OpenAIResolveOptions{})
	case strings.EqualFold(selected.Provider(), auth.OpenAICodexProviderID):
		result, err = auth.ResolveOpenAICodexAuth(ctx, r.runtime, nil, configured.ConfiguredAPIKey, r.ambientEnvironment, auth.OpenAIResolveOptions{OAuth: r.codexOAuth, Clock: r.codexClock})
	case strings.EqualFold(selected.Provider(), auth.AnthropicProviderID):
		result, err = auth.ResolveAnthropicAuth(ctx, r.runtime, nil, configured.ConfiguredAPIKey, r.ambientEnvironment, auth.AnthropicResolveOptions{OAuth: r.anthropicOAuth, Clock: r.anthropicClock})
	default:
		result, err = auth.ResolveProviderAPIKey(ctx, r.runtime, selected.Provider(), nil, configured.ConfiguredAPIKey, r.ambientEnvironment)
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
			return auth.OpenAIAuthResult{}, false, productionAuthError(err)
		}
	}
	if result.APIKey != "" {
		key, validationErr := validateResolvedAPIKey(result.APIKey, "resolved "+selected.Provider()+" API key")
		if validationErr != nil {
			return auth.OpenAIAuthResult{}, false, validationErr
		}
		result.APIKey = key
	}
	providerHeaders, headerErr := auth.ResolveHeaders(ctx, configured.Headers, "provider "+selected.Provider(), result.Env, r.ambientEnvironment)
	if headerErr != nil {
		return auth.OpenAIAuthResult{}, false, productionAuthError(headerErr)
	}
	result.Headers = mergeProductionHeaders(result.Headers, providerHeaders)
	if configured.AuthHeader != nil && *configured.AuthHeader {
		if result.APIKey == "" {
			return auth.OpenAIAuthResult{}, false, productionAuthError(&auth.Error{Kind: auth.KindNotConfigured, Operation: "resolve authHeader API key", Provider: selected.Provider()})
		}
		result.Headers = mergeProductionHeaders(result.Headers, map[string]string{"Authorization": "Bearer " + result.APIKey})
	}
	modelHeaders, headerErr := auth.ResolveHeaders(ctx, selected.Headers(), "model "+selected.Provider()+"/"+selected.ID(), result.Env, r.ambientEnvironment)
	if headerErr != nil {
		return auth.OpenAIAuthResult{}, false, productionAuthError(headerErr)
	}
	result.Headers = mergeProductionHeaders(result.Headers, modelHeaders)
	if result.APIKey == "" && !authHeadersAuthorizeModel(selected.API(), result.Headers) {
		return auth.OpenAIAuthResult{}, false, nil
	}
	return result, true, nil
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
		}
	}
	return false
}

func productionProviderEnv(authEnv, ambient map[string]string) map[string]string {
	value := ambient["PI_CACHE_RETENTION"]
	if scoped := authEnv["PI_CACHE_RETENTION"]; scoped != "" {
		value = scoped
	}
	if value == "" {
		return nil
	}
	return map[string]string{"PI_CACHE_RETENTION": value}
}

func (r *productionProviderAuthResolver) requirePromptAccess(ctx context.Context, selected provider.Model, docsDir string) (auth.OpenAIAuthResult, error) {
	resolved, available, err := r.resolve(ctx, selected)
	if err != nil {
		return auth.OpenAIAuthResult{}, err
	}
	if !available {
		return auth.OpenAIAuthResult{}, &agent.ModelAccessError{Message: agentruntime.FormatNoAPIKeyFoundMessage(selected.Provider(), docsDir)}
	}
	return resolved, nil
}

func (r *productionProviderAuthResolver) requireSelectionAccess(ctx context.Context, selected provider.Model) (auth.OpenAIAuthResult, error) {
	resolved, available, err := r.resolve(ctx, selected)
	if err != nil {
		return auth.OpenAIAuthResult{}, err
	}
	if !available {
		return auth.OpenAIAuthResult{}, &agent.ModelAccessError{Message: "No API key for " + selected.Provider() + "/" + selected.ID()}
	}
	return resolved, nil
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
