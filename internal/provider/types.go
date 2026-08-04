package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/llm"
)

var (
	ErrInvalidModel   = errors.New("invalid model reference")
	ErrInvalidRequest = errors.New("invalid provider request")
)

// ThinkingLevel mirrors pi's portable reasoning preference. Providers map it
// to their own request dialect; "off" is a real setting rather than the
// absence of a setting, because it is durable AgentSession state.
type ThinkingLevel string

const (
	ThinkingOff     ThinkingLevel = "off"
	ThinkingMinimal ThinkingLevel = "minimal"
	ThinkingLow     ThinkingLevel = "low"
	ThinkingMedium  ThinkingLevel = "medium"
	ThinkingHigh    ThinkingLevel = "high"
	ThinkingXHigh   ThinkingLevel = "xhigh"
	ThinkingMax     ThinkingLevel = "max"
)

func (l ThinkingLevel) Valid() bool {
	switch l {
	case ThinkingOff, ThinkingMinimal, ThinkingLow, ThinkingMedium, ThinkingHigh, ThinkingXHigh, ThinkingMax:
		return true
	default:
		return false
	}
}

// InputKind records a model's portable accepted input modalities.
type InputKind string

const (
	InputText  InputKind = "text"
	InputImage InputKind = "image"
)

// CostRates is the catalog-level price per million tokens. Request handling
// does not calculate prices, but keeping this with Model prevents a provider
// wire format becoming the product's model contract.
type CostRates struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	// Tiers are request-wide: the highest threshold not exceeding input usage
	// prices the whole request, matching pi's ModelCost.tiers semantics.
	Tiers []CostTier `json:"tiers,omitempty"`
}
type CostTier struct {
	InputTokensAbove uint64  `json:"inputTokensAbove"`
	Input            float64 `json:"input"`
	Output           float64 `json:"output"`
	CacheRead        float64 `json:"cacheRead"`
	CacheWrite       float64 `json:"cacheWrite"`
}

// OpenAIResponsesCompat is the adapter-owned subset of pi's Responses
// compatibility contract. Pointer booleans preserve an explicit false value
// from catalog data. The Responses adapter consumes SupportsDeveloperRole;
// the remaining fields are retained as typed model data for later adapter
// features rather than leaking a vendor wire map into the generic request.
type OpenAIResponsesCompat struct {
	SupportsDeveloperRole           *bool
	SessionAffinityFormat           *string
	SupportsLongCacheRetention      *bool
	SupportsStrictMode              *bool
	SupportsOpenAIGrammarTools      *bool
	SupportsToolSearch              *bool
	SupportsExplicitPromptCacheMode *bool
}

// OpenAICompletionsCompat mirrors pi's explicit compatibility contract for
// OpenAI-compatible Chat Completions endpoints. It remains model data: Agent
// never reads these dialect details.
type OpenAICompletionsCompat struct {
	SupportsStore                               *bool
	SupportsDeveloperRole                       *bool
	SupportsReasoningEffort                     *bool
	SupportsUsageInStreaming                    *bool
	SupportsFinishReason                        *bool
	MaxTokensField                              *string
	RequiresToolResultName                      *bool
	RequiresAssistantAfterToolResult            *bool
	RequiresThinkingAsText                      *bool
	RequiresReasoningContentOnAssistantMessages *bool
	ThinkingFormat                              *string
	SupportsOpenAIGrammarTools                  *bool
	SupportsStrictMode                          *bool
	SendSessionAffinityHeaders                  *bool
	SessionAffinityFormat                       *string
	SupportsLongCacheRetention                  *bool
	CacheControlFormat                          *string
	DeferredToolsMode                           *string
	ZaiToolStream                               *bool
	ChatTemplateKwargs                          map[string]any
	OpenRouterRouting                           map[string]any
	VercelGatewayRouting                        map[string]any
}

type ModelCompat struct {
	OpenAIResponses   *OpenAIResponsesCompat
	OpenAICompletions *OpenAICompletionsCompat
	AnthropicMessages *AnthropicMessagesCompat
	Bedrock           *BedrockCompat
	// Additional carries API-specific compatibility values for current or
	// future dialects that have no provider implementation yet. Values are
	// immutable JSON objects; generic Agent code never inspects them.
	Additional map[string]json.RawMessage
}

type AnthropicMessagesCompat struct {
	SupportsEagerToolInputStreaming *bool
	SupportsLongCacheRetention      *bool
	SendSessionAffinityHeaders      *bool
	SupportsCacheControlOnTools     *bool
	SupportsTemperature             *bool
	ForceAdaptiveThinking           *bool
	AllowEmptySignature             *bool
	SupportsStrictTools             *bool
	SupportsToolReferences          *bool
}
type BedrockCompat struct{ SupportsStrictMode *bool }

// ModelSpec is pi's generic model contract in Go form. Compatibility is an
// adapter-owned, JSON-like value: the loop never reads vendor-specific keys.
type ModelSpec struct {
	Provider, API, ID, Name, BaseURL string
	Reasoning                        bool
	ThinkingLevelMap                 map[ThinkingLevel]*string
	Input                            []InputKind
	Cost                             CostRates
	ContextWindow, MaxTokens         uint64
	Headers                          map[string]string
	Compat                           ModelCompat
}

// ModelRef is the minimum stable identity needed to route one provider call.
// Catalog metadata and adapter configuration remain outside this value.
type ModelRef struct {
	provider string
	api      string
	id       string
	metadata *modelMetadata
}

type modelMetadata struct {
	name             string
	baseURL          string
	reasoning        bool
	thinkingLevelMap map[ThinkingLevel]*string
	input            []InputKind
	cost             CostRates
	contextWindow    uint64
	maxTokens        uint64
	headers          map[string]string
	compat           ModelCompat
}

func NewModelRef(provider, api, id string) (ModelRef, error) {
	model := ModelRef{provider: provider, api: api, id: id}
	if err := model.validate(); err != nil {
		return ModelRef{}, err
	}
	return model, nil
}

