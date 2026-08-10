package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/model"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

// SessionFactoryOptions contains the already-assembled, cwd-bound product
// dependencies used by createAgentSession. It deliberately accepts resolved
// catalog models and provider/auth predicates instead of discovering either
// inside the agent core.
type SessionFactoryOptions struct {
	Services              *Services
	Provider              provider.Provider
	SessionManager        *session.SessionManager
	AllModels             []model.Model
	Availability          model.Availability
	ExplicitModel         *model.Model
	ExplicitThinkingLevel *provider.ThinkingLevel
	ScopedModels          []model.ScopedModel
	Settings              model.Settings
	PersistSettings       agent.SettingsPersistence
	BaseConfig            agent.SessionConfig
	SessionStartEvent     *agent.SessionStartHookEvent
	Diagnostics           []Diagnostic
	// DocsDir supplies the installed coding-agent documentation directory used
	// by the original no-model guidance. An empty value uses the relative
	// package path "docs".
	DocsDir string
}

// CreateAgentSession mirrors sdk.ts's model/thinking bootstrap and returns the
// transport-neutral result consumed by Runtime.Factory and future app hosts.
func CreateAgentSession(ctx context.Context, options SessionFactoryOptions) (CreateResult, error) {
	if ctx == nil {
		return CreateResult{}, errors.New("session factory context is required")
	}
	if options.Services == nil {
		return CreateResult{}, errors.New("cwd-bound services are required")
	}
	if options.SessionManager == nil {
		return CreateResult{}, errors.New("session manager is required")
	}
	if isNilProvider(options.Provider) {
		return CreateResult{}, errors.New("provider is required")
	}
	if cause := context.Cause(ctx); cause != nil {
		return CreateResult{}, cause
	}

	existing := options.SessionManager.BuildContext()
	hasExistingSession := len(existing.AgentMessages()) > 0
	hasThinkingEntry, err := branchHasThinkingEntry(options.SessionManager)
	if err != nil {
		return CreateResult{}, fmt.Errorf("inspect session thinking state: %w", err)
	}

	selected := cloneCatalogModelPointer(options.ExplicitModel)
	if selected != nil {
		if options.Availability.SupportsRoute == nil || !options.Availability.SupportsRoute(*selected) {
			return CreateResult{}, fmt.Errorf("explicit model %s/%s is not supported by a registered provider route", selected.Provider, selected.ID)
		}
	}

	var restorePrefix string
	if selected == nil && hasExistingSession {
		if saved, ok := existing.Model(); ok {
			restored := exactCatalogModel(options.AllModels, saved.Provider, saved.ModelID)
			if restored != nil && options.Availability.Available(*restored) {
				selected = restored
			} else {
				restorePrefix = "Could not restore model " + saved.Provider + "/" + saved.ModelID
			}
		}
	}

	var scopedThinking *provider.ThinkingLevel
	if selected == nil && !hasExistingSession {
		selected, scopedThinking = selectScopedModel(options.ScopedModels, options.Settings, options.Availability)
	}
	if selected == nil {
		resolved := model.ResolveInitialModel(model.InitialModelOptions{
			IsContinuing:         hasExistingSession,
			DefaultProvider:      options.Settings.DefaultProvider,
			DefaultModelID:       options.Settings.DefaultModel,
			DefaultThinkingLevel: settingsThinkingPointer(options.Settings.DefaultThinkingLevel),
			AllModels:            options.AllModels,
			Availability:         options.Availability,
		})
		selected = cloneCatalogModelPointer(resolved.Model)
	}

	var fallback *string
	if selected == nil {
		message := formatNoModelsAvailableMessage(options.DocsDir)
		fallback = &message
	} else if restorePrefix != "" {
		message := restorePrefix + ". Using " + selected.Provider + "/" + selected.ID
		fallback = &message
	}

	thinking := model.DefaultThinkingLevel
	if options.ExplicitThinkingLevel != nil {
		thinking = *options.ExplicitThinkingLevel
	} else if hasExistingSession && hasThinkingEntry {
		if stored, ok := existing.ThinkingLevel(); ok {
			thinking = provider.ThinkingLevel(stored)
		}
	} else if scopedThinking != nil {
		thinking = *scopedThinking
	} else if options.Settings.DefaultThinkingLevel != "" {
		thinking = options.Settings.DefaultThinkingLevel
	}
	config := options.BaseConfig
	config.Provider = options.Provider
	config.SessionManager = options.SessionManager
	if config.AllTools == nil && options.Services.Tools != nil {
		config.AllTools = append([]provider.ToolDefinition(nil), options.Services.Tools...)
	}
	if config.Resources == nil && options.Services.ResourceService != nil {
		config.Resources = newSessionResources(options.Services.ResourceService)
	}
	if config.ReloadRuntime == nil && options.Services.ModelRuntime != nil {
		config.ReloadRuntime = options.Services.ModelRuntime.Reload
	}
	if config.StandaloneBash == nil && options.Services.StandaloneBash != nil {
		config.StandaloneBash = options.Services.StandaloneBash
	}
	if config.ReloadTools == nil && options.Services.ReloadTools != nil {
		config.ReloadTools = options.Services.ReloadTools
	}
	config.Model = provider.Model{}
	config.ThinkingLevel = provider.ThinkingOff
	if selected != nil {
		ref, refErr := selected.Ref()
		if refErr != nil {
			return CreateResult{}, fmt.Errorf("resolve selected model %s/%s: %w", selected.Provider, selected.ID, refErr)
		}
		config.Model = ref
		config.ThinkingLevel = ref.ClampThinkingLevel(thinking)
	}
	config.InitializeSessionState = true
	config.SessionStartEvent = options.SessionStartEvent
	config.NoModelSelectedMessage = formatNoModelSelectedMessage(options.DocsDir)
	allModels, err := catalogModelRefs(options.AllModels)
	if err != nil {
		return CreateResult{}, err
	}
	scopedModels, err := scopedModelRefs(options.ScopedModels)
	if err != nil {
		return CreateResult{}, err
	}
	config.AllModels = allModels
	config.ScopedModels = scopedModels
	config.ModelAvailable = func(_ context.Context, candidate provider.Model) (bool, error) {
		catalogModels := options.AllModels
		if options.Services.ModelRuntime != nil {
			catalogModels = options.Services.ModelRuntime.Snapshot().Models
		}
		catalog := exactCatalogModel(catalogModels, candidate.Provider(), candidate.ID())
		return catalog != nil && options.Availability.Available(*catalog), nil
	}
	if options.Services.ModelRuntime != nil && config.ResolveAvailableModels == nil {
		config.ResolveAvailableModels = func(_ context.Context) ([]provider.Model, error) {
			available := model.FilterAvailableModels(options.Services.ModelRuntime.Snapshot().Models, options.Availability)
			return catalogModelRefs(available)
		}
	}
	config.DefaultThinkingLevel = options.Settings.DefaultThinkingLevel
	if options.Services.ModelRuntime != nil {
		config.ResolveDefaultThinkingLevel = func() (provider.ThinkingLevel, bool) {
			level := options.Services.ModelRuntime.Snapshot().Settings.DefaultThinkingLevel
			return level, level != ""
		}
	}
	config.PersistSettings = options.PersistSettings
	if config.PersistSettings == nil && options.Services.ModelRuntime != nil {
		config.PersistSettings = runtimeSettingsPersistence(options.Services.ModelRuntime)
	}
	steeringMode, err := queueModeFromSetting(options.Settings.SteeringModeOrDefault())
	if err != nil {
		return CreateResult{}, err
	}
	followUpMode, err := queueModeFromSetting(options.Settings.FollowUpModeOrDefault())
	if err != nil {
		return CreateResult{}, err
	}
	config.SteeringMode = steeringMode
	config.FollowUpMode = followUpMode
	compactionEnabled := options.Settings.Compaction.EnabledOrDefault()
	retryEnabled := options.Settings.Retry.EnabledOrDefault()
	config.CompactionEnabled = &compactionEnabled
	config.AutoRetryEnabled = &retryEnabled
	baseRetry := config.Retry
	// Static factory users receive the same effective settings as the full
	// production runtime. ModelRuntime replaces these values dynamically below,
	// but its absence must not silently discard retry/compaction configuration.
	config.Retry = retryPolicyFromSettings(options.Settings.Retry, baseRetry)
	config.ContextReserve = options.Settings.Compaction.ReserveTokensOrDefault()
	config.ContextReserveSet = true
	config.KeepRecentTokens = options.Settings.Compaction.KeepRecentTokensOrDefault()
	config.KeepRecentTokensSet = true
	config.BranchSummaryReserveTokens = options.Settings.BranchSummary.ReserveTokensOrDefault()
	config.BranchSummaryReserveSet = true
	if options.Services.ModelRuntime != nil && config.ResolveRuntimeSettings == nil {
		config.ResolveRuntimeSettings = func() agent.RuntimeControlSettings {
			settings := options.Services.ModelRuntime.Snapshot().Settings
			steering, _ := queueModeFromSetting(settings.SteeringModeOrDefault())
			followUp, _ := queueModeFromSetting(settings.FollowUpModeOrDefault())
			return agent.RuntimeControlSettings{
				SteeringMode: steering, FollowUpMode: followUp,
				AutoCompactionEnabled:   settings.Compaction.EnabledOrDefault(),
				AutoRetryEnabled:        settings.Retry.EnabledOrDefault(),
				Retry:                   retryPolicyFromSettings(settings.Retry, baseRetry),
				CompactionReserveTokens: settings.Compaction.ReserveTokensOrDefault(), CompactionReserveSet: true,
				CompactionKeepRecentTokens: settings.Compaction.KeepRecentTokensOrDefault(), CompactionKeepRecentSet: true,
				BranchSummaryReserveTokens: settings.BranchSummary.ReserveTokensOrDefault(), BranchSummaryReserveSet: true,
			}
		}
	}

	created, err := agent.NewSession(config)
	if err != nil {
		return CreateResult{}, err
	}
	return CreateResult{
		Session: created, Services: options.Services,
		Diagnostics: append([]Diagnostic(nil), options.Diagnostics...), ModelFallbackMessage: fallback,
	}, nil
}

