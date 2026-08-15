package model

import (
	"path"
	"sort"
	"strings"

	"github.com/cat3399/pi-go/internal/provider"
)

const DefaultThinkingLevel = provider.ThinkingMedium

// ProviderDefault preserves pi's provider preference order. A map alone cannot
// represent this ordering, which is observable when more than one provider's
// default model is available.
type ProviderDefault struct {
	Provider string
	ModelID  string
}

var defaultModelPreferences = []ProviderDefault{
	{Provider: "amazon-bedrock", ModelID: "us.anthropic.claude-opus-4-6-v1"},
	{Provider: "ant-ling", ModelID: "Ring-2.6-1T"},
	{Provider: "anthropic", ModelID: "claude-opus-4-8"},
	{Provider: "openai", ModelID: "gpt-5.5"},
	{Provider: AzureOpenAIProviderID, ModelID: DefaultAzureOpenAIModel},
	{Provider: "openai-codex", ModelID: "gpt-5.5"},
	{Provider: "radius", ModelID: "auto"},
	{Provider: "nvidia", ModelID: "nvidia/nemotron-3-super-120b-a12b"},
	{Provider: "deepseek", ModelID: "deepseek-v4-pro"},
	{Provider: "google", ModelID: "gemini-3.1-pro-preview"},
	{Provider: "google-vertex", ModelID: "gemini-3.1-pro-preview"},
	{Provider: "github-copilot", ModelID: "gpt-5.4"},
	{Provider: "openrouter", ModelID: "moonshotai/kimi-k2.6"},
	{Provider: "vercel-ai-gateway", ModelID: "zai/glm-5.1"},
	{Provider: "xai", ModelID: "grok-4.5"},
	{Provider: "groq", ModelID: "openai/gpt-oss-120b"},
	{Provider: "cerebras", ModelID: "zai-glm-4.7"},
	{Provider: "zai", ModelID: "glm-5.1"},
	{Provider: "zai-coding-cn", ModelID: "glm-5.1"},
	{Provider: "mistral", ModelID: "devstral-medium-latest"},
	{Provider: "minimax", ModelID: "MiniMax-M2.7"},
	{Provider: "minimax-cn", ModelID: "MiniMax-M2.7"},
	{Provider: "moonshotai", ModelID: "kimi-k2.6"},
	{Provider: "moonshotai-cn", ModelID: "kimi-k2.6"},
	{Provider: "huggingface", ModelID: "moonshotai/Kimi-K2.6"},
	{Provider: "fireworks", ModelID: "accounts/fireworks/models/kimi-k2p6"},
	{Provider: "together", ModelID: "moonshotai/Kimi-K2.6"},
	{Provider: "opencode", ModelID: "kimi-k2.6"},
	{Provider: "opencode-go", ModelID: "kimi-k2.6"},
	{Provider: "kimi-coding", ModelID: "kimi-for-coding"},
	{Provider: "cloudflare-workers-ai", ModelID: "@cf/moonshotai/kimi-k2.6"},
	{Provider: "cloudflare-ai-gateway", ModelID: "workers-ai/@cf/moonshotai/kimi-k2.6"},
	{Provider: "qwen-token-plan", ModelID: "qwen3.7-max"},
	{Provider: "qwen-token-plan-cn", ModelID: "qwen3.7-max"},
	{Provider: "xiaomi", ModelID: "mimo-v2.5-pro"},
	{Provider: "xiaomi-token-plan-cn", ModelID: "mimo-v2.5-pro"},
	{Provider: "xiaomi-token-plan-ams", ModelID: "mimo-v2.5-pro"},
	{Provider: "xiaomi-token-plan-sgp", ModelID: "mimo-v2.5-pro"},
}

func DefaultModelPreferences() []ProviderDefault {
	return append([]ProviderDefault(nil), defaultModelPreferences...)
}

func DefaultModelID(providerID string) (string, bool) {
	for _, preference := range defaultModelPreferences {
		if preference.Provider == providerID {
			return preference.ModelID, true
		}
	}
	return "", false
}

// Availability is deliberately provider-neutral. Production supplies real
// credential and registered-route predicates; the model package never infers
// runnability from catalog presence.
type Availability struct {
	HasConfiguredAuth      func(providerID string) bool
	HasConfiguredModelAuth func(Model) bool
	SupportsRoute          func(Model) bool
}