// NewModel constructs a generic model value. ModelRef is retained as the
// name for source compatibility with the first Go milestone, but it is now a
// complete model contract rather than only an OpenAI route key.
func NewModel(spec ModelSpec) (ModelRef, error) {
	input := append([]InputKind(nil), spec.Input...)
	if len(input) == 0 {
		input = []InputKind{InputText}
	}
	if err := validateModelSpec(spec, input); err != nil {
		return ModelRef{}, err
	}
	model := ModelRef{provider: spec.Provider, api: spec.API, id: spec.ID, metadata: &modelMetadata{
		name: spec.Name, baseURL: spec.BaseURL, reasoning: spec.Reasoning,
		thinkingLevelMap: cloneThinkingLevelMap(spec.ThinkingLevelMap), input: input, cost: cloneCostRates(spec.Cost),
		contextWindow: spec.ContextWindow, maxTokens: spec.MaxTokens, headers: cloneStrings(spec.Headers), compat: CloneModelCompat(spec.Compat),
	}}
	if err := model.validate(); err != nil {
		return ModelRef{}, err
	}
	return model, nil
}

func validateModelSpec(spec ModelSpec, input []InputKind) error {
	if spec.ContextWindow != 0 && spec.MaxTokens > spec.ContextWindow {
		return fmt.Errorf("%w: max tokens cannot exceed context window", ErrInvalidModel)
	}
	for _, rate := range []float64{spec.Cost.Input, spec.Cost.Output, spec.Cost.CacheRead, spec.Cost.CacheWrite} {
		if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
			return fmt.Errorf("%w: cost rates must be finite and non-negative", ErrInvalidModel)
		}
	}
	lastTier := uint64(0)
	for index, tier := range spec.Cost.Tiers {
		if index != 0 && tier.InputTokensAbove <= lastTier {
			return fmt.Errorf("%w: cost tiers must be strictly increasing", ErrInvalidModel)
		}
		lastTier = tier.InputTokensAbove
		for _, rate := range []float64{tier.Input, tier.Output, tier.CacheRead, tier.CacheWrite} {
			if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
				return fmt.Errorf("%w: tier cost rates must be finite and non-negative", ErrInvalidModel)
			}
		}
	}
	seenInput := map[InputKind]struct{}{}
	for _, kind := range input {
		if kind != InputText && kind != InputImage {
			return fmt.Errorf("%w: unsupported input kind %q", ErrInvalidModel, kind)
		}
		if _, duplicate := seenInput[kind]; duplicate {
			return fmt.Errorf("%w: duplicate input kind %q", ErrInvalidModel, kind)
		}
		seenInput[kind] = struct{}{}
	}
	for level, mapped := range spec.ThinkingLevelMap {
		if !level.Valid() {
			return fmt.Errorf("%w: invalid thinking level map key %q", ErrInvalidModel, level)
		}
		if mapped != nil && (!utf8.ValidString(*mapped) || strings.TrimSpace(*mapped) == "") {
			return fmt.Errorf("%w: invalid thinking level mapping %q", ErrInvalidModel, level)
		}
	}
	for name, value := range spec.Headers {
		if !utf8.ValidString(name) || !utf8.ValidString(value) || strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\r\n") || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("%w: invalid model header", ErrInvalidModel)
		}
	}
	if compat := spec.Compat.OpenAICompletions; compat != nil {
		if compat.MaxTokensField != nil && *compat.MaxTokensField != "max_completion_tokens" && *compat.MaxTokensField != "max_tokens" {
			return fmt.Errorf("%w: invalid Chat Completions max tokens field", ErrInvalidModel)
		}
		if compat.ThinkingFormat != nil {
			switch *compat.ThinkingFormat {
			case "openai", "openrouter", "deepseek", "together", "zai", "qwen", "chat-template", "qwen-chat-template", "string-thinking", "ant-ling":
			default:
				return fmt.Errorf("%w: invalid Chat Completions thinking format", ErrInvalidModel)
			}
		}
		if compat.SessionAffinityFormat != nil && *compat.SessionAffinityFormat != "openai" && *compat.SessionAffinityFormat != "openai-nosession" && *compat.SessionAffinityFormat != "openrouter" {
			return fmt.Errorf("%w: invalid Chat Completions session affinity format", ErrInvalidModel)
		}
	}
	for label, value := range map[string]map[string]any{
		"chatTemplateKwargs": compatMap(spec.Compat.OpenAICompletions, func(value *OpenAICompletionsCompat) map[string]any { return value.ChatTemplateKwargs }),
		"openRouterRouting":  compatMap(spec.Compat.OpenAICompletions, func(value *OpenAICompletionsCompat) map[string]any { return value.OpenRouterRouting }),
		"vercelGatewayRouting": compatMap(spec.Compat.OpenAICompletions, func(value *OpenAICompletionsCompat) map[string]any {
			return value.VercelGatewayRouting
		}),
	} {
		if value == nil {
			continue
		}
		if _, err := json.Marshal(value); err != nil {
			return fmt.Errorf("%w: compat %s is not JSON-like: %v", ErrInvalidModel, label, err)
		}
	}
	for api, raw := range spec.Compat.Additional {
		if !utf8.ValidString(api) || strings.TrimSpace(api) == "" || !json.Valid(raw) {
			return fmt.Errorf("%w: invalid additional compat entry", ErrInvalidModel)
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil || object == nil {
			return fmt.Errorf("%w: additional compat for %q must be a JSON object", ErrInvalidModel, api)
		}
	}
	return nil
}

func compatMap(value *OpenAICompletionsCompat, selectValue func(*OpenAICompletionsCompat) map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	return selectValue(value)
}

func (m ModelRef) validate() error {
	fields := []struct {
		name  string
		value string
	}{
		{name: "provider", value: m.provider},
		{name: "api", value: m.api},
		{name: "id", value: m.id},
	}
	for _, field := range fields {
		if !utf8.ValidString(field.value) || strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%w: %s must be non-empty valid UTF-8", ErrInvalidModel, field.name)
		}
	}
	return nil
}

func (m ModelRef) Provider() string { return m.provider }
func (m ModelRef) API() string      { return m.api }
func (m ModelRef) ID() string       { return m.id }

