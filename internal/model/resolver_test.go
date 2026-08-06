package model

import (
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/provider"
)

func resolverModel(providerID, id, name string) Model {
	return Model{
		Provider: providerID, ID: id, Name: name, API: provider.OpenAICompletionsAPI,
		BaseURL: "https://" + providerID + ".invalid/v1", Input: []provider.InputKind{provider.InputText},
		ContextWindow: 128_000, MaxTokens: 8_192,
	}
}

func resolverModels() []Model {
	return []Model{
		resolverModel("anthropic", "claude-sonnet-4-5", "Claude Sonnet 4.5"),
		resolverModel("openai", "gpt-4o", "GPT-4o"),
		resolverModel("openrouter", "qwen/qwen3-coder:exacto", "Qwen3 Coder Exacto"),
		resolverModel("openrouter", "openai/gpt-4o:extended", "GPT-4o Extended"),
	}
}

func levelValue(level *provider.ThinkingLevel) string {
	if level == nil {
		return ""
	}
	return string(*level)
}

func TestFindExactModelReferenceMatchRejectsAmbiguousBareIDs(t *testing.T) {
	models := []Model{resolverModel("one", "shared", "One"), resolverModel("two", "shared", "Two")}
	if got := FindExactModelReferenceMatch("shared", models); got != nil {
		t.Fatalf("ambiguous bare id = %#v", got)
	}
	got := FindExactModelReferenceMatch(" TWO / shared ", models)
	if got == nil || got.Provider != "two" || got.ID != "shared" {
		t.Fatalf("canonical reference = %#v", got)
	}
	if got := FindExactModelReferenceMatch("", models); got != nil {
		t.Fatalf("empty exact reference = %#v", got)
	}
}

func TestParseModelPatternMatchesOriginalTable(t *testing.T) {
	models := resolverModels()
	tests := []struct {
		name, pattern, providerID, id, thinking, warning string
	}{
		{name: "exact", pattern: "claude-sonnet-4-5", providerID: "anthropic", id: "claude-sonnet-4-5"},
		{name: "partial", pattern: "sonnet", providerID: "anthropic", id: "claude-sonnet-4-5"},
		{name: "valid suffix", pattern: "sonnet:high", providerID: "anthropic", id: "claude-sonnet-4-5", thinking: "high"},
		{name: "invalid suffix", pattern: "gpt-4o:invalid", providerID: "openai", id: "gpt-4o", warning: "Invalid thinking level"},
		{name: "colon id", pattern: "qwen/qwen3-coder:exacto", providerID: "openrouter", id: "qwen/qwen3-coder:exacto"},
		{name: "canonical colon id", pattern: "openrouter/qwen/qwen3-coder:exacto", providerID: "openrouter", id: "qwen/qwen3-coder:exacto"},
		{name: "colon id thinking", pattern: "qwen/qwen3-coder:exacto:high", providerID: "openrouter", id: "qwen/qwen3-coder:exacto", thinking: "high"},
		{name: "nested invalid", pattern: "qwen/qwen3-coder:exacto:high:random", providerID: "openrouter", id: "qwen/qwen3-coder:exacto", warning: "Invalid thinking level"},
		{name: "slash colon id", pattern: "openai/gpt-4o:extended", providerID: "openrouter", id: "openai/gpt-4o:extended"},
		{name: "empty suffix", pattern: "sonnet:", providerID: "anthropic", id: "claude-sonnet-4-5", warning: "Invalid thinking level"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			got := ParseModelPattern(testCase.pattern, models, ParseModelPatternOptions{})
			if got.Model == nil || got.Model.Provider != testCase.providerID || got.Model.ID != testCase.id || levelValue(got.ThinkingLevel) != testCase.thinking {
				t.Fatalf("ParseModelPattern(%q) = %#v", testCase.pattern, got)
			}
			if testCase.warning == "" && got.Warning != "" || testCase.warning != "" && !strings.Contains(got.Warning, testCase.warning) {
				t.Fatalf("warning = %q", got.Warning)
			}
		})
	}
	if got := ParseModelPattern("missing", models, ParseModelPatternOptions{}); got.Model != nil || got.ThinkingLevel != nil || got.Warning != "" {
		t.Fatalf("missing = %#v", got)
	}
	for _, level := range []provider.ThinkingLevel{provider.ThinkingOff, provider.ThinkingMinimal, provider.ThinkingLow, provider.ThinkingMedium, provider.ThinkingHigh, provider.ThinkingXHigh, provider.ThinkingMax} {
		got := ParseModelPattern("sonnet:"+string(level), models, ParseModelPatternOptions{})
		if got.Model == nil || got.Model.ID != "claude-sonnet-4-5" || got.ThinkingLevel == nil || *got.ThinkingLevel != level || got.Warning != "" {
			t.Fatalf("thinking %q = %#v", level, got)
		}
	}
	// Empty is a partial match in pi; it selects the highest-sorting alias.
	if got := ParseModelPattern("", models, ParseModelPatternOptions{}); got.Model == nil {
		t.Fatal("empty pattern did not use partial matching")
	}
	strict := false
	if got := ParseModelPattern("gpt-4o:invalid", models, ParseModelPatternOptions{AllowInvalidThinkingLevelFallback: &strict}); got.Model != nil || got.Warning != "" {
		t.Fatalf("strict invalid suffix = %#v", got)
	}
}