func (a Availability) Available(model Model) bool {
	return a.hasAuth(model) && a.SupportsRoute != nil && a.SupportsRoute(model)
}

func (a Availability) hasAuth(model Model) bool {
	if a.HasConfiguredModelAuth != nil {
		return a.HasConfiguredModelAuth(model)
	}
	return a.HasConfiguredAuth != nil && a.HasConfiguredAuth(model.Provider)
}

func FilterAvailableModels(models []Model, availability Availability) []Model {
	result := make([]Model, 0, len(models))
	for _, candidate := range models {
		if availability.Available(candidate) {
			result = append(result, cloneModel(candidate))
		}
	}
	return result
}

type ScopedModel struct {
	Model         Model
	ThinkingLevel *provider.ThinkingLevel
}

type ParsedModelResult struct {
	Model         *Model
	ThinkingLevel *provider.ThinkingLevel
	Warning       string
}

type ParseModelPatternOptions struct {
	// nil and true match scope behavior. false is CLI strict behavior: an
	// invalid suffix remains part of the requested model id.
	AllowInvalidThinkingLevelFallback *bool
}

func FindExactModelReferenceMatch(reference string, models []Model) *Model {
	trimmed := strings.TrimSpace(reference)
	if trimmed == "" {
		return nil
	}
	canonicalMatches := matchingModels(models, func(candidate Model) bool {
		return strings.EqualFold(candidate.Provider+"/"+candidate.ID, trimmed)
	})
	if len(canonicalMatches) == 1 {
		return modelPointer(canonicalMatches[0])
	}
	if len(canonicalMatches) > 1 {
		return nil
	}
	if slash := strings.IndexByte(trimmed, '/'); slash >= 0 {
		providerID := strings.TrimSpace(trimmed[:slash])
		modelID := strings.TrimSpace(trimmed[slash+1:])
		if providerID != "" && modelID != "" {
			providerMatches := matchingModels(models, func(candidate Model) bool {
				return strings.EqualFold(candidate.Provider, providerID) && strings.EqualFold(candidate.ID, modelID)
			})
			if len(providerMatches) == 1 {
				return modelPointer(providerMatches[0])
			}
			if len(providerMatches) > 1 {
				return nil
			}
		}
	}
	idMatches := matchingModels(models, func(candidate Model) bool {
		return strings.EqualFold(candidate.ID, trimmed)
	})
	if len(idMatches) == 1 {
		return modelPointer(idMatches[0])
	}
	return nil
}

func ParseModelPattern(pattern string, models []Model, options ParseModelPatternOptions) ParsedModelResult {
	if matched := tryMatchModel(pattern, models); matched != nil {
		return ParsedModelResult{Model: matched}
	}
	lastColon := strings.LastIndexByte(pattern, ':')
	if lastColon < 0 {
		return ParsedModelResult{}
	}
	prefix, suffix := pattern[:lastColon], pattern[lastColon+1:]
	level := provider.ThinkingLevel(suffix)
	if level.Valid() {
		result := ParseModelPattern(prefix, models, options)
		if result.Model != nil && result.Warning == "" {
			result.ThinkingLevel = thinkingPointer(level)
		}
		return result
	}
	allowFallback := options.AllowInvalidThinkingLevelFallback == nil || *options.AllowInvalidThinkingLevelFallback
	if !allowFallback {
		return ParsedModelResult{}
	}
	result := ParseModelPattern(prefix, models, options)
	if result.Model != nil {
		result.ThinkingLevel = nil
		result.Warning = `Invalid thinking level "` + suffix + `" in pattern "` + pattern + `". Using default instead.`
	}
	return result
}

type ScopeDiagnosticCode string

const (
	ScopeNoMatch              ScopeDiagnosticCode = "no-match"
	ScopeInvalidThinkingLevel ScopeDiagnosticCode = "invalid-thinking-level"
)

type ScopeDiagnostic struct {
	Code    ScopeDiagnosticCode
	Message string
	Pattern string
}

type ScopeResult struct {
	ScopedModels []ScopedModel
	Diagnostics  []ScopeDiagnostic
}