// Equal follows pi model identity: provider plus model id. API is an adapter
// property and catalog metadata must not change logical model equality.
func (m ModelRef) Equal(other ModelRef) bool { return m.provider == other.provider && m.id == other.id }
func (m ModelRef) Name() string {
	if m.metadata == nil {
		return ""
	}
	return m.metadata.name
}
func (m ModelRef) BaseURL() string {
	if m.metadata == nil {
		return ""
	}
	return m.metadata.baseURL
}
func (m ModelRef) Reasoning() bool { return m.metadata != nil && m.metadata.reasoning }
func (m ModelRef) ContextWindow() uint64 {
	if m.metadata == nil {
		return 0
	}
	return m.metadata.contextWindow
}
func (m ModelRef) MaxTokens() uint64 {
	if m.metadata == nil {
		return 0
	}
	return m.metadata.maxTokens
}
func (m ModelRef) Cost() CostRates {
	if m.metadata == nil {
		return CostRates{}
	}
	return cloneCostRates(m.metadata.cost)
}

// CalculateCost implements pi's request-wide tier selection. The highest
// threshold strictly below total input/cache tokens applies to the whole
// request; one-hour Anthropic cache writes cost 2x input rate.
func (m ModelRef) CalculateCost(usage llm.Usage) llm.Cost {
	rates := m.Cost()
	inputTokens := usage.Input() + usage.CacheRead() + usage.CacheWrite()
	matched := int64(-1)
	for _, tier := range rates.Tiers {
		if inputTokens > tier.InputTokensAbove && int64(tier.InputTokensAbove) > matched {
			rates.Input = tier.Input
			rates.Output = tier.Output
			rates.CacheRead = tier.CacheRead
			rates.CacheWrite = tier.CacheWrite
			matched = int64(tier.InputTokensAbove)
		}
	}
	longWrite := uint64(0)
	if value, ok := usage.CacheWrite1h(); ok {
		longWrite = value
	}
	shortWrite := usage.CacheWrite() - longWrite
	cost := llm.Cost{Input: rates.Input * float64(usage.Input()) / 1_000_000, Output: rates.Output * float64(usage.Output()) / 1_000_000, CacheRead: rates.CacheRead * float64(usage.CacheRead()) / 1_000_000, CacheWrite: (rates.CacheWrite*float64(shortWrite) + rates.Input*2*float64(longWrite)) / 1_000_000}
	cost.Total = cost.Input + cost.Output + cost.CacheRead + cost.CacheWrite
	return cost
}
func (m ModelRef) Input() []InputKind {
	if m.metadata == nil {
		return []InputKind{InputText}
	}
	return append([]InputKind(nil), m.metadata.input...)
}
func (m ModelRef) Headers() map[string]string {
	if m.metadata == nil {
		return nil
	}
	return cloneStrings(m.metadata.headers)
}
func (m ModelRef) Compat() ModelCompat {
	if m.metadata == nil {
		return ModelCompat{}
	}
	return CloneModelCompat(m.metadata.compat)
}
func (m ModelRef) ThinkingLevelMap() map[ThinkingLevel]*string {
	if m.metadata == nil {
		return nil
	}
	return cloneThinkingLevelMap(m.metadata.thinkingLevelMap)
}

var extendedThinkingLevels = []ThinkingLevel{ThinkingOff, ThinkingMinimal, ThinkingLow, ThinkingMedium, ThinkingHigh, ThinkingXHigh, ThinkingMax}

// SupportedThinkingLevels and ClampThinkingLevel mirror pi's models.ts.
// A null mapping disables any level (including off); xhigh/max are opt-in.
func (m ModelRef) SupportedThinkingLevels() []ThinkingLevel {
	if !m.Reasoning() {
		return []ThinkingLevel{ThinkingOff}
	}
	mapping := m.ThinkingLevelMap()
	levels := make([]ThinkingLevel, 0, len(extendedThinkingLevels))
	for _, level := range extendedThinkingLevels {
		mapped, configured := mapping[level]
		if configured && mapped == nil {
			continue
		}
		if (level == ThinkingXHigh || level == ThinkingMax) && !configured {
			continue
		}
		levels = append(levels, level)
	}
	return levels
}

func (m ModelRef) ClampThinkingLevel(level ThinkingLevel) ThinkingLevel {
	if level == "" {
		level = ThinkingOff
	}
	available := m.SupportedThinkingLevels()
	for _, candidate := range available {
		if candidate == level {
			return level
		}
	}
	requested := -1
	for i, candidate := range extendedThinkingLevels {
		if candidate == level {
			requested = i
			break
		}
	}
	if requested < 0 {
		if len(available) != 0 {
			return available[0]
		}
		return ThinkingOff
	}
	for i := requested; i < len(extendedThinkingLevels); i++ {
		for _, candidate := range available {
			if candidate == extendedThinkingLevels[i] {
				return candidate
			}
		}
	}
	for i := requested - 1; i >= 0; i-- {
		for _, candidate := range available {
			if candidate == extendedThinkingLevels[i] {
				return candidate
			}
		}
	}
	if len(available) != 0 {
		return available[0]
	}
	return ThinkingOff
}

func (m ModelRef) ThinkingEffort(level ThinkingLevel) (string, bool) {
	if !m.Reasoning() {
		return "", false
	}
	level = m.ClampThinkingLevel(level)
	if level == ThinkingOff {
		value, configured := m.ThinkingLevelMap()[ThinkingOff]
		if configured && value == nil {
			return "", false
		}
		if configured {
			return *value, true
		}
		return "none", true
	}
	if value, configured := m.ThinkingLevelMap()[level]; configured && value != nil {
		return *value, true
	}
	return string(level), true
}

func cloneStrings(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	copy := make(map[string]string, len(values))
	for key, value := range values {
		copy[key] = value
	}
	return copy
}
func cloneCostRates(value CostRates) CostRates {
	value.Tiers = append([]CostTier(nil), value.Tiers...)
	return value
}
func cloneAny(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	copy := make(map[string]any, len(values))
	for key, value := range values {
		copy[key] = cloneJSONLike(value)
	}
	return copy
}

