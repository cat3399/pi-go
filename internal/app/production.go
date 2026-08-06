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
	// OpenAIOAuthHTTPClient/BaseURL are test and embedding seams for OAuth
	// token exchange. Empty values use a separately cloned no-redirect default
	// HTTP client and the fixed auth.openai.com endpoint.
	OpenAIOAuthHTTPClient *http.Client
	OpenAIOAuthBaseURL    string
	OpenAIOAuthClock      func() time.Time

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
	resources, err := resource.New(resource.Config{
		CWD: cwd, AgentDir: p.agentDir,
		Tools: []resource.Tool{{Name: tool.BashToolName, Snippet: "Execute a shell command in the current working directory."}},
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
	authResolver, err := newProductionOpenAIAuthResolver(p.parsed, p.agentDir, catalog, p.ambientEnvironment, p.config)
	if err != nil {
		return agentruntime.CreateResult{}, err
	}
	resolvedAuth, hasAuth, err := authResolver.resolve(ctx)
	if err != nil {
		return agentruntime.CreateResult{}, err
	}
	responses, err := provider.NewOpenAIResponsesProvider(provider.OpenAIResponsesConfig{APIKey: resolvedAuth.APIKey, Client: p.config.OpenAIHTTPClient, Clock: p.config.OpenAIClock})
	if err != nil {
		return agentruntime.CreateResult{}, fmt.Errorf("%w: initialize OpenAI Responses provider: %w", ErrInvalidProductionConfig, err)
	}
	completions, err := provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{APIKey: resolvedAuth.APIKey, Client: p.config.OpenAIHTTPClient, Clock: p.config.OpenAIClock})
	if err != nil {
		return agentruntime.CreateResult{}, fmt.Errorf("%w: initialize OpenAI Chat Completions provider: %w", ErrInvalidProductionConfig, err)
	}
	router, err := provider.NewModelRouter([]provider.ProviderRegistration{{ID: openAIProviderID, Adapters: map[string]provider.Provider{
		provider.OpenAIResponsesAPI: responses, provider.OpenAICompletionsAPI: completions,
	}}})
	if err != nil {
		return agentruntime.CreateResult{}, fmt.Errorf("%w: initialize provider router: %w", ErrInvalidProductionConfig, err)
	}
	availability := modelcatalog.Availability{
		HasConfiguredAuth: func(providerID string) bool { return strings.EqualFold(providerID, openAIProviderID) && hasAuth },
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
			HasConfiguredAuth: availability.HasConfiguredAuth,
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
	availableModels := modelcatalog.FilterAvailableModels(snapshot.Models, availability)
	scope := modelcatalog.ResolveModelScope(snapshot.Settings.EnabledModels, availableModels)
	for _, diagnostic := range scope.Diagnostics {
		diagnostics = append(diagnostics, agentruntime.Diagnostic{Kind: agentruntime.DiagnosticWarning, Message: diagnostic.Message})
	}
	toolOptions := tool.BashOptions{
		WorkingDir: cwd, Environment: append([]string(nil), p.environment...), Runner: p.config.BashRunner,
		ShellPath: p.config.BashShellPath, ArtifactDirectory: p.config.BashArtifactDirectory,
		MaxOutputLines: p.config.BashMaxOutputLines, MaxOutputBytes: p.config.BashMaxOutputBytes,
	}
	executor, definitions, err := buildProductionToolRuntime(toolOptions)
	if err != nil {
		return agentruntime.CreateResult{}, fmt.Errorf("%w: initialize session tool runtime: %w", ErrInvalidProductionConfig, err)
	}
	services := &agentruntime.Services{
		CWD: cwd, AgentDir: p.agentDir, ModelRuntime: catalog, ResourceService: resources,
		AuthRuntime: authResolver.runtime, Provider: router, Tool: executor, Tools: append([]provider.ToolDefinition(nil), definitions...),
	}
	stream := provider.StreamOptions{SessionID: options.SessionManager.SessionID()}
	if hasAuth {
		stream.APIKey = resolvedAuth.APIKey
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
			SystemPrompt: resourceSnapshot.SystemPrompt, Tool: executor, Tools: definitions, Stream: stream,
			ResolveStreamOptions: func(turnCtx context.Context, selected provider.Model) (provider.StreamOptions, error) {
				resolved, err := authResolver.requirePromptAccess(turnCtx, selected, p.docsDir)
				if err != nil {
					return provider.StreamOptions{}, err
				}
				return provider.StreamOptions{APIKey: resolved.APIKey}, nil
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

func clockBeginningWith(first time.Time, subsequent session.Clock) session.Clock {
	var delivered atomic.Bool
	return func() time.Time {
		if delivered.CompareAndSwap(false, true) {
			return first
		}
		return subsequent()
	}
}

func resolveOpenAIAPIKey(
	ctx context.Context,
	parsed options,
	agentDir string,
	modelConfig openAIModelConfig,
	ambientEnvironment map[string]string,
	config ProductionConfig,
) (auth.OpenAIAuthResult, error) {
	store, err := auth.NewStore(auth.Options{Path: filepath.Join(agentDir, "auth.json")})
	if err != nil {
		return auth.OpenAIAuthResult{}, fmt.Errorf("%w: initialize auth storage", ErrInvalidProductionConfig)
	}
	runtime := auth.NewRuntime(store)
	if parsed.hasAPIKey {
		if err := runtime.SetAPIKey(openAIProviderID, parsed.apiKey); err != nil {
			return auth.OpenAIAuthResult{}, productionAuthError(err)
		}
	}
	flow, err := auth.NewOpenAICodexOAuth(auth.OpenAICodexOAuthConfig{HTTPClient: config.OpenAIOAuthHTTPClient, AuthBaseURL: config.OpenAIOAuthBaseURL, Clock: config.OpenAIOAuthClock})
	if err != nil {
		return auth.OpenAIAuthResult{}, productionAuthError(err)
	}
	resolved, err := auth.ResolveOpenAIAuth(ctx, runtime, nil, modelConfig.apiKey, ambientEnvironment, auth.OpenAIResolveOptions{OAuth: flow, Clock: config.OpenAIOAuthClock})
	if err != nil {
		return auth.OpenAIAuthResult{}, productionAuthError(err)
	}
	key, err := validateResolvedAPIKey(resolved.APIKey, "resolved OpenAI API key")
	if err != nil {
		return auth.OpenAIAuthResult{}, err
	}
	resolved.APIKey = key
	return resolved, nil
}

type productionOpenAIAuthResolver struct {
	runtime            *auth.Runtime
	configured         *string
	ambientEnvironment map[string]string
	oauth              *auth.OpenAICodexOAuth
	clock              func() time.Time
}

func newProductionOpenAIAuthResolver(
	parsed options,
	agentDir string,
	catalog *modelcatalog.Runtime,
	ambientEnvironment map[string]string,
	config ProductionConfig,
) (*productionOpenAIAuthResolver, error) {
	store, err := auth.NewStore(auth.Options{Path: filepath.Join(agentDir, "auth.json")})
	if err != nil {
		return nil, fmt.Errorf("%w: initialize auth storage", ErrInvalidProductionConfig)
	}
	authRuntime := auth.NewRuntime(store)
	// Match main.ts: install a CLI key only after CLI model admission. A key
	// without a model becomes a blocking runtime diagnostic and must not make
	// an automatic/default model available.
	if parsed.hasAPIKey && parsed.modelID != "" {
		if err := authRuntime.SetAPIKey(openAIProviderID, parsed.apiKey); err != nil {
			return nil, productionAuthError(err)
		}
	}
	flow, err := auth.NewOpenAICodexOAuth(auth.OpenAICodexOAuthConfig{
		HTTPClient: config.OpenAIOAuthHTTPClient, AuthBaseURL: config.OpenAIOAuthBaseURL, Clock: config.OpenAIOAuthClock,
	})
	if err != nil {
		return nil, productionAuthError(err)
	}
	configured, _ := catalog.Provider(openAIProviderID)
	var configuredKey *string
	if configured.ConfiguredAPIKey != nil {
		copy := *configured.ConfiguredAPIKey
		configuredKey = &copy
	}
	return &productionOpenAIAuthResolver{
		runtime: authRuntime, configured: configuredKey, ambientEnvironment: cloneStringMap(ambientEnvironment),
		oauth: flow, clock: config.OpenAIOAuthClock,
	}, nil
}

func (r *productionOpenAIAuthResolver) resolve(ctx context.Context) (auth.OpenAIAuthResult, bool, error) {
	resolved, err := auth.ResolveOpenAIAuth(ctx, r.runtime, nil, r.configured, r.ambientEnvironment, auth.OpenAIResolveOptions{
		OAuth: r.oauth, Clock: r.clock,
	})
	if err != nil {
		var typed *auth.Error
		if auth.IsKind(err, auth.KindNotConfigured) && errors.As(err, &typed) && typed.Operation == "resolve OpenAI API key" {
			return auth.OpenAIAuthResult{}, false, nil
		}
		return auth.OpenAIAuthResult{}, false, productionAuthError(err)
	}
	key, err := validateResolvedAPIKey(resolved.APIKey, "resolved OpenAI API key")
	if err != nil {
		return auth.OpenAIAuthResult{}, false, err
	}
	resolved.APIKey = key
	return resolved, true, nil
}

func (r *productionOpenAIAuthResolver) requirePromptAccess(ctx context.Context, selected provider.Model, docsDir string) (auth.OpenAIAuthResult, error) {
	resolved, available, err := r.resolve(ctx)
	if err != nil {
		return auth.OpenAIAuthResult{}, err
	}
	if !strings.EqualFold(selected.Provider(), openAIProviderID) || !available {
		return auth.OpenAIAuthResult{}, &agent.ModelAccessError{Message: agentruntime.FormatNoAPIKeyFoundMessage(selected.Provider(), docsDir)}
	}
	return resolved, nil
}

func (r *productionOpenAIAuthResolver) requireSelectionAccess(ctx context.Context, selected provider.Model) (auth.OpenAIAuthResult, error) {
	resolved, available, err := r.resolve(ctx)
	if err != nil {
		return auth.OpenAIAuthResult{}, err
	}
	if !strings.EqualFold(selected.Provider(), openAIProviderID) || !available {
		return auth.OpenAIAuthResult{}, &agent.ModelAccessError{Message: "No API key for " + selected.Provider() + "/" + selected.ID()}
	}
	return resolved, nil
}

// productionAuthError maps service categories to the existing product error
// vocabulary without including the rejected value, raw file bytes, or a key.
func productionAuthError(err error) error {
	var typed *auth.Error
	_ = errors.As(err, &typed)
	description := "OpenAI API key"
	if typed != nil && strings.Contains(typed.Operation, "stored") {
		description = "stored OpenAI API key"
	} else if typed != nil && strings.Contains(typed.Operation, "configured") {
		description = "configured OpenAI API key"
	}
	switch {
	case errors.Is(err, auth.ErrCredentialType):
		return fmt.Errorf("%w: auth.json OpenAI credential type is not migrated", ErrUnsupportedProductionValue)
	case errors.Is(err, auth.ErrCommandBacked):
		return fmt.Errorf("%w: command-backed %s is not migrated", ErrUnsupportedProductionValue, description)
	case errors.Is(err, auth.ErrPersistentAuthUnavailable):
		return fmt.Errorf("%w: persistent auth storage is unavailable on this platform", ErrUnsupportedProductionValue)
	case auth.IsKind(err, auth.KindNotConfigured):
		if typed != nil && typed.Operation == "resolve OpenAI API key" {
			return fmt.Errorf(
				"%w: OpenAI API key is not configured by --api-key, auth.json, models.json, or %s",
				ErrInvalidProductionConfig,
				openAIKeyEnvironment,
			)
		}
		return fmt.Errorf("%w: %s references missing environment variable", ErrInvalidProductionConfig, description)
	case auth.IsKind(err, auth.KindPermission):
		return fmt.Errorf("%w: auth.json credential file permissions are unsafe", ErrInvalidProductionConfig)
	case auth.IsKind(err, auth.KindMalformed):
		return fmt.Errorf("%w: auth.json is malformed", ErrInvalidProductionConfig)
	case auth.IsKind(err, auth.KindCancelled):
		return fmt.Errorf("%w: resolve %s cancelled", ErrInvalidProductionConfig, description)
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