func ResolveModelScope(patterns []string, availableModels []Model) ScopeResult {
	result := ScopeResult{}
	for _, pattern := range patterns {
		if containsGlob(pattern) {
			globPattern := pattern
			var thinking *provider.ThinkingLevel
			if colon := strings.LastIndexByte(pattern, ':'); colon >= 0 {
				level := provider.ThinkingLevel(pattern[colon+1:])
				if level.Valid() {
					globPattern = pattern[:colon]
					thinking = thinkingPointer(level)
				}
			}
			if exact := FindExactModelReferenceMatch(globPattern, availableModels); exact != nil {
				appendScopedUnique(&result.ScopedModels, *exact, thinking)
				continue
			}
			matched := false
			for _, candidate := range availableModels {
				if globMatch(globPattern, candidate.Provider+"/"+candidate.ID) || globMatch(globPattern, candidate.ID) {
					matched = true
					appendScopedUnique(&result.ScopedModels, candidate, thinking)
				}
			}
			if !matched {
				result.Diagnostics = append(result.Diagnostics, noMatchDiagnostic(pattern))
			}
			continue
		}
		parsed := ParseModelPattern(pattern, availableModels, ParseModelPatternOptions{})
		if parsed.Warning != "" {
			result.Diagnostics = append(result.Diagnostics, ScopeDiagnostic{
				Code: ScopeInvalidThinkingLevel, Message: parsed.Warning, Pattern: pattern,
			})
		}
		if parsed.Model == nil {
			result.Diagnostics = append(result.Diagnostics, noMatchDiagnostic(pattern))
			continue
		}
		appendScopedUnique(&result.ScopedModels, *parsed.Model, parsed.ThinkingLevel)
	}
	return result
}

type CLIModelOptions struct {
	Provider               string
	Model                  string
	ThinkingLevel          *provider.ThinkingLevel
	AllModels              []Model
	HasConfiguredAuth      func(providerID string) bool
	HasConfiguredModelAuth func(Model) bool
}

type CLIModelResult struct {
	Model         *Model
	ThinkingLevel *provider.ThinkingLevel
	Warning       string
	Error         string
}