func cloneJSONLike(value any) any {
	if value == nil {
		return nil
	}
	return cloneJSONReflect(reflect.ValueOf(value)).Interface()
}

func cloneJSONReflect(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		copy := cloneJSONReflect(value.Elem())
		out := reflect.New(value.Type()).Elem()
		out.Set(copy)
		return out
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		out := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			out.SetMapIndex(cloneJSONReflect(iter.Key()), cloneJSONReflect(iter.Value()))
		}
		return out
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		out := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			out.Index(i).Set(cloneJSONReflect(value.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(value.Type()).Elem()
		for i := 0; i < value.Len(); i++ {
			out.Index(i).Set(cloneJSONReflect(value.Index(i)))
		}
		return out
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		out := reflect.New(value.Type().Elem())
		out.Elem().Set(cloneJSONReflect(value.Elem()))
		return out
	default:
		return value
	}
}
func CloneStreamOptions(value StreamOptions) StreamOptions {
	value.Headers = cloneStrings(value.Headers)
	value.HeaderOverrides = cloneHeaderOverrides(value.HeaderOverrides)
	value.ThinkingBudgets = cloneThinkingBudgets(value.ThinkingBudgets)
	value.Metadata = cloneAny(value.Metadata)
	value.Extra = cloneAny(value.Extra)
	value.Env = cloneStrings(value.Env)
	if value.Temperature != nil {
		copy := *value.Temperature
		value.Temperature = &copy
	}
	return value
}