func TestParseModelPatternPrefersAliasesThenNewestDate(t *testing.T) {
	models := []Model{
		resolverModel("p", "sonnet-20241022", "dated old"),
		resolverModel("p", "sonnet-20250929", "dated new"),
		resolverModel("p", "sonnet-latest", "alias latest"),
		resolverModel("p", "sonnet-z", "alias z"),
	}
	if got := ParseModelPattern("sonnet", models, ParseModelPatternOptions{}); got.Model == nil || got.Model.ID != "sonnet-z" {
		t.Fatalf("alias selection = %#v", got)
	}
	if got := ParseModelPattern("dated", models[:2], ParseModelPatternOptions{}); got.Model == nil || got.Model.ID != "sonnet-20250929" {
		t.Fatalf("dated selection = %#v", got)
	}
}

func TestResolveModelScopeDiagnosticsGlobOrderDedupAndBrackets(t *testing.T) {
	models := append(resolverModels(), resolverModel("custom", "bracketed-model[1m]", "Bracketed Model"))
	result := ResolveModelScope([]string{
		"sonnet:high", "gpt-4o:invalid", "missing", "OPENROUTER/qwen/*:low",
		"custom/bracketed-model[1m]:high", "sonnet:off",
	}, models)
	want := []struct {
		providerID, id, thinking string
	}{
		{"anthropic", "claude-sonnet-4-5", "high"},
		{"openai", "gpt-4o", ""},
		{"openrouter", "qwen/qwen3-coder:exacto", "low"},
		{"custom", "bracketed-model[1m]", "high"},
	}
	if len(result.ScopedModels) != len(want) {
		t.Fatalf("scope = %#v", result)
	}
	for index, expected := range want {
		got := result.ScopedModels[index]
		if got.Model.Provider != expected.providerID || got.Model.ID != expected.id || levelValue(got.ThinkingLevel) != expected.thinking {
			t.Fatalf("scope[%d] = %#v", index, got)
		}
	}
	if len(result.Diagnostics) != 2 || result.Diagnostics[0].Code != ScopeInvalidThinkingLevel || result.Diagnostics[1].Code != ScopeNoMatch {
		t.Fatalf("diagnostics = %#v", result.Diagnostics)
	}
	if result.Diagnostics[0].Pattern != "gpt-4o:invalid" || result.Diagnostics[1].Pattern != "missing" {
		t.Fatalf("diagnostic order = %#v", result.Diagnostics)
	}
}

func TestResolveModelScopeQuestionAndCharacterClassGlobs(t *testing.T) {
	models := []Model{
		resolverModel("custom", "model-a1", "A1"),
		resolverModel("custom", "model-b2", "B2"),
		resolverModel("other", "model-a1", "Other"),
	}
	result := ResolveModelScope([]string{"CUSTOM/model-?1", "custom/model-[ab]2"}, models)
	if len(result.Diagnostics) != 0 || len(result.ScopedModels) != 2 ||
		result.ScopedModels[0].Model.ID != "model-a1" || result.ScopedModels[1].Model.ID != "model-b2" {
		t.Fatalf("glob scope = %#v", result)
	}
}