func ResolveCLIModel(options CLIModelOptions) CLIModelResult {
	if options.Model == "" {
		return CLIModelResult{}
	}
	models := options.AllModels
	if len(models) == 0 {
		return CLIModelResult{Error: "No models available. Check your installation or add models to models.json."}
	}
	providerMap := make(map[string]string)
	for _, candidate := range models {
		providerMap[strings.ToLower(candidate.Provider)] = candidate.Provider
	}
	providerID := ""
	if options.Provider != "" {
		providerID = providerMap[strings.ToLower(options.Provider)]
		if providerID == "" {
			return CLIModelResult{Error: `Unknown provider "` + options.Provider + `". Use --list-models to see available providers/models.`}
		}
	}
	pattern := options.Model
	inferredProvider := false
	if providerID == "" {
		if slash := strings.IndexByte(options.Model, '/'); slash >= 0 {
			if canonical := providerMap[strings.ToLower(options.Model[:slash])]; canonical != "" {
				providerID = canonical
				pattern = options.Model[slash+1:]
				inferredProvider = true
			}
		}
	}
	if providerID == "" {
		if exact := firstRawExactModel(options.Model, models); exact != nil {
			return CLIModelResult{Model: exact}
		}
	}
	if options.Provider != "" && providerID != "" {
		prefix := providerID + "/"
		if len(options.Model) >= len(prefix) && strings.EqualFold(options.Model[:len(prefix)], prefix) {
			pattern = options.Model[len(prefix):]
		}
	}
	candidates := models
	if providerID != "" {
		candidates = candidates[:0:0]
		for _, candidate := range models {
			if candidate.Provider == providerID {
				candidates = append(candidates, candidate)
			}
		}
	}
	strict := false
	parsed := ParseModelPattern(pattern, candidates, ParseModelPatternOptions{AllowInvalidThinkingLevelFallback: &strict})
	if parsed.Model != nil {
		if inferredProvider {
			rawMatches := make([]Model, 0)
			for _, candidate := range models {
				if strings.EqualFold(candidate.ID, options.Model) && !modelsEqual(candidate, *parsed.Model) {
					rawMatches = append(rawMatches, candidate)
				}
			}
			if len(rawMatches) > 0 && !hasModelAuth(options, *parsed.Model) {
				authenticated := make([]Model, 0, len(rawMatches))
				for _, candidate := range rawMatches {
					if hasModelAuth(options, candidate) {
						authenticated = append(authenticated, candidate)
					}
				}
				if len(authenticated) == 1 {
					return CLIModelResult{Model: modelPointer(authenticated[0])}
				}
			}
		}
		return CLIModelResult{Model: parsed.Model, ThinkingLevel: parsed.ThinkingLevel, Warning: parsed.Warning}
	}
	if inferredProvider {
		if exact := firstRawExactModel(options.Model, models); exact != nil {
			return CLIModelResult{Model: exact}
		}
		fallback := ParseModelPattern(options.Model, models, ParseModelPatternOptions{AllowInvalidThinkingLevelFallback: &strict})
		if fallback.Model != nil {
			return CLIModelResult{Model: fallback.Model, ThinkingLevel: fallback.ThinkingLevel, Warning: fallback.Warning}
		}
	}
	if providerID != "" {
		fallbackPattern := pattern
		var fallbackThinking *provider.ThinkingLevel
		if options.ThinkingLevel == nil {
			if colon := strings.LastIndexByte(pattern, ':'); colon >= 0 {
				level := provider.ThinkingLevel(pattern[colon+1:])
				if level.Valid() {
					fallbackPattern = pattern[:colon]
					fallbackThinking = thinkingPointer(level)
				}
			}
		}
		if fallback := buildFallbackModel(providerID, fallbackPattern, models); fallback != nil {
			requestedThinking := options.ThinkingLevel
			if requestedThinking == nil {
				requestedThinking = fallbackThinking
			}
			if requestedThinking != nil && *requestedThinking != provider.ThinkingOff {
				fallback.Reasoning = true
			}
			warning := `Model "` + fallbackPattern + `" not found for provider "` + providerID + `". Using custom model id.`
			if parsed.Warning != "" {
				warning = parsed.Warning + " " + warning
			}
			return CLIModelResult{Model: fallback, ThinkingLevel: fallbackThinking, Warning: warning}
		}
	}
	display := options.Model
	if providerID != "" {
		display = providerID + "/" + pattern
	}
	return CLIModelResult{Warning: parsed.Warning, Error: `Model "` + display + `" not found. Use --list-models to see available models.`}
}

type InitialModelOptions struct {
	CLIProvider          string
	CLIModel             string
	CLIThinkingLevel     *provider.ThinkingLevel
	ScopePatterns        []string
	IsContinuing         bool
	DefaultProvider      string
	DefaultModelID       string
	DefaultThinkingLevel *provider.ThinkingLevel
	AllModels            []Model
	Availability         Availability
}

type InitialModelResult struct {
	Model         *Model
	ThinkingLevel provider.ThinkingLevel
	Warning       string
	Error         string
	Scope         ScopeResult
}