func cloneHeaderOverrides(values map[string]*string) map[string]*string {
	if values == nil {
		return nil
	}
	out := make(map[string]*string, len(values))
	for key, value := range values {
		out[key] = cloneString(value)
	}
	return out
}
func cloneThinkingBudgets(values map[ThinkingLevel]uint64) map[ThinkingLevel]uint64 {
	if values == nil {
		return nil
	}
	out := make(map[ThinkingLevel]uint64, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}
func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
func CloneModelCompat(value ModelCompat) ModelCompat {
	copy := ModelCompat{}
	if value.OpenAIResponses != nil {
		copy.OpenAIResponses = &OpenAIResponsesCompat{
			SupportsDeveloperRole:           cloneBool(value.OpenAIResponses.SupportsDeveloperRole),
			SessionAffinityFormat:           cloneString(value.OpenAIResponses.SessionAffinityFormat),
			SupportsLongCacheRetention:      cloneBool(value.OpenAIResponses.SupportsLongCacheRetention),
			SupportsStrictMode:              cloneBool(value.OpenAIResponses.SupportsStrictMode),
			SupportsOpenAIGrammarTools:      cloneBool(value.OpenAIResponses.SupportsOpenAIGrammarTools),
			SupportsToolSearch:              cloneBool(value.OpenAIResponses.SupportsToolSearch),
			SupportsExplicitPromptCacheMode: cloneBool(value.OpenAIResponses.SupportsExplicitPromptCacheMode),
		}
	}
	if value.OpenAICompletions != nil {
		copy.OpenAICompletions = &OpenAICompletionsCompat{
			SupportsStore: cloneBool(value.OpenAICompletions.SupportsStore), SupportsDeveloperRole: cloneBool(value.OpenAICompletions.SupportsDeveloperRole),
			SupportsReasoningEffort: cloneBool(value.OpenAICompletions.SupportsReasoningEffort), SupportsUsageInStreaming: cloneBool(value.OpenAICompletions.SupportsUsageInStreaming),
			SupportsFinishReason: cloneBool(value.OpenAICompletions.SupportsFinishReason), MaxTokensField: cloneString(value.OpenAICompletions.MaxTokensField),
			RequiresToolResultName: cloneBool(value.OpenAICompletions.RequiresToolResultName), RequiresAssistantAfterToolResult: cloneBool(value.OpenAICompletions.RequiresAssistantAfterToolResult),
			RequiresThinkingAsText: cloneBool(value.OpenAICompletions.RequiresThinkingAsText), RequiresReasoningContentOnAssistantMessages: cloneBool(value.OpenAICompletions.RequiresReasoningContentOnAssistantMessages),
			ThinkingFormat: cloneString(value.OpenAICompletions.ThinkingFormat), SupportsOpenAIGrammarTools: cloneBool(value.OpenAICompletions.SupportsOpenAIGrammarTools),
			SupportsStrictMode: cloneBool(value.OpenAICompletions.SupportsStrictMode), SendSessionAffinityHeaders: cloneBool(value.OpenAICompletions.SendSessionAffinityHeaders),
			SessionAffinityFormat: cloneString(value.OpenAICompletions.SessionAffinityFormat), SupportsLongCacheRetention: cloneBool(value.OpenAICompletions.SupportsLongCacheRetention),
			CacheControlFormat: cloneString(value.OpenAICompletions.CacheControlFormat), DeferredToolsMode: cloneString(value.OpenAICompletions.DeferredToolsMode), ZaiToolStream: cloneBool(value.OpenAICompletions.ZaiToolStream), ChatTemplateKwargs: cloneAny(value.OpenAICompletions.ChatTemplateKwargs), OpenRouterRouting: cloneAny(value.OpenAICompletions.OpenRouterRouting), VercelGatewayRouting: cloneAny(value.OpenAICompletions.VercelGatewayRouting),
		}
	}
	if value.AnthropicMessages != nil {
		v := value.AnthropicMessages
		copy.AnthropicMessages = &AnthropicMessagesCompat{SupportsEagerToolInputStreaming: cloneBool(v.SupportsEagerToolInputStreaming), SupportsLongCacheRetention: cloneBool(v.SupportsLongCacheRetention), SendSessionAffinityHeaders: cloneBool(v.SendSessionAffinityHeaders), SupportsCacheControlOnTools: cloneBool(v.SupportsCacheControlOnTools), SupportsTemperature: cloneBool(v.SupportsTemperature), ForceAdaptiveThinking: cloneBool(v.ForceAdaptiveThinking), AllowEmptySignature: cloneBool(v.AllowEmptySignature), SupportsStrictTools: cloneBool(v.SupportsStrictTools), SupportsToolReferences: cloneBool(v.SupportsToolReferences)}
	}
	if value.Bedrock != nil {
		copy.Bedrock = &BedrockCompat{SupportsStrictMode: cloneBool(value.Bedrock.SupportsStrictMode)}
	}
	if value.Additional != nil {
		copy.Additional = make(map[string]json.RawMessage, len(value.Additional))
		for key, value := range value.Additional {
			copy.Additional[key] = append(json.RawMessage(nil), value...)
		}
	}
	return copy
}
func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
func cloneThinkingLevelMap(values map[ThinkingLevel]*string) map[ThinkingLevel]*string {
	if values == nil {
		return nil
	}
	copy := make(map[ThinkingLevel]*string, len(values))
	for key, value := range values {
		if value != nil {
			item := *value
			copy[key] = &item
		} else {
			copy[key] = nil
		}
	}
	return copy
}

// Request is an immutable snapshot of one provider invocation.
type Request struct {
	model             ModelRef
	systemPrompt      string
	messages          []llm.ConversationMessage
	tools             []ToolDefinition
	parallelToolCalls bool
	toolChoice        *ToolChoice
	thinkingLevel     ThinkingLevel
	metadata          map[string]any
	stream            StreamOptions
	replayTarget      llm.AssistantProvenance
	deferredToolNames []string
	hasDeferredTools  bool
}

// ToolChoice is a portable coordinator policy. Providers map it to their own
// wire dialect; a named tool means that function is required.
type ToolChoice struct{ Mode, Name string }

func (c ToolChoice) validate() error {
	if c.Name != "" {
		if c.Mode != "" && c.Mode != "required" {
			return fmt.Errorf("%w: named tool choice cannot use mode %q", ErrInvalidRequest, c.Mode)
		}
		if _, err := NewToolDefinition(c.Name, "tool choice", false, []byte(`{"type":"object"}`)); err != nil {
			return fmt.Errorf("%w: invalid named tool choice", ErrInvalidRequest)
		}
		return nil
	}
	switch c.Mode {
	case "", "auto", "none", "required":
		return nil
	default:
		return fmt.Errorf("%w: invalid tool choice mode %q", ErrInvalidRequest, c.Mode)
	}
}

// RequestOptions contains provider capabilities that must be chosen by the
// coordinator constructing a request. The zero value remains the safe
// single-call policy for callers without a parallel batch scheduler.
type RequestOptions struct {
	Tools                  []ToolDefinition
	AllowParallelToolCalls bool
	ToolChoice             *ToolChoice
	ThinkingLevel          ThinkingLevel
	// Metadata carries portable per-request annotations. Adapters may consume
	// keys they recognize; AgentLoop never branches on them.
	Metadata map[string]any
	Stream   StreamOptions
}

// StreamOptions is request-scoped adapter configuration. Credentials are not
// properties of an API dialect: a model/provider selection resolves them for
// each request.
type StreamOptions struct {
	Temperature *float64
	APIKey      string
	Headers     map[string]string
	// HeaderOverrides has three states per name: absent leaves inherited
	// headers untouched, non-nil replaces/adds it, and nil explicitly removes
	// it. Headers remains the ordinary convenient add/replace map.
	HeaderOverrides map[string]*string
	MaxTokens       uint64
	SessionID       string
	Transport       Transport
	// Fetch overrides the provider's configured HTTP transport for this one
	// request. It is intentionally an interface, not an untyped Extra value.
	Fetch                     HTTPDoer
	OnPayload                 PayloadHook
	OnHeaders                 HeaderHook
	OnResponse                ResponseHook
	ThinkingBudgets           map[ThinkingLevel]uint64
	CacheRetention            CacheRetention
	TimeoutMS                 uint64
	WebsocketConnectTimeoutMS uint64
	MaxRetries                uint32
	MaxRetryDelayMS           uint64
	Metadata                  map[string]any
	Env                       map[string]string
	// Extra is retained for source compatibility only. New portable options
	// must have a typed field above; adapters must not treat Extra as a wire
	// request escape hatch.
	Extra map[string]any
}

// PayloadHook may make a final JSON-level request adjustment after an adapter
// has produced its dialect payload. It is useful for transports/gateways while
// keeping provider wire structs out of Agent.
type PayloadHook func(model ModelRef, payload []byte) ([]byte, error)
type HeaderHook func(model ModelRef, headers map[string]*string) error

// ResponseInfo is a portable transport observation supplied to OnResponse.
type ResponseInfo struct {
	StatusCode int
	Headers    map[string][]string
}
type ResponseHook func(model ModelRef, response ResponseInfo) error

type Transport string

const (
	TransportSSE             Transport = "sse"
	TransportWebsocket       Transport = "websocket"
	TransportWebsocketCached Transport = "websocket-cached"
	TransportAuto            Transport = "auto"
)

type CacheRetention string

const (
	CacheRetentionNone  CacheRetention = "none"
	CacheRetentionShort CacheRetention = "short"
	CacheRetentionLong  CacheRetention = "long"
)

func NewRequest(
	model ModelRef,
	systemPrompt string,
	messages []llm.ConversationMessage,
) (Request, error) {
	return NewRequestWithOptions(model, systemPrompt, messages, RequestOptions{})
}

// NewRequestWithTools creates one immutable provider request. The legacy
// NewRequest convenience remains deliberately tool-free for existing callers.
func NewRequestWithTools(
	model ModelRef,
	systemPrompt string,
	messages []llm.ConversationMessage,
	tools []ToolDefinition,
) (Request, error) {
	return NewRequestWithOptions(model, systemPrompt, messages, RequestOptions{Tools: tools})
}

// NewRequestWithOptions creates one immutable provider request with explicit
// tool-call concurrency capability. Callers that do not own a multi-call
// scheduler must leave AllowParallelToolCalls false.
func NewRequestWithOptions(
	model ModelRef,
	systemPrompt string,
	messages []llm.ConversationMessage,
	options RequestOptions,
) (Request, error) {
	for label, value := range map[string]any{"metadata": options.Metadata, "stream metadata": options.Stream.Metadata, "stream extra": options.Stream.Extra} {
		if value != nil {
			if _, err := json.Marshal(value); err != nil {
				return Request{}, fmt.Errorf("%w: %s is not JSON-like: %v", ErrInvalidRequest, label, err)
			}
		}
	}
	request := Request{
		model:             model,
		systemPrompt:      systemPrompt,
		messages:          append([]llm.ConversationMessage(nil), messages...),
		tools:             append([]ToolDefinition(nil), options.Tools...),
		parallelToolCalls: options.AllowParallelToolCalls,
		thinkingLevel:     options.ThinkingLevel,
		metadata:          cloneAny(options.Metadata),
		stream:            CloneStreamOptions(options.Stream),
		replayTarget:      llm.AssistantProvenance{Provider: model.Provider(), API: model.API(), Model: model.ID()},
	}
	request.deferredToolNames, request.hasDeferredTools = collectDeferredToolNames(request.messages)
	if options.ToolChoice != nil {
		copy := *options.ToolChoice
		request.toolChoice = &copy
	}
	if err := request.validate(); err != nil {
		return Request{}, err
	}
	return request, nil
}

func (r Request) validate() error {
	if err := r.model.validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	if !utf8.ValidString(r.systemPrompt) {
		return fmt.Errorf("%w: system prompt is not valid UTF-8", ErrInvalidRequest)
	}
	if r.thinkingLevel != "" && !r.thinkingLevel.Valid() {
		return fmt.Errorf("%w: invalid thinking level %q", ErrInvalidRequest, r.thinkingLevel)
	}
	if !utf8.ValidString(r.stream.APIKey) || !utf8.ValidString(r.stream.SessionID) {
		return fmt.Errorf("%w: stream credentials/session are not valid UTF-8", ErrInvalidRequest)
	}
	if r.stream.APIKey != "" && (strings.TrimSpace(r.stream.APIKey) == "" || strings.ContainsFunc(r.stream.APIKey, unicode.IsControl)) {
		return fmt.Errorf("%w: stream API key is invalid", ErrInvalidRequest)
	}
	if r.stream.Temperature != nil && (math.IsNaN(*r.stream.Temperature) || math.IsInf(*r.stream.Temperature, 0)) {
		return fmt.Errorf("%w: invalid temperature", ErrInvalidRequest)
	}
	if r.stream.Transport != "" && r.stream.Transport != TransportSSE && r.stream.Transport != TransportWebsocket && r.stream.Transport != TransportWebsocketCached && r.stream.Transport != TransportAuto {
		return fmt.Errorf("%w: invalid transport", ErrInvalidRequest)
	}
	if r.stream.CacheRetention != "" && r.stream.CacheRetention != CacheRetentionNone && r.stream.CacheRetention != CacheRetentionShort && r.stream.CacheRetention != CacheRetentionLong {
		return fmt.Errorf("%w: invalid cache retention", ErrInvalidRequest)
	}
	for name, value := range r.stream.Headers {
		if !utf8.ValidString(name) || !utf8.ValidString(value) || strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\r\n") || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("%w: invalid stream header", ErrInvalidRequest)
		}
	}
	for name, value := range r.stream.HeaderOverrides {
		if !utf8.ValidString(name) || strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\r\n") {
			return fmt.Errorf("%w: invalid stream header override", ErrInvalidRequest)
		}
		if value != nil && (!utf8.ValidString(*value) || strings.ContainsAny(*value, "\r\n")) {
			return fmt.Errorf("%w: invalid stream header override", ErrInvalidRequest)
		}
	}
	for level := range r.stream.ThinkingBudgets {
		if !level.Valid() {
			return fmt.Errorf("%w: invalid thinking budget level", ErrInvalidRequest)
		}
	}
	if r.stream.Fetch != nil && isTypedNil(r.stream.Fetch) {
		return fmt.Errorf("%w: stream fetch is typed nil", ErrInvalidRequest)
	}
	for index, message := range r.messages {
		if err := llm.ValidateConversationMessage(message); err != nil {
			return fmt.Errorf("%w: message %d: %w", ErrInvalidRequest, index, err)
		}
	}
	if err := validateToolResultCausality(r.messages); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	seenTools := make(map[string]struct{}, len(r.tools))
	for index, definition := range r.tools {
		if err := definition.validate(); err != nil {
			return fmt.Errorf("%w: tool %d: %w", ErrInvalidRequest, index, err)
		}
		if _, duplicate := seenTools[definition.Name()]; duplicate {
			return fmt.Errorf("%w: duplicate tool name %q", ErrInvalidRequest, definition.Name())
		}
		seenTools[definition.Name()] = struct{}{}
	}
	if r.toolChoice != nil {
		if err := r.toolChoice.validate(); err != nil {
			return err
		}
		if r.toolChoice.Name != "" {
			found := false
			for _, tool := range r.tools {
				if tool.Name() == r.toolChoice.Name {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("%w: tool choice %q is not registered", ErrInvalidRequest, r.toolChoice.Name)
			}
		}
	}
	return nil
}