func TestAvailabilityRequiresBothRealPredicates(t *testing.T) {
	models := []Model{resolverModel("auth-only", "one", "one"), resolverModel("route-only", "two", "two"), resolverModel("ready", "three", "three")}
	availability := Availability{
		HasConfiguredAuth: func(providerID string) bool { return providerID != "route-only" },
		SupportsRoute:     func(model Model) bool { return model.Provider != "auth-only" },
	}
	got := FilterAvailableModels(models, availability)
	if len(got) != 1 || got[0].Provider != "ready" {
		t.Fatalf("available = %#v", got)
	}
	if got := FilterAvailableModels(models, Availability{}); len(got) != 0 {
		t.Fatalf("nil predicates must fail closed: %#v", got)
	}
}

func TestResolveCLIModelMatchesOriginalProviderAndRawIDRules(t *testing.T) {
	models := resolverModels()
	tests := []struct {
		name, cliProvider, cliModel, providerID, id, thinking string
	}{
		{name: "provider slash", cliModel: "openai/gpt-4o", providerID: "openai", id: "gpt-4o"},
		{name: "case insensitive canonical", cliModel: "OPENAI/GPT-4O", providerID: "openai", id: "gpt-4o"},
		{name: "provider fuzzy", cliProvider: "OPENAI", cliModel: "4o", providerID: "openai", id: "gpt-4o"},
		{name: "thinking", cliModel: "sonnet:high", providerID: "anthropic", id: "claude-sonnet-4-5", thinking: "high"},
		{name: "raw slash id", cliModel: "openai/gpt-4o:extended", providerID: "openrouter", id: "openai/gpt-4o:extended"},
		{name: "strict invalid custom", cliProvider: "openai", cliModel: "gpt-4o:extended", providerID: "openai", id: "gpt-4o:extended"},
		{name: "strip duplicate provider", cliProvider: "openrouter", cliModel: "openrouter/openai/ghost-model", providerID: "openrouter", id: "openai/ghost-model"},
		{name: "provider fuzzy slash", cliModel: "openrouter/qwen", providerID: "openrouter", id: "qwen/qwen3-coder:exacto"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			got := ResolveCLIModel(CLIModelOptions{Provider: testCase.cliProvider, Model: testCase.cliModel, AllModels: models})
			if got.Error != "" || got.Model == nil || got.Model.Provider != testCase.providerID || got.Model.ID != testCase.id || levelValue(got.ThinkingLevel) != testCase.thinking {
				t.Fatalf("ResolveCLIModel = %#v", got)
			}
		})
	}
	if got := ResolveCLIModel(CLIModelOptions{Provider: "missing", Model: "x", AllModels: models}); !strings.Contains(got.Error, "Unknown provider") {
		t.Fatalf("unknown provider = %#v", got)
	}
	if got := ResolveCLIModel(CLIModelOptions{Provider: "openai", Model: "x"}); !strings.Contains(got.Error, "No models available") {
		t.Fatalf("empty catalog = %#v", got)
	}
}

func TestResolveCLIModelProviderInferenceAndAuthenticationTieBreak(t *testing.T) {
	models := append(resolverModels(),
		resolverModel("zai", "glm-5", "GLM-5"),
		resolverModel("vercel-ai-gateway", "zai/glm-5", "GLM-5 Gateway"),
		resolverModel("xiaomi", "mimo-v2.5-pro", "Xiaomi MiMo"),
		resolverModel("commandcode", "xiaomi/mimo-v2.5-pro", "Xiaomi via Commandcode"),
	)
	got := ResolveCLIModel(CLIModelOptions{Model: "zai/glm-5", AllModels: models, HasConfiguredAuth: func(string) bool { return true }})
	if got.Model == nil || got.Model.Provider != "zai" || got.Model.ID != "glm-5" {
		t.Fatalf("provider split = %#v", got)
	}
	got = ResolveCLIModel(CLIModelOptions{
		Model: "xiaomi/mimo-v2.5-pro", AllModels: models,
		HasConfiguredAuth: func(providerID string) bool { return providerID == "commandcode" },
	})
	if got.Model == nil || got.Model.Provider != "commandcode" || got.Model.ID != "xiaomi/mimo-v2.5-pro" {
		t.Fatalf("authenticated raw id = %#v", got)
	}
}