// ResolveInitialModel combines main.ts' scope/default choice with SDK's final
// thinking selection and findInitialModel's provider-default fallback. Its
// ThinkingLevel is therefore the value entering capability clamping, not the
// intermediate thinking value returned by findInitialModel. Session model
// restoration remains separate because SDK performs it before fallback.
func ResolveInitialModel(options InitialModelOptions) InitialModelResult {
	var available []Model
	availabilityResolved := false
	scope := ScopeResult{}
	if len(options.ScopePatterns) > 0 {
		available = FilterAvailableModels(options.AllModels, options.Availability)
		availabilityResolved = true
		scope = ResolveModelScope(options.ScopePatterns, available)
	}
	if options.CLIModel != "" {
		resolved := ResolveCLIModel(CLIModelOptions{
			Provider: options.CLIProvider, Model: options.CLIModel, ThinkingLevel: options.CLIThinkingLevel,
			AllModels: options.AllModels, HasConfiguredAuth: options.Availability.HasConfiguredAuth,
			HasConfiguredModelAuth: options.Availability.HasConfiguredModelAuth,
		})
		thinking := resolveInitialThinking(options.CLIThinkingLevel, resolved.ThinkingLevel, options.DefaultThinkingLevel)
		if resolved.Model != nil && (options.Availability.SupportsRoute == nil || !options.Availability.SupportsRoute(*resolved.Model)) {
			resolved.Error = `Model "` + resolved.Model.Provider + `/` + resolved.Model.ID + `" is not supported by a registered provider route.`
			resolved.Model = nil
		}
		return InitialModelResult{Model: resolved.Model, ThinkingLevel: thinking, Warning: resolved.Warning, Error: resolved.Error, Scope: scope}
	}
	if len(scope.ScopedModels) > 0 && !options.IsContinuing {
		selected := scope.ScopedModels[0]
		if options.DefaultProvider != "" && options.DefaultModelID != "" {
			for _, scoped := range scope.ScopedModels {
				if scoped.Model.Provider == options.DefaultProvider && scoped.Model.ID == options.DefaultModelID {
					selected = scoped
					break
				}
			}
		}
		thinking := resolveInitialThinking(options.CLIThinkingLevel, selected.ThinkingLevel, options.DefaultThinkingLevel)
		return InitialModelResult{Model: modelPointer(selected.Model), ThinkingLevel: thinking, Scope: scope}
	}
	if options.DefaultProvider != "" && options.DefaultModelID != "" {
		if saved := exactProviderModel(options.AllModels, options.DefaultProvider, options.DefaultModelID); saved != nil && options.Availability.Available(*saved) {
			thinking := resolveInitialThinking(options.CLIThinkingLevel, nil, options.DefaultThinkingLevel)
			return InitialModelResult{Model: saved, ThinkingLevel: thinking, Scope: scope}
		}
	}
	if !availabilityResolved {
		available = FilterAvailableModels(options.AllModels, options.Availability)
	}
	if selected := preferredAvailableModel(available); selected != nil {
		return InitialModelResult{
			Model: selected, ThinkingLevel: resolveInitialThinking(options.CLIThinkingLevel, nil, options.DefaultThinkingLevel), Scope: scope,
		}
	}
	return InitialModelResult{ThinkingLevel: resolveInitialThinking(options.CLIThinkingLevel, nil, options.DefaultThinkingLevel), Scope: scope}
}

func resolveInitialThinking(cli, selected, settings *provider.ThinkingLevel) provider.ThinkingLevel {
	if cli != nil {
		return *cli
	}
	if selected != nil {
		return *selected
	}
	if settings != nil {
		return *settings
	}
	return DefaultThinkingLevel
}

type RestoreModelOptions struct {
	SavedProvider string
	SavedModelID  string
	CurrentModel  *Model
	AllModels     []Model
	Availability  Availability
}

type RestoreModelResult struct {
	Model           *Model
	FallbackMessage string
	Reason          string
}

func RestoreModelFromSession(options RestoreModelOptions) RestoreModelResult {
	restored := exactProviderModel(options.AllModels, options.SavedProvider, options.SavedModelID)
	if restored != nil && options.Availability.Available(*restored) {
		return RestoreModelResult{Model: restored}
	}
	reason := "model no longer exists"
	if restored != nil {
		switch {
		case !options.Availability.hasAuth(*restored):
			reason = "no auth configured"
		case options.Availability.SupportsRoute == nil || !options.Availability.SupportsRoute(*restored):
			reason = "model route is not registered"
		}
	}
	if options.CurrentModel != nil {
		current := modelPointer(*options.CurrentModel)
		return RestoreModelResult{Model: current, Reason: reason, FallbackMessage: restoreFallbackMessage(options, reason, current)}
	}
	if fallback := preferredAvailableModel(FilterAvailableModels(options.AllModels, options.Availability)); fallback != nil {
		return RestoreModelResult{Model: fallback, Reason: reason, FallbackMessage: restoreFallbackMessage(options, reason, fallback)}
	}
	return RestoreModelResult{Reason: reason}
}

func restoreFallbackMessage(options RestoreModelOptions, reason string, fallback *Model) string {
	return "Could not restore model " + options.SavedProvider + "/" + options.SavedModelID + " (" + reason + "). Using " + fallback.Provider + "/" + fallback.ID + "."
}