type pendingToolCall struct {
	call         llm.ToolCallBlock
	messageIndex int
}

type toolResultIdentity interface {
	ToolCallID() string
	ToolName() string
}

// validateToolResultCausality validates the conversation itself rather than
// relying on an adapter to notice malformed replay incidentally. One
// successful assistant tool-use turn introduces its calls in source order;
// the immediately following tool results must consume that ordered queue.
func validateToolResultCausality(messages []llm.ConversationMessage) error {
	pending := make([]pendingToolCall, 0)
	seenCalls := make(map[string]int)
	seenResults := make(map[string]int)

	for messageIndex, message := range messages {
		switch message := message.(type) {
		case llm.AssistantToolUseMessage:
			if len(pending) != 0 {
				return fmt.Errorf(
					"message %d: assistant tool call arrived before result for call %q from message %d",
					messageIndex,
					pending[0].call.ID(),
					pending[0].messageIndex,
				)
			}
			for _, block := range message.Blocks() {
				call, ok := block.(llm.ToolCallBlock)
				if !ok {
					continue
				}
				if firstIndex, duplicate := seenCalls[call.ID()]; duplicate {
					return fmt.Errorf(
						"message %d: duplicate tool call id %q first used by message %d",
						messageIndex,
						call.ID(),
						firstIndex,
					)
				}
				seenCalls[call.ID()] = messageIndex
				pending = append(pending, pendingToolCall{call: call, messageIndex: messageIndex})
			}

		case llm.ToolResultMessage:
			var err error
			pending, err = consumePendingToolResult(messageIndex, message, pending, seenResults)
			if err != nil {
				return err
			}

		case llm.ToolResultContentMessage:
			var err error
			pending, err = consumePendingToolResult(messageIndex, message, pending, seenResults)
			if err != nil {
				return err
			}

		default:
			if len(pending) != 0 {
				return fmt.Errorf(
					"message %d: %T arrived before result for call %q from message %d",
					messageIndex,
					message,
					pending[0].call.ID(),
					pending[0].messageIndex,
				)
			}
		}
	}

	if len(pending) != 0 {
		return fmt.Errorf(
			"conversation ended before result for call %q from message %d",
			pending[0].call.ID(),
			pending[0].messageIndex,
		)
	}
	return nil
}