func TestResolveCLIModelBareCrossProviderIDKeepsOriginalFirstExactBehavior(t *testing.T) {
	models := []Model{resolverModel("first", "shared", "First"), resolverModel("second", "shared", "Second")}
	got := ResolveCLIModel(CLIModelOptions{Model: "shared", AllModels: models})
	if got.Model == nil || got.Model.Provider != "first" {
		t.Fatalf("CLI first exact match = %#v", got)
	}
}

func TestResolveCLIModelCustomFallbackThinkingSemantics(t *testing.T) {
	base := resolverModel("neuralwatt", "some-base-model", "Some Base Model")
	models := append(resolverModels(), base)
	for _, level := range []provider.ThinkingLevel{provider.ThinkingOff, provider.ThinkingMinimal, provider.ThinkingLow, provider.ThinkingMedium, provider.ThinkingHigh, provider.ThinkingXHigh, provider.ThinkingMax} {
		got := ResolveCLIModel(CLIModelOptions{Model: "neuralwatt/zai-org/GLM-5.1-FP8:" + string(level), AllModels: models})
		if got.Error != "" || got.Model == nil || got.Model.ID != "zai-org/GLM-5.1-FP8" || got.ThinkingLevel == nil || *got.ThinkingLevel != level {
			t.Fatalf("custom thinking %q = %#v", level, got)
		}
		if level != provider.ThinkingOff && !got.Model.Reasoning {
			t.Fatalf("custom thinking %q did not enable reasoning", level)
		}
	}
	got := ResolveCLIModel(CLIModelOptions{Model: "neuralwatt/zai-org/GLM-5.1-FP8:banana", AllModels: models})
	if got.Model == nil || got.Model.ID != "zai-org/GLM-5.1-FP8:banana" || got.ThinkingLevel != nil {
		t.Fatalf("invalid suffix = %#v", got)
	}
	explicit := provider.ThinkingMedium
	got = ResolveCLIModel(CLIModelOptions{Model: "neuralwatt/zai-org/GLM-5.1-FP8:high", ThinkingLevel: &explicit, AllModels: models})
	if got.Model == nil || got.Model.ID != "zai-org/GLM-5.1-FP8:high" || got.ThinkingLevel != nil {
		t.Fatalf("explicit thinking = %#v", got)
	}
	if got.Model.BaseURL != base.BaseURL || got.Model.API != base.API {
		t.Fatalf("custom did not inherit provider base metadata: %#v", got.Model)
	}
}

func TestResolveInitialModelPriorityAndAllVersusAvailable(t *testing.T) {
	high := provider.ThinkingHigh
	low := provider.ThinkingLow
	models := []Model{
		resolverModel("anthropic", "claude-opus-4-8", "Claude Opus"),
		resolverModel("openai", "gpt-5.5", "GPT"),
		resolverModel("deepseek", "deepseek-v4-pro", "DeepSeek"),
		resolverModel("local", "deepseek-v4-pro", "Local DeepSeek"),
	}
	availability := Availability{
		HasConfiguredAuth: func(providerID string) bool { return providerID == "openai" || providerID == "local" },
		SupportsRoute:     func(model Model) bool { return model.Provider != "deepseek" },
	}
	// CLI resolves against all models even when auth is not preconfigured, as
	// --api-key may be applied by the factory afterward.
	got := ResolveInitialModel(InitialModelOptions{CLIModel: "anthropic/claude-opus-4-8", CLIThinkingLevel: &low, AllModels: models, Availability: availability})
	if got.Model == nil || got.Model.Provider != "anthropic" || got.ThinkingLevel != low {
		t.Fatalf("CLI all-model selection = %#v", got)
	}
	// A route still must actually be registered.
	blocked := ResolveInitialModel(InitialModelOptions{CLIModel: "deepseek/deepseek-v4-pro", AllModels: models, Availability: availability})
	if blocked.Model != nil || !strings.Contains(blocked.Error, "registered provider route") {
		t.Fatalf("unregistered CLI route = %#v", blocked)
	}
	// On a new session, scope wins, while the saved default wins inside scope.
	got = ResolveInitialModel(InitialModelOptions{
		ScopePatterns: []string{"local/*:low", "openai/*:high"}, DefaultProvider: "openai", DefaultModelID: "gpt-5.5",
		DefaultThinkingLevel: &high, AllModels: models, Availability: availability,
	})
	if got.Model == nil || got.Model.Provider != "openai" || got.ThinkingLevel != high {
		t.Fatalf("new-session scoped default = %#v", got)
	}
	// Continuing skips scope. An unavailable saved default is ignored, then the
	// ordered provider defaults select OpenAI ahead of the local first model.
	got = ResolveInitialModel(InitialModelOptions{
		ScopePatterns: []string{"local/*:low"}, IsContinuing: true,
		DefaultProvider: "deepseek", DefaultModelID: "deepseek-v4-pro", DefaultThinkingLevel: &high,
		AllModels: models, Availability: availability,
	})
	if got.Model == nil || got.Model.Provider != "openai" || got.Model.ID != "gpt-5.5" || got.ThinkingLevel != high {
		t.Fatalf("continuing fallback = %#v", got)
	}
}