func tryMatchModel(pattern string, models []Model) *Model {
	if exact := FindExactModelReferenceMatch(pattern, models); exact != nil {
		return exact
	}
	lower := strings.ToLower(pattern)
	matches := make([]Model, 0)
	for _, candidate := range models {
		if strings.Contains(strings.ToLower(candidate.ID), lower) || strings.Contains(strings.ToLower(candidate.Name), lower) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 0 {
		return nil
	}
	aliases := make([]Model, 0, len(matches))
	dated := make([]Model, 0, len(matches))
	for _, candidate := range matches {
		if isAlias(candidate.ID) {
			aliases = append(aliases, candidate)
		} else {
			dated = append(dated, candidate)
		}
	}
	selected := dated
	if len(aliases) > 0 {
		selected = aliases
	}
	sort.SliceStable(selected, func(i, j int) bool { return selected[i].ID > selected[j].ID })
	return modelPointer(selected[0])
}

func isAlias(id string) bool {
	if strings.HasSuffix(id, "-latest") {
		return true
	}
	if len(id) < 9 || id[len(id)-9] != '-' {
		return true
	}
	for _, character := range id[len(id)-8:] {
		if character < '0' || character > '9' {
			return true
		}
	}
	return false
}

func buildFallbackModel(providerID, modelID string, models []Model) *Model {
	providerModels := make([]Model, 0)
	for _, candidate := range models {
		if candidate.Provider == providerID {
			providerModels = append(providerModels, candidate)
		}
	}
	if len(providerModels) == 0 {
		return nil
	}
	base := providerModels[0]
	if defaultID, ok := DefaultModelID(providerID); ok {
		for _, candidate := range providerModels {
			if candidate.ID == defaultID {
				base = candidate
				break
			}
		}
	}
	base = cloneModel(base)
	base.ID, base.Name = modelID, modelID
	return &base
}

func preferredAvailableModel(available []Model) *Model {
	for _, preference := range defaultModelPreferences {
		for _, candidate := range available {
			if candidate.Provider == preference.Provider && candidate.ID == preference.ModelID {
				return modelPointer(candidate)
			}
		}
	}
	if len(available) > 0 {
		return modelPointer(available[0])
	}
	return nil
}

func exactProviderModel(models []Model, providerID, modelID string) *Model {
	for _, candidate := range models {
		if candidate.Provider == providerID && candidate.ID == modelID {
			return modelPointer(candidate)
		}
	}
	return nil
}

func firstRawExactModel(reference string, models []Model) *Model {
	for _, candidate := range models {
		if strings.EqualFold(candidate.ID, reference) || strings.EqualFold(candidate.Provider+"/"+candidate.ID, reference) {
			return modelPointer(candidate)
		}
	}
	return nil
}

func matchingModels(models []Model, match func(Model) bool) []Model {
	result := make([]Model, 0)
	for _, candidate := range models {
		if match(candidate) {
			result = append(result, candidate)
		}
	}
	return result
}

func appendScopedUnique(scoped *[]ScopedModel, candidate Model, thinking *provider.ThinkingLevel) {
	for _, existing := range *scoped {
		if modelsEqual(existing.Model, candidate) {
			return
		}
	}
	copy := cloneModel(candidate)
	*scoped = append(*scoped, ScopedModel{Model: copy, ThinkingLevel: cloneThinkingPointer(thinking)})
}

func containsGlob(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[")
}

func globMatch(pattern, value string) bool {
	matched, err := path.Match(strings.ToLower(pattern), strings.ToLower(value))
	return err == nil && matched
}

func noMatchDiagnostic(pattern string) ScopeDiagnostic {
	return ScopeDiagnostic{Code: ScopeNoMatch, Message: `No models match pattern "` + pattern + `"`, Pattern: pattern}
}

func modelsEqual(left, right Model) bool {
	return left.Provider == right.Provider && left.ID == right.ID
}

func modelPointer(model Model) *Model {
	copy := cloneModel(model)
	return &copy
}

func thinkingPointer(level provider.ThinkingLevel) *provider.ThinkingLevel {
	copy := level
	return &copy
}

func cloneThinkingPointer(level *provider.ThinkingLevel) *provider.ThinkingLevel {
	if level == nil {
		return nil
	}
	return thinkingPointer(*level)
}

func hasAuth(predicate func(string) bool, providerID string) bool {
	return predicate != nil && predicate(providerID)
}

func hasModelAuth(options CLIModelOptions, candidate Model) bool {
	if options.HasConfiguredModelAuth != nil {
		return options.HasConfiguredModelAuth(candidate)
	}
	return hasAuth(options.HasConfiguredAuth, candidate.Provider)
}