func runtimeSettingsPersistence(runtime *model.Runtime) agent.SettingsPersistence {
	return func(ctx context.Context, update agent.SettingsUpdate) (agent.SettingsWriteResult, error) {
		var previous model.Settings
		var expectedGeneration uint64
		err := runtime.SetGlobalSettings(ctx, func(settings *model.Settings) error {
			// SetGlobalSettings owns the runtime operation gate while invoking this
			// callback, so the generation published by this write is exactly next.
			expectedGeneration = runtime.Snapshot().Generation + 1
			previous.DefaultProvider = settings.DefaultProvider
			previous.DefaultModel = settings.DefaultModel
			previous.DefaultThinkingLevel = settings.DefaultThinkingLevel
			previous.SteeringMode = settings.SteeringMode
			previous.FollowUpMode = settings.FollowUpMode
			previous.Compaction.Enabled = cloneBool(settings.Compaction.Enabled)
			previous.Retry.Enabled = cloneBool(settings.Retry.Enabled)
			if update.DefaultProvider != nil {
				settings.DefaultProvider = *update.DefaultProvider
			}
			if update.DefaultModel != nil {
				settings.DefaultModel = *update.DefaultModel
			}
			if update.DefaultThinkingLevel != nil {
				settings.DefaultThinkingLevel = *update.DefaultThinkingLevel
			}
			if update.SteeringMode != nil {
				settings.SteeringMode = update.SteeringMode.String()
			}
			if update.FollowUpMode != nil {
				settings.FollowUpMode = update.FollowUpMode.String()
			}
			if update.AutoCompactionEnabled != nil {
				value := *update.AutoCompactionEnabled
				settings.Compaction.Enabled = &value
			}
			if update.AutoRetryEnabled != nil {
				value := *update.AutoRetryEnabled
				settings.Retry.Enabled = &value
			}
			return nil
		})
		if err != nil && !errors.Is(err, model.ErrCommitUnknown) {
			return agent.SettingsWriteResult{}, err
		}
		undo := func(undoCtx context.Context) error {
			return runtime.SetGlobalSettings(undoCtx, func(settings *model.Settings) error {
				if runtime.Snapshot().Generation != expectedGeneration {
					// Any intervening Runtime mutation owns the newer intent, including
					// an ABA/same-value write which field comparison cannot detect.
					return nil
				}
				// Restore only values still equal to this transaction's write.
				// This also protects against an out-of-band file replacement which
				// did not advance this process-local Runtime generation.
				if update.DefaultProvider != nil && settings.DefaultProvider == *update.DefaultProvider {
					settings.DefaultProvider = previous.DefaultProvider
				}
				if update.DefaultModel != nil && settings.DefaultModel == *update.DefaultModel {
					settings.DefaultModel = previous.DefaultModel
				}
				if update.DefaultThinkingLevel != nil && settings.DefaultThinkingLevel == *update.DefaultThinkingLevel {
					settings.DefaultThinkingLevel = previous.DefaultThinkingLevel
				}
				if update.SteeringMode != nil && settings.SteeringMode == update.SteeringMode.String() {
					settings.SteeringMode = previous.SteeringMode
				}
				if update.FollowUpMode != nil && settings.FollowUpMode == update.FollowUpMode.String() {
					settings.FollowUpMode = previous.FollowUpMode
				}
				if update.AutoCompactionEnabled != nil && optionalBoolEqual(settings.Compaction.Enabled, update.AutoCompactionEnabled) {
					settings.Compaction.Enabled = cloneBool(previous.Compaction.Enabled)
				}
				if update.AutoRetryEnabled != nil && optionalBoolEqual(settings.Retry.Enabled, update.AutoRetryEnabled) {
					settings.Retry.Enabled = cloneBool(previous.Retry.Enabled)
				}
				return nil
			})
		}
		return agent.SettingsWriteResult{Undo: undo, CommitUnknown: err != nil}, err
	}
}