func TestResolveInitialModelFinalThinkingPriorityAcrossSelectionBranches(t *testing.T) {
	models := []Model{
		resolverModel("openai", "gpt-5.5", "GPT"),
		resolverModel("custom", "first", "First"),
	}
	available := Availability{HasConfiguredAuth: func(string) bool { return true }, SupportsRoute: func(Model) bool { return true }}
	low, high, xhigh, maximum := provider.ThinkingLow, provider.ThinkingHigh, provider.ThinkingXHigh, provider.ThinkingMax
	tests := []struct {
		name    string
		options InitialModelOptions
		want    provider.ThinkingLevel
	}{
		{
			name:    "CLI thinking without explicit model applies to provider default",
			options: InitialModelOptions{CLIThinkingLevel: &xhigh},
			want:    xhigh,
		},
		{
			name:    "settings thinking applies to provider default fallback",
			options: InitialModelOptions{DefaultThinkingLevel: &high},
			want:    high,
		},
		{
			name:    "scope suffix overrides settings",
			options: InitialModelOptions{ScopePatterns: []string{"openai/*:low"}, DefaultThinkingLevel: &high},
			want:    low,
		},
		{
			name:    "CLI thinking overrides scope suffix",
			options: InitialModelOptions{ScopePatterns: []string{"openai/*:low"}, CLIThinkingLevel: &maximum, DefaultThinkingLevel: &high},
			want:    maximum,
		},
		{
			name:    "CLI thinking overrides settings model",
			options: InitialModelOptions{DefaultProvider: "openai", DefaultModelID: "gpt-5.5", CLIThinkingLevel: &xhigh, DefaultThinkingLevel: &high},
			want:    xhigh,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.options.AllModels = models
			testCase.options.Availability = available
			got := ResolveInitialModel(testCase.options)
			if got.Model == nil || got.ThinkingLevel != testCase.want {
				t.Fatalf("ResolveInitialModel = %#v, want thinking %q", got, testCase.want)
			}
		})
	}
	// The resolver returns the final pre-clamp preference even when no model is
	// available. The factory is responsible for clamping that case to off.
	got := ResolveInitialModel(InitialModelOptions{
		CLIThinkingLevel: &low, AllModels: models,
		Availability: Availability{HasConfiguredAuth: func(string) bool { return false }, SupportsRoute: func(Model) bool { return true }},
	})
	if got.Model != nil || got.ThinkingLevel != low {
		t.Fatalf("model-less pre-clamp thinking = %#v", got)
	}
}

func TestResolveInitialSettingsDefaultUsesStrictProviderAndID(t *testing.T) {
	models := []Model{
		resolverModel("anthropic", "claude-opus-4-8", "Claude"),
		resolverModel("openai", "gpt-5.5", "GPT"),
	}
	availability := Availability{HasConfiguredAuth: func(string) bool { return true }, SupportsRoute: func(Model) bool { return true }}
	got := ResolveInitialModel(InitialModelOptions{
		DefaultProvider: "OPENAI", DefaultModelID: "GPT-5.5", AllModels: models, Availability: availability,
	})
	if got.Model == nil || got.Model.Provider != "anthropic" {
		t.Fatalf("case-mismatched settings default was restored instead of falling back: %#v", got)
	}
}

func TestResolveInitialExplicitCLIWithoutScopeDoesNotEnumerateAvailability(t *testing.T) {
	model := resolverModel("openai", "gpt-5.5", "GPT")
	authChecks := 0
	got := ResolveInitialModel(InitialModelOptions{
		CLIModel: "openai/gpt-5.5", AllModels: []Model{model},
		Availability: Availability{
			HasConfiguredAuth: func(string) bool { authChecks++; return true },
			SupportsRoute:     func(Model) bool { return true },
		},
	})
	if got.Model == nil || authChecks != 0 {
		t.Fatalf("explicit CLI unexpectedly enumerated availability: %#v, checks=%d", got, authChecks)
	}
}