func consumePendingToolResult(
	messageIndex int,
	message toolResultIdentity,
	pending []pendingToolCall,
	seenResults map[string]int,
) ([]pendingToolCall, error) {
	if firstIndex, duplicate := seenResults[message.ToolCallID()]; duplicate {
		return pending, fmt.Errorf(
			"message %d: duplicate tool result for call %q first supplied by message %d",
			messageIndex,
			message.ToolCallID(),
			firstIndex,
		)
	}
	if len(pending) == 0 {
		return pending, fmt.Errorf(
			"message %d: orphan tool result for call %q",
			messageIndex,
			message.ToolCallID(),
		)
	}

	expected := pending[0]
	if message.ToolCallID() != expected.call.ID() {
		for _, later := range pending[1:] {
			if message.ToolCallID() == later.call.ID() {
				return pending, fmt.Errorf(
					"message %d: out-of-order tool result for call %q; next call is %q",
					messageIndex,
					message.ToolCallID(),
					expected.call.ID(),
				)
			}
		}
	}
	if message.ToolCallID() != expected.call.ID() {
		return pending, fmt.Errorf(
			"message %d: %w: result call id %q, want %q",
			messageIndex,
			llm.ErrToolResultMismatch,
			message.ToolCallID(),
			expected.call.ID(),
		)
	}
	if message.ToolName() != expected.call.Name() {
		return pending, fmt.Errorf(
			"message %d: %w: result tool name %q, want %q",
			messageIndex,
			llm.ErrToolResultMismatch,
			message.ToolName(),
			expected.call.Name(),
		)
	}
	seenResults[message.ToolCallID()] = messageIndex
	return pending[1:], nil
}

func (r Request) clone() Request {
	r.messages = append([]llm.ConversationMessage(nil), r.messages...)
	r.tools = append([]ToolDefinition(nil), r.tools...)
	r.metadata = cloneAny(r.metadata)
	r.stream = CloneStreamOptions(r.stream)
	r.deferredToolNames = append([]string(nil), r.deferredToolNames...)
	return r
}

func (r Request) Model() ModelRef { return r.model }

func (r Request) SystemPrompt() string { return r.systemPrompt }

func (r Request) Messages() []llm.ConversationMessage {
	return append([]llm.ConversationMessage(nil), r.messages...)
}

func (r Request) Tools() []ToolDefinition {
	return append([]ToolDefinition(nil), r.tools...)
}

func (r Request) ParallelToolCalls() bool { return r.parallelToolCalls }
func (r Request) ToolChoice() (ToolChoice, bool) {
	if r.toolChoice == nil {
		return ToolChoice{}, false
	}
	return *r.toolChoice, true
}
func (r Request) ThinkingLevel() ThinkingLevel { return r.thinkingLevel }
func (r Request) Metadata() map[string]any     { return cloneAny(r.metadata) }
func (r Request) StreamOptions() StreamOptions {
	return CloneStreamOptions(r.stream)
}

func (r Request) ReplayTarget() llm.AssistantProvenance { return r.replayTarget }

// ThinkingBudget returns the optional cap selected for a thinking level.
func (r Request) ThinkingBudget(level ThinkingLevel) (uint64, bool) {
	value, ok := r.stream.ThinkingBudgets[level]
	return value, ok
}

// DeferredToolNames is the provider-neutral collection of tool names made
// available by earlier tool results. Its presence distinguishes no deferred
// tool protocol from an explicitly empty update.
func (r Request) DeferredToolNames() ([]string, bool) {
	if r.deferredToolNames == nil {
		return nil, r.hasDeferredTools
	}
	return append([]string{}, r.deferredToolNames...), r.hasDeferredTools
}

type deferredToolNamesCarrier interface {
	AddedToolNames() []string
	HasAddedToolNames() bool
}

func collectDeferredToolNames(messages []llm.ConversationMessage) ([]string, bool) {
	seen := map[string]struct{}{}
	var out []string
	has := false
	for _, message := range messages {
		carrier, ok := message.(deferredToolNamesCarrier)
		if !ok || !carrier.HasAddedToolNames() {
			continue
		}
		has = true
		for _, name := range carrier.AddedToolNames() {
			if _, exists := seen[name]; !exists {
				seen[name] = struct{}{}
				out = append(out, name)
			}
		}
	}
	if has && out == nil {
		out = []string{}
	}
	return out, has
}