func queueModeFromSetting(value string) (agent.QueueMode, error) {
	switch value {
	case model.QueueModeAll:
		return agent.QueueAll, nil
	case model.QueueModeOneAtATime:
		return agent.QueueOneAtATime, nil
	default:
		return 0, fmt.Errorf("invalid queue mode %q", value)
	}
}

func retryPolicyFromSettings(settings model.RetrySettings, base agent.RetryPolicy) agent.RetryPolicy {
	base.MaxAttempts = uint32(settings.MaxRetriesOrDefault()) + 1
	base.InitialDelay = time.Duration(settings.BaseDelayMSOrDefault()) * time.Millisecond
	return base
}

func optionalBoolEqual(value *bool, expected *bool) bool {
	return value != nil && expected != nil && *value == *expected
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func catalogModelRefs(models []model.Model) ([]provider.Model, error) {
	result := make([]provider.Model, len(models))
	for index, candidate := range models {
		ref, err := candidate.Ref()
		if err != nil {
			return nil, fmt.Errorf("resolve catalog model %s/%s: %w", candidate.Provider, candidate.ID, err)
		}
		result[index] = ref
	}
	return result, nil
}

func scopedModelRefs(models []model.ScopedModel) ([]agent.ScopedModel, error) {
	result := make([]agent.ScopedModel, len(models))
	for index, candidate := range models {
		ref, err := candidate.Model.Ref()
		if err != nil {
			return nil, fmt.Errorf("resolve scoped model %s/%s: %w", candidate.Model.Provider, candidate.Model.ID, err)
		}
		result[index] = agent.ScopedModel{Model: ref, ThinkingLevel: cloneThinkingPointer(candidate.ThinkingLevel)}
	}
	return result, nil
}

func branchHasThinkingEntry(manager *session.SessionManager) (bool, error) {
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

func exactCatalogModel(models []model.Model, providerID, modelID string) *model.Model {
	for _, candidate := range models {
		if candidate.Provider == providerID && candidate.ID == modelID {
			copy := candidate
			return &copy
		}
	}
	return nil
}

func selectScopedModel(scoped []model.ScopedModel, settings model.Settings, availability model.Availability) (*model.Model, *provider.ThinkingLevel) {
	var first *model.Model
	var firstThinking *provider.ThinkingLevel
	for _, candidate := range scoped {
		if !availability.Available(candidate.Model) {
			continue
		}
		if first == nil {
			copy := candidate.Model
			first = &copy
			firstThinking = cloneThinkingPointer(candidate.ThinkingLevel)
		}
		if candidate.Model.Provider == settings.DefaultProvider && candidate.Model.ID == settings.DefaultModel {
			copy := candidate.Model
			return &copy, cloneThinkingPointer(candidate.ThinkingLevel)
		}
	}
	return first, firstThinking
}

func cloneThinkingPointer(value *provider.ThinkingLevel) *provider.ThinkingLevel {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneCatalogModelPointer(value *model.Model) *model.Model {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func settingsThinkingPointer(level provider.ThinkingLevel) *provider.ThinkingLevel {
	if level == "" {
		return nil
	}
	copy := level
	return &copy
}

func formatNoModelsAvailableMessage(docsDir string) string {
	return "No models available. " + formatProviderLoginHelp(docsDir)
}

func formatNoModelSelectedMessage(docsDir string) string {
	return "No model selected.\n\n" + formatProviderLoginHelp(docsDir) + "\n\nThen use /model to select a model."
}

// FormatNoAPIKeyFoundMessage is shared by product model-access validators and
// the session factory guidance so both use the same installation docs paths.
func FormatNoAPIKeyFoundMessage(providerID, docsDir string) string {
	providerDisplay := providerID
	if providerDisplay == "unknown" {
		providerDisplay = "the selected model"
	}
	return "No API key found for " + providerDisplay + ".\n\n" + formatProviderLoginHelp(docsDir)
}

func formatProviderLoginHelp(docsDir string) string {
	if docsDir == "" {
		docsDir = "docs"
	}
	return "Use /login to log into a provider via OAuth or API key. See:\n  " +
		filepath.Join(docsDir, "providers.md") + "\n  " + filepath.Join(docsDir, "models.md")
}

func isNilProvider(value provider.Provider) bool {
	if value == nil {
		return true
	}
	return false
}