func TestResolveInitialModelUsesAvailableInputOrderAfterProviderDefaults(t *testing.T) {
	models := []Model{resolverModel("custom-b", "b", "B"), resolverModel("custom-a", "a", "A")}
	availability := Availability{HasConfiguredAuth: func(string) bool { return true }, SupportsRoute: func(Model) bool { return true }}
	got := ResolveInitialModel(InitialModelOptions{AllModels: models, Availability: availability})
	if got.Model == nil || got.Model.Provider != "custom-b" {
		t.Fatalf("first available = %#v", got)
	}
	if DefaultThinkingLevel != provider.ThinkingMedium {
		t.Fatalf("default thinking = %q", DefaultThinkingLevel)
	}
}

func TestRestoreModelFromSessionExactAvailabilityAndFallback(t *testing.T) {
	models := []Model{
		resolverModel("anthropic", "claude-opus-4-8", "Claude"),
		resolverModel("openai", "gpt-5.5", "GPT"),
		resolverModel("blocked", "saved", "Blocked"),
	}
	availability := Availability{
		HasConfiguredAuth: func(providerID string) bool { return providerID != "blocked" },
		SupportsRoute:     func(Model) bool { return true },
	}
	got := RestoreModelFromSession(RestoreModelOptions{SavedProvider: "openai", SavedModelID: "gpt-5.5", AllModels: models, Availability: availability})
	if got.Model == nil || got.Model.Provider != "openai" || got.FallbackMessage != "" {
		t.Fatalf("restored = %#v", got)
	}
	got = RestoreModelFromSession(RestoreModelOptions{SavedProvider: "OPENAI", SavedModelID: "GPT-5.5", AllModels: models, Availability: availability})
	if got.Model == nil || got.Model.Provider != "anthropic" || got.Reason != "model no longer exists" || got.FallbackMessage == "" {
		t.Fatalf("case-mismatched saved model did not use fallback: %#v", got)
	}
	got = RestoreModelFromSession(RestoreModelOptions{SavedProvider: "blocked", SavedModelID: "saved", AllModels: models, Availability: availability})
	if got.Model == nil || got.Model.Provider != "anthropic" || got.Reason != "no auth configured" || !strings.Contains(got.FallbackMessage, "Using anthropic/claude-opus-4-8") {
		t.Fatalf("auth fallback = %#v", got)
	}
	current := resolverModel("current", "chosen", "Current")
	got = RestoreModelFromSession(RestoreModelOptions{SavedProvider: "gone", SavedModelID: "missing", CurrentModel: &current, AllModels: models, Availability: availability})
	if got.Model == nil || got.Model.Provider != "current" || got.Reason != "model no longer exists" {
		t.Fatalf("current fallback = %#v", got)
	}
	got = RestoreModelFromSession(RestoreModelOptions{SavedProvider: "gone", SavedModelID: "missing", AllModels: models, Availability: Availability{HasConfiguredAuth: func(string) bool { return false }, SupportsRoute: func(Model) bool { return true }}})
	if got.Model != nil || got.FallbackMessage != "" {
		t.Fatalf("no fallback = %#v", got)
	}
}

func TestDefaultModelPreferencesMatchPiAndAreDefensivelyCopied(t *testing.T) {
	tests := map[string]string{
		"openai": "gpt-5.5", "openai-codex": "gpt-5.5", "zai": "glm-5.1",
		"minimax": "MiniMax-M2.7", "minimax-cn": "MiniMax-M2.7", "cerebras": "zai-glm-4.7",
		"ant-ling": "Ring-2.6-1T", "vercel-ai-gateway": "zai/glm-5.1",
	}
	for providerID, expected := range tests {
		if got, ok := DefaultModelID(providerID); !ok || got != expected {
			t.Fatalf("DefaultModelID(%q) = %q, %t", providerID, got, ok)
		}
	}
	preferences := DefaultModelPreferences()
	preferences[0].Provider = "mutated"
	if DefaultModelPreferences()[0].Provider != "amazon-bedrock" {
		t.Fatal("default preference storage was exposed")
	}
}
