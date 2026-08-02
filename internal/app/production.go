package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/auth"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
	"github.com/cat3399/pi-go/internal/tool"
)

const (
	openAIProviderID     = provider.OpenAIProviderID
	openAIResponsesAPI   = provider.OpenAIResponsesAPI
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
	Environment []string

	OpenAIHTTPClient provider.HTTPDoer
	OpenAIClock      provider.Clock

	SessionID         string
	SessionNow        session.Clock
	NewSessionEntryID session.IDGenerator
	AgentNow          func() time.Time
	SettlementTimeout time.Duration

	BashRunner            tool.Runner
	BashShellPath         string
	BashArtifactDirectory string
	BashMaxOutputLines    int
	BashMaxOutputBytes    int
}

// RunProduction admits product-facing model/auth flags, resolves the fixed
// OpenAI production configuration, and then executes the exact same lifecycle
// path as Run. Assembly completes before session or network side effects.
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
		dependencies, err := assembleProductionDependencies(ctx, config, parsed)
		if err != nil {
			return runtimeDependencies{}, err
		}
		return validateDependencies(dependencies)
	})
}

func assembleProductionDependencies(
	ctx context.Context,
	config ProductionConfig,
	parsed options,
) (Dependencies, error) {
	if cause := context.Cause(ctx); cause != nil {
		return Dependencies{}, fmt.Errorf("production assembly cancelled: %w", cause)
	}
	workingDir, err := resolveWorkingDirectory(config.WorkingDir)
	if err != nil {
		return Dependencies{}, fmt.Errorf("%w: %w", ErrInvalidProductionConfig, err)
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
		return Dependencies{}, err
	}
	model, err := resolveProductionModel(parsed)
	if err != nil {
		return Dependencies{}, err
	}
	modelConfig, err := loadOpenAIModelConfig(agentDir)
	if err != nil {
		return Dependencies{}, err
	}
	apiKey, err := resolveOpenAIAPIKey(ctx, parsed, agentDir, modelConfig, ambientEnvironment)
	if err != nil {
		return Dependencies{}, err
	}
	implementation, err := provider.NewOpenAIResponsesProvider(provider.OpenAIResponsesConfig{
		BaseURL: modelConfig.baseURL,
		APIKey:  apiKey,
		Client:  config.OpenAIHTTPClient,
		Clock:   config.OpenAIClock,
	})
	if err != nil {
		return Dependencies{}, fmt.Errorf("%w: initialize OpenAI Responses provider: %w", ErrInvalidProductionConfig, err)
	}

	sessionClock := config.SessionNow
	if sessionClock == nil {
		sessionClock = time.Now
	}
	createdAt := sessionClock()
	if createdAt.IsZero() {
		return Dependencies{}, fmt.Errorf("%w: default session timestamp is zero", ErrInvalidProductionConfig)
	}
	sessionID := config.SessionID
	if sessionID == "" {
		sessionID, err = session.NewSessionID(createdAt)
		if err != nil {
			return Dependencies{}, fmt.Errorf("%w: generate default session ID: %w", ErrInvalidProductionConfig, err)
		}
	}
	if err := validateProductionSessionID(sessionID); err != nil {
		return Dependencies{}, err
	}
	defaultPath := productionSessionPathFactory(agentDir, createdAt, sessionID)

	return Dependencies{
		Provider:              implementation,
		Model:                 model,
		SystemPrompt:          "",
		WorkingDir:            workingDir,
		DefaultSessionPath:    defaultPath,
		SessionID:             sessionID,
		SessionNow:            sessionClock,
		SessionCreateTime:     createdAt,
		NewSessionEntryID:     config.NewSessionEntryID,
		AgentNow:              config.AgentNow,
		SettlementTimeout:     config.SettlementTimeout,
		BashRunner:            config.BashRunner,
		BashEnvironment:       append([]string(nil), environment...),
		BashShellPath:         config.BashShellPath,
		BashArtifactDirectory: config.BashArtifactDirectory,
		BashMaxOutputLines:    config.BashMaxOutputLines,
		BashMaxOutputBytes:    config.BashMaxOutputBytes,
	}, nil
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

func resolveProductionModel(parsed options) (provider.ModelRef, error) {
	if parsed.hasAPIKey && parsed.modelID == "" {
		return provider.ModelRef{}, fmt.Errorf(
			"%w: --api-key requires an explicit --model",
			ErrInvalidArguments,
		)
	}
	if parsed.providerID != "" && parsed.modelID == "" {
		return provider.ModelRef{}, fmt.Errorf(
			"%w: --provider requires --model",
			ErrInvalidArguments,
		)
	}

	providerID := parsed.providerID
	modelID := parsed.modelID
	if modelID == "" {
		providerID = openAIProviderID
		modelID = defaultOpenAIModel
	} else if providerID == "" {
		prefix, remainder, hasPrefix := strings.Cut(modelID, "/")
		switch {
		case hasPrefix && strings.EqualFold(prefix, openAIProviderID):
			providerID = openAIProviderID
			modelID = remainder
		case hasPrefix:
			return provider.ModelRef{}, fmt.Errorf(
				"%w: --model selects an unknown provider",
				ErrInvalidArguments,
			)
		case modelID == defaultOpenAIModel:
			providerID = openAIProviderID
		default:
			return provider.ModelRef{}, fmt.Errorf(
				"%w: bare model is outside the migrated registry; specify --provider openai or --model openai/<id>",
				ErrInvalidArguments,
			)
		}
	} else {
		if !strings.EqualFold(providerID, openAIProviderID) {
			return provider.ModelRef{}, fmt.Errorf(
				"%w: selected provider is not supported by this production assembly",
				ErrInvalidArguments,
			)
		}
		providerID = openAIProviderID
		prefix := openAIProviderID + "/"
		if len(modelID) >= len(prefix) && strings.EqualFold(modelID[:len(prefix)], prefix) {
			modelID = modelID[len(prefix):]
		}
	}
	if !validSelectorValue(modelID) || strings.TrimSpace(modelID) != modelID {
		return provider.ModelRef{}, fmt.Errorf("%w: resolved model ID is invalid", ErrInvalidArguments)
	}
	model, err := provider.NewModelRef(providerID, openAIResponsesAPI, modelID)
	if err != nil {
		return provider.ModelRef{}, fmt.Errorf("%w: %w", ErrInvalidArguments, err)
	}
	return model, nil
}

func resolveOpenAIAPIKey(
	ctx context.Context,
	parsed options,
	agentDir string,
	modelConfig openAIModelConfig,
	ambientEnvironment map[string]string,
) (string, error) {
	if parsed.hasAPIKey {
		return validateResolvedAPIKey(parsed.apiKey, "CLI OpenAI API key")
	}
	store, err := auth.NewStore(auth.Options{Path: filepath.Join(agentDir, "auth.json")})
	if err != nil {
		return "", fmt.Errorf("%w: initialize auth storage", ErrInvalidProductionConfig)
	}
	runtime := auth.NewRuntime(store)
	stored, exists, err := runtime.Read(ctx, openAIProviderID)
	if err != nil {
		return "", productionAuthError(err, "stored OpenAI API key")
	}
	if exists {
		if stored.Type != "api_key" {
			return "", fmt.Errorf("%w: auth.json OpenAI credential type is not migrated", ErrUnsupportedProductionValue)
		}
		key, err := auth.ResolveValue(ctx, stored.Key, "stored OpenAI API key", stored.Env, ambientEnvironment)
		if err != nil {
			return "", productionAuthError(err, "stored OpenAI API key")
		}
		return validateResolvedAPIKey(key, "stored OpenAI API key")
	}
	if modelConfig.apiKey != nil {
		key, err := auth.ResolveValue(ctx, *modelConfig.apiKey, "configured OpenAI API key", nil, ambientEnvironment)
		if err != nil {
			return "", productionAuthError(err, "configured OpenAI API key")
		}
		return validateResolvedAPIKey(key, "configured OpenAI API key")
	}
	if key := ambientEnvironment[openAIKeyEnvironment]; strings.TrimSpace(key) != "" {
		return validateResolvedAPIKey(key, openAIKeyEnvironment)
	}
	return "", fmt.Errorf(
		"%w: OpenAI API key is not configured by --api-key, auth.json, models.json, or %s",
		ErrInvalidProductionConfig,
		openAIKeyEnvironment,
	)
}

// productionAuthError maps service categories to the existing product error
// vocabulary without including the rejected value, raw file bytes, or a key.
func productionAuthError(err error, description string) error {
	switch {
	case auth.IsKind(err, auth.KindUnsupported):
		return fmt.Errorf("%w: command-backed %s is not migrated", ErrUnsupportedProductionValue, description)
	case auth.IsKind(err, auth.KindNotConfigured):
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