// splitDeferredTools mirrors pi's transcript-driven placement rule. A tool is
// deferred only when a prior tool result advertises it and it has not already
// been called in this context; adapters decide how to load the resulting
// definitions on their own wire protocol.
func splitDeferredTools(request Request, enabled bool) ([]ToolDefinition, map[string]ToolDefinition) {
	if !enabled {
		return request.Tools(), nil
	}
	used := map[string]struct{}{}
	deferredNames := map[string]struct{}{}
	for _, message := range request.Messages() {
		switch value := message.(type) {
		case llm.AssistantToolUseMessage:
			collectAssistantToolNames(value.Blocks(), used)
		case llm.AssistantRichMessage:
			collectAssistantToolNames(value.Blocks(), used)
		case llm.ToolResultMessage:
			for _, name := range value.AddedToolNames() {
				if _, called := used[name]; !called {
					deferredNames[name] = struct{}{}
				}
			}
		case llm.ToolResultContentMessage:
			for _, name := range value.AddedToolNames() {
				if _, called := used[name]; !called {
					deferredNames[name] = struct{}{}
				}
			}
		}
	}
	immediate := make([]ToolDefinition, 0, len(request.Tools()))
	deferred := map[string]ToolDefinition{}
	for _, tool := range request.Tools() {
		if _, ok := deferredNames[tool.Name()]; ok {
			deferred[tool.Name()] = tool
		} else {
			immediate = append(immediate, tool)
		}
	}
	return immediate, deferred
}

func collectAssistantToolNames(blocks []llm.AssistantBlock, used map[string]struct{}) {
	for _, block := range blocks {
		if call, ok := block.(llm.ToolCallBlock); ok {
			used[call.Name()] = struct{}{}
		}
	}
}

// EventStream is a single-consumer pull stream. All expected provider failures
// are represented by llm.ErrorEvent; io.EOF follows the unique terminal event.
type EventStream interface {
	Next() (llm.StreamEvent, error)
	Close() error
}

// Provider is the narrow stream port consumed by the agent runtime.
type Provider interface {
	Stream(context.Context, Request) EventStream
}

// RouteValidator is implemented by model-driven provider runtimes. Sessions
// use it at SetModel time so a route that has no registered adapter fails at
// the control boundary instead of being sent to whichever adapter happened to
// construct the session.
type RouteValidator interface {
	SupportsModel(ModelRef) bool
}

// Router is the small runtime equivalent of pi's Models.stream(model,
// context, options): model API selects an adapter for every request. It owns
// no model catalog or credentials; adapters own those resource lifetimes.
type Router struct {
	adapters map[string]Provider
	models   map[string]map[string]Provider
}

// ProviderRegistration binds a pi provider id to adapters by API dialect.
// It is the product routing form; NewRouter remains API-only compatibility.
type ProviderRegistration struct {
	ID       string
	Adapters map[string]Provider
}

func NewModelRouter(registrations []ProviderRegistration) (*Router, error) {
	if len(registrations) == 0 {
		return nil, fmt.Errorf("%w: at least one provider registration is required", ErrInvalidRequest)
	}
	r := &Router{models: make(map[string]map[string]Provider)}
	for _, registration := range registrations {
		if !utf8.ValidString(registration.ID) || strings.TrimSpace(registration.ID) == "" || len(registration.Adapters) == 0 {
			return nil, fmt.Errorf("%w: invalid provider registration", ErrInvalidRequest)
		}
		if _, exists := r.models[registration.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate provider registration %q", ErrInvalidRequest, registration.ID)
		}
		adapters := make(map[string]Provider, len(registration.Adapters))
		for api, adapter := range registration.Adapters {
			if !utf8.ValidString(api) || strings.TrimSpace(api) == "" || isNilProvider(adapter) {
				return nil, fmt.Errorf("%w: invalid provider adapter route", ErrInvalidRequest)
			}
			adapters[api] = adapter
		}
		r.models[registration.ID] = adapters
	}
	return r, nil
}

func NewRouter(adapters map[string]Provider) (*Router, error) {
	if len(adapters) == 0 {
		return nil, fmt.Errorf("%w: at least one provider adapter is required", ErrInvalidRequest)
	}
	copy := make(map[string]Provider, len(adapters))
	for api, adapter := range adapters {
		if !utf8.ValidString(api) || strings.TrimSpace(api) == "" || isNilProvider(adapter) {
			return nil, fmt.Errorf("%w: invalid provider adapter route", ErrInvalidRequest)
		}
		if _, duplicate := copy[api]; duplicate {
			return nil, fmt.Errorf("%w: duplicate provider adapter API %q", ErrInvalidRequest, api)
		}
		copy[api] = adapter
	}
	return &Router{adapters: copy}, nil
}

func (r *Router) SupportsModel(model ModelRef) bool {
	if r == nil {
		return false
	}
	if r.models != nil {
		adapter, ok := r.models[model.Provider()][model.API()]
		if !ok {
			return false
		}
		if validator, ok := adapter.(RouteValidator); ok {
			return validator.SupportsModel(model)
		}
		return true
	}
	adapter, ok := r.adapters[model.API()]
	if !ok {
		return false
	}
	if validator, ok := adapter.(RouteValidator); ok {
		return validator.SupportsModel(model)
	}
	return true
}

func (r *Router) Stream(ctx context.Context, request Request) EventStream {
	if r == nil {
		return &routeFailureStream{err: fmt.Errorf("%w: nil provider router", ErrInvalidRequest)}
	}
	adapter, ok := r.adapters[request.Model().API()]
	if r.models != nil {
		adapter, ok = r.models[request.Model().Provider()][request.Model().API()]
	}
	if !ok {
		return &routeFailureStream{err: fmt.Errorf("%w: no adapter registered for model API %q", ErrInvalidRequest, request.Model().API())}
	}
	if validator, ok := adapter.(RouteValidator); ok && !validator.SupportsModel(request.Model()) {
		return &routeFailureStream{err: fmt.Errorf("%w: adapter does not support model %s/%s/%s", ErrInvalidRequest, request.Model().Provider(), request.Model().API(), request.Model().ID())}
	}
	return adapter.Stream(ctx, request)
}

func isNilProvider(value Provider) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	return reflected.Kind() == reflect.Pointer && reflected.IsNil()
}

type routeFailureStream struct{ err error }

func (s *routeFailureStream) Next() (llm.StreamEvent, error) {
	if s == nil || s.err == nil {
		return nil, io.EOF
	}
	err := s.err
	s.err = nil
	return nil, err
}
func (*routeFailureStream) Close() error { return nil }

func closedStreamError(err error) error {
	if errors.Is(err, io.EOF) {
		return err
	}
	return fmt.Errorf("provider stream failed: %w", err)
}
