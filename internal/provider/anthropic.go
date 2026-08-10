package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/llm"
)

const (
	AnthropicProviderID          = "anthropic"
	AnthropicMessagesAPI         = "anthropic-messages"
	defaultAnthropicBaseURL      = "https://api.anthropic.com"
	defaultAnthropicEventBytes   = 1 << 20
	defaultAnthropicErrorBytes   = 64 << 10
	interleavedThinkingBeta      = "interleaved-thinking-2025-05-14"
	fineGrainedToolStreamingBeta = "fine-grained-tool-streaming-2025-05-14"
	claudeCodeVersion            = "2.1.75"
)

var claudeCodeToolNames = map[string]string{
	"read": "Read", "write": "Write", "edit": "Edit", "bash": "Bash", "grep": "Grep", "glob": "Glob",
	"askuserquestion": "AskUserQuestion", "enterplanmode": "EnterPlanMode", "exitplanmode": "ExitPlanMode",
	"killshell": "KillShell", "notebookedit": "NotebookEdit", "skill": "Skill", "task": "Task",
	"taskoutput": "TaskOutput", "todowrite": "TodoWrite", "webfetch": "WebFetch", "websearch": "WebSearch",
}

var (
	ErrInvalidAnthropicConfig = errors.New("invalid Anthropic Messages configuration")
	ErrAnthropicRequest       = errors.New("invalid Anthropic Messages request")
	ErrAnthropicStream        = errors.New("invalid Anthropic Messages stream")
	ErrAnthropicAborted       = errors.New("Anthropic Messages request aborted")
	errAnthropicStreamClosed  = errors.New("Anthropic Messages stream closed")
	errAnthropicStreamDone    = errors.New("Anthropic Messages stream finished")
)

type AnthropicConfig struct {
	BaseURL           string
	APIKey            string
	Headers           map[string]string
	Client            HTTPDoer
	Clock             Clock
	MaxEventBytes     int
	MaxErrorBodyBytes int
}

type AnthropicProvider struct {
	endpoint          string
	apiKey            string
	headers           map[string]string
	client            HTTPDoer
	clock             Clock
	maxEventBytes     int
	maxErrorBodyBytes int
	configurationFail *anthropicFailureSpec
}

func NewAnthropicProvider(config AnthropicConfig) (*AnthropicProvider, error) {
	baseURL := config.BaseURL
	if baseURL == "" {
		baseURL = defaultAnthropicBaseURL
	}
	endpoint, err := anthropicEndpoint(baseURL)
	if err != nil {
		return nil, err
	}
	if config.MaxEventBytes < 0 || config.MaxErrorBodyBytes < 0 {
		return nil, fmt.Errorf("%w: byte limits cannot be negative", ErrInvalidAnthropicConfig)
	}
	client := config.Client
	if client == nil {
		client = http.DefaultClient
	} else if isTypedNil(client) {
		return nil, fmt.Errorf("%w: HTTP client is a typed nil", ErrInvalidAnthropicConfig)
	}
	clock := config.Clock
	if clock == nil {
		clock = time.Now
	}
	provider := &AnthropicProvider{
		endpoint: endpoint, apiKey: config.APIKey, headers: cloneStrings(config.Headers), client: client,
		clock: synchronizedClock(clock), maxEventBytes: config.MaxEventBytes, maxErrorBodyBytes: config.MaxErrorBodyBytes,
	}
	if provider.maxEventBytes == 0 {
		provider.maxEventBytes = defaultAnthropicEventBytes
	}
	if provider.maxErrorBodyBytes == 0 {
		provider.maxErrorBodyBytes = defaultAnthropicErrorBytes
	}
	if !validBearerAPIKey(provider.apiKey) && !anthropicHeadersHaveAuth(provider.headers) {
		provider.configurationFail = &anthropicFailureSpec{kind: FailureConfiguration, cause: fmt.Errorf("%w: API key must be non-empty valid UTF-8", ErrInvalidAnthropicConfig), message: "Anthropic API key is not configured"}
	}
	return provider, nil
}

func anthropicEndpoint(base string) (string, error) {
	if !utf8.ValidString(base) || strings.TrimSpace(base) == "" {
		return "", fmt.Errorf("%w: base URL must be non-empty valid UTF-8", ErrInvalidAnthropicConfig)
	}
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", fmt.Errorf("%w: base URL must be an absolute HTTP(S) URL without user info, query, or fragment", ErrInvalidAnthropicConfig)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/v1/messages"
	parsed.RawPath = ""
	return parsed.String(), nil
}

func (*AnthropicProvider) SupportsModel(model Model) bool { return model.API() == AnthropicMessagesAPI }

func (p *AnthropicProvider) Stream(ctx context.Context, request Request) EventStream {
	clock := Clock(time.Now)
	if p != nil && p.clock != nil {
		clock = p.clock
	}
	if ctx == nil {
		return newAnthropicFailureStream(context.Background(), clock, request.Model(), FailureInvalidRequest, fmt.Errorf("%w: nil context", ErrInvalidRequest), "Anthropic Messages request requires a context")
	}
	if p == nil {
		return newAnthropicFailureStream(ctx, clock, request.Model(), FailureConfiguration, fmt.Errorf("%w: nil provider", ErrInvalidAnthropicConfig), "Anthropic Messages provider is not configured")
	}
	if err := request.validate(); err != nil {
		return newAnthropicFailureStream(ctx, clock, request.Model(), FailureInvalidRequest, err, "")
	}
	if request.Model().API() != AnthropicMessagesAPI {
		return newAnthropicFailureStream(ctx, clock, request.Model(), FailureConfiguration, fmt.Errorf("%w: model routes to provider %q API %q", ErrAnthropicRequest, request.Model().Provider(), request.Model().API()), "")
	}
	effectiveAPIKey := requestAPIKey(request, p.apiKey)
	isOAuth := strings.Contains(effectiveAPIKey, "sk-ant-oat")
	payload, err := encodeAnthropicRequest(request, isOAuth)
	if err != nil {
		return newAnthropicFailureStream(ctx, clock, request.Model(), FailureInvalidRequest, err, "")
	}
	options := request.StreamOptions()
	if payload, err = applyPayloadHook(options.OnPayload, request.Model(), payload); err != nil {
		return newAnthropicFailureStream(ctx, clock, request.Model(), FailureInvalidRequest, err, "")
	}
	endpoint := p.endpoint
	if baseURL := request.Model().BaseURL(); baseURL != "" {
		endpoint, err = anthropicEndpoint(baseURL)
		if err != nil {
			return newAnthropicFailureStream(ctx, clock, request.Model(), FailureInvalidRequest, err, "")
		}
	}
	streamContext, cancel, timeoutCancel := streamContextWithTimeout(ctx, options.TimeoutMS)
	cacheRetention := resolveOpenAICacheRetention(options)
	compat := anthropicCompat(request.Model())
	beta := make([]string, 0, 4)
	defaults := map[string]string{
		"accept": "application/json",
		"anthropic-dangerous-direct-browser-access": "true",
	}
	if isOAuth {
		beta = append(beta, "claude-code-20250219", "oauth-2025-04-20")
		defaults["user-agent"] = "claude-cli/" + claudeCodeVersion
		defaults["x-app"] = "cli"
	} else if options.SessionID != "" && cacheRetention != CacheRetentionNone && compat.sendSessionAffinityHeaders {
		defaults["x-session-affinity"] = options.SessionID
	}
	interleaved := true
	if options.InterleavedThinking != nil {
		interleaved = *options.InterleavedThinking
	}
	// pi asks for the interleaved-thinking beta at client construction time,
	// independently of whether this individual call enables thinking. This is
	// important for replaying signed thinking blocks on a nominally non-thinking
	// turn.
	if interleaved && !compat.forceAdaptiveThinking {
		beta = append(beta, interleavedThinkingBeta)
	}
	if len(request.Tools()) != 0 && !compat.supportsEagerToolInputStreaming {
		beta = append(beta, fineGrainedToolStreamingBeta)
	}
	if len(beta) != 0 {
		defaults["anthropic-beta"] = strings.Join(beta, ",")
	}
	headers := mergeResponseHeaders(defaults, request.Model().Headers(), p.headers, options.Headers)
	client := p.client
	if options.Fetch != nil {
		client = options.Fetch
	}
	toolNames := make(map[string]string, len(request.Tools()))
	for _, tool := range request.Tools() {
		toolNames[strings.ToLower(tool.Name())] = tool.Name()
	}
	return &anthropicStream{
		ctx: streamContext, cancel: cancel, timeoutCancel: timeoutCancel, endpoint: endpoint,
		apiKey: effectiveAPIKey, client: client, clock: clock, timestamp: clock(), payload: payload,
		model: request.Model(), headers: headers, maxEventBytes: p.maxEventBytes, maxErrorBodyBytes: p.maxErrorBodyBytes,
		onResponse: options.OnResponse, onHeaders: options.OnHeaders, headerOverrides: cloneHeaderOverrides(options.HeaderOverrides),
		configurationFail: p.configurationFail, maxRetries: valueOrZero32(options.MaxRetries), maxRetryDelayMS: cloneUint64(options.MaxRetryDelayMS),
		slots: make(map[int]*anthropicContentSlot), toolNames: toolNames,
	}
}

func anthropicHeadersHaveAuth(headers map[string]string) bool {
	for name, value := range headers {
		if (strings.EqualFold(name, "authorization") || strings.EqualFold(name, "x-api-key") || strings.EqualFold(name, "cf-aig-authorization")) && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

type resolvedAnthropicCompat struct {
	supportsEagerToolInputStreaming bool
	supportsLongCacheRetention      bool
	sendSessionAffinityHeaders      bool
	supportsCacheControlOnTools     bool
	supportsTemperature             bool
	forceAdaptiveThinking           bool
	allowEmptySignature             bool
	supportsStrictTools             bool
	supportsToolReferences          bool
}

func anthropicCompat(model Model) resolvedAnthropicCompat {
	result := resolvedAnthropicCompat{
		supportsEagerToolInputStreaming: true, supportsLongCacheRetention: true,
		supportsCacheControlOnTools: true, supportsTemperature: true,
		supportsToolReferences: defaultAnthropicSupportsToolReferences(model),
	}
	compat := model.Compat().AnthropicMessages
	if compat == nil {
		return result
	}
	setDefaultBool := func(target *bool, value *bool) {
		if value != nil {
			*target = *value
		}
	}
	setDefaultBool(&result.supportsEagerToolInputStreaming, compat.SupportsEagerToolInputStreaming)
	setDefaultBool(&result.supportsLongCacheRetention, compat.SupportsLongCacheRetention)
	setDefaultBool(&result.sendSessionAffinityHeaders, compat.SendSessionAffinityHeaders)
	setDefaultBool(&result.supportsCacheControlOnTools, compat.SupportsCacheControlOnTools)
	setDefaultBool(&result.supportsTemperature, compat.SupportsTemperature)
	setDefaultBool(&result.forceAdaptiveThinking, compat.ForceAdaptiveThinking)
	setDefaultBool(&result.allowEmptySignature, compat.AllowEmptySignature)
	setDefaultBool(&result.supportsStrictTools, compat.SupportsStrictTools)
	setDefaultBool(&result.supportsToolReferences, compat.SupportsToolReferences)
	return result
}

func defaultAnthropicSupportsToolReferences(model Model) bool {
	if model.Provider() != AnthropicProviderID || strings.Contains(strings.ToLower(model.ID()), "haiku") {
		return false
	}
	parts := strings.Split(model.ID(), "-")
	if len(parts) < 3 || parts[0] != "claude" || (parts[1] != "opus" && parts[1] != "sonnet" && parts[1] != "fable") {
		return false
	}
	major, err := strconv.ParseUint(parts[2], 10, 32)
	if err != nil {
		return false
	}
	var minor uint64
	if len(parts) > 3 && len(parts[3]) < 8 {
		// A non-numeric fourth segment is a suffix such as "latest", not a
		// minor version. This mirrors the optional numeric capture in pi's
		// anchored model-id pattern.
		minor, _ = strconv.ParseUint(parts[3], 10, 32)
	}
	return major > 4 || major == 4 && minor >= 5
}

type anthropicRequestPayload struct {
	Model        string         `json:"model"`
	Messages     []any          `json:"messages"`
	MaxTokens    uint64         `json:"max_tokens"`
	Stream       bool           `json:"stream"`
	System       []any          `json:"system,omitempty"`
	Temperature  *float64       `json:"temperature,omitempty"`
	Tools        []any          `json:"tools,omitempty"`
	Thinking     any            `json:"thinking,omitempty"`
	OutputConfig map[string]any `json:"output_config,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	ToolChoice   any            `json:"tool_choice,omitempty"`
}

type anthropicCacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

func encodeAnthropicRequest(request Request, isOAuth bool) ([]byte, error) {
	messages, err := transformConversationMessages(request.Messages())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAnthropicRequest, err)
	}
	options := request.StreamOptions()
	compat := anthropicCompat(request.Model())
	cacheRetention := resolveOpenAICacheRetention(options)
	var cacheControl *anthropicCacheControl
	if cacheRetention != CacheRetentionNone {
		cacheControl = &anthropicCacheControl{Type: "ephemeral"}
		if cacheRetention == CacheRetentionLong && compat.supportsLongCacheRetention {
			cacheControl.TTL = "1h"
		}
	}
	wireMessages, err := encodeAnthropicMessages(messages, request, cacheControl, compat, isOAuth)
	if err != nil {
		return nil, err
	}
	maxTokens, thinkingBudget := resolveAnthropicTokenLimits(request, compat)
	payload := anthropicRequestPayload{Model: request.Model().ID(), Messages: wireMessages, MaxTokens: maxTokens, Stream: true}
	if isOAuth {
		block := map[string]any{"type": "text", "text": "You are Claude Code, Anthropic's official CLI for Claude."}
		if cacheControl != nil {
			block["cache_control"] = cacheControl
		}
		payload.System = append(payload.System, block)
	}
	if request.SystemPrompt() != "" {
		block := map[string]any{"type": "text", "text": request.SystemPrompt()}
		if cacheControl != nil {
			block["cache_control"] = cacheControl
		}
		payload.System = append(payload.System, block)
	}
	if options.Temperature != nil && !anthropicThinkingEnabled(request) && compat.supportsTemperature {
		payload.Temperature = options.Temperature
	}
	tools, err := encodeAnthropicTools(request, cacheControl, compat, isOAuth)
	if err != nil {
		return nil, err
	}
	payload.Tools = tools
	applyAnthropicThinking(&payload, request, compat, thinkingBudget)
	if userID, ok := options.Metadata["user_id"].(string); ok && strings.TrimSpace(userID) != "" {
		payload.Metadata = map[string]any{"user_id": userID}
	}
	if choice, ok := request.ToolChoice(); ok {
		if choice.Name != "" {
			payload.ToolChoice = map[string]string{"type": "tool", "name": normalizeAnthropicToolName(choice.Name, isOAuth)}
		} else if choice.Mode != "" {
			mode := choice.Mode
			if mode == "required" {
				mode = "any"
			}
			payload.ToolChoice = map[string]string{"type": mode}
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: encode JSON: %v", ErrAnthropicRequest, err)
	}
	return encoded, nil
}

func anthropicThinkingEnabled(request Request) bool {
	if enabled := request.StreamOptions().ThinkingEnabled; enabled != nil {
		return *enabled
	}
	level := request.ThinkingLevel()
	return request.Model().Reasoning() && level != "" && level != ThinkingOff
}

func resolveAnthropicTokenLimits(request Request, compat resolvedAnthropicCompat) (uint64, uint64) {
	options := request.StreamOptions()
	maxTokens := request.Model().MaxTokens()
	if options.MaxTokens != nil {
		maxTokens = *options.MaxTokens
	}
	if !anthropicThinkingEnabled(request) || compat.forceAdaptiveThinking {
		return maxTokens, 0
	}

	// An explicitly supplied Anthropic thinking switch represents the raw
	// adapter contract. As in pi's stream(), max_tokens is already the complete
	// cap and is not expanded to make room for the thinking budget.
	if options.ThinkingEnabled != nil {
		budget := uint64(1024)
		if options.ThinkingBudgetTokens != nil && *options.ThinkingBudgetTokens != 0 {
			budget = *options.ThinkingBudgetTokens
		}
		return maxTokens, budget
	}

	// Agent calls use pi's streamSimple() contract: maxTokens is the desired
	// visible-output cap, so budget-based thinking is added within the model cap.
	level := request.ThinkingLevel()
	if level == ThinkingXHigh || level == ThinkingMax {
		level = ThinkingHigh
	}
	defaults := map[ThinkingLevel]uint64{ThinkingMinimal: 1024, ThinkingLow: 2048, ThinkingMedium: 8192, ThinkingHigh: 16384}
	budget := defaults[level]
	if configured, ok := request.ThinkingBudget(level); ok {
		budget = configured
	}
	if options.MaxTokens != nil {
		if *options.MaxTokens > request.Model().MaxTokens()-minUint64(request.Model().MaxTokens(), budget) {
			maxTokens = request.Model().MaxTokens()
		} else {
			maxTokens = *options.MaxTokens + budget
		}
	}
	if maxTokens <= budget {
		if maxTokens > 1024 {
			budget = maxTokens - 1024
		} else {
			budget = 0
		}
	}
	return maxTokens, budget
}

func minUint64(left, right uint64) uint64 {
	if left < right {
		return left
	}
	return right
}

func applyAnthropicThinking(payload *anthropicRequestPayload, request Request, compat resolvedAnthropicCompat, budget uint64) {
	if payload == nil || !request.Model().Reasoning() {
		return
	}
	options := request.StreamOptions()
	enabled := anthropicThinkingEnabled(request)
	if !enabled {
		off, configured := request.Model().ThinkingLevelMap()[ThinkingOff]
		// streamSimple always lowers portable reasoning-off to the raw
		// thinkingEnabled:false contract. A null off mapping is the sole catalog
		// opt-out (currently used by models that reject thinking.disabled).
		if !configured || off != nil {
			payload.Thinking = map[string]any{"type": "disabled"}
		}
		return
	}
	display := options.ThinkingDisplay
	if display == "" {
		display = "summarized"
	}
	if compat.forceAdaptiveThinking {
		payload.Thinking = map[string]any{"type": "adaptive", "display": display}
		effort := options.AnthropicEffort
		ok := effort != ""
		if !ok {
			effort, ok = request.Model().ThinkingEffort(request.ThinkingLevel())
		}
		if ok && effort != "" {
			payload.OutputConfig = map[string]any{"effort": effort}
		}
		return
	}
	payload.Thinking = map[string]any{"type": "enabled", "budget_tokens": budget, "display": display}
}

func encodeAnthropicMessages(messages []llm.ConversationMessage, request Request, cacheControl *anthropicCacheControl, compat resolvedAnthropicCompat, isOAuth bool) ([]any, error) {
	result := make([]any, 0, len(messages))
	toolIDs := map[string]string{}
	deferredNames := map[string]struct{}{}
	if compat.supportsToolReferences {
		_, deferred := splitDeferredTools(request, true)
		for name := range deferred {
			deferredNames[normalizeAnthropicToolName(name, isOAuth)] = struct{}{}
		}
	}
	loadedNames := map[string]struct{}{}
	for index := 0; index < len(messages); index++ {
		message := messages[index]
		switch value := message.(type) {
		case llm.UserTextMessage:
			var text strings.Builder
			for _, block := range value.Content() {
				text.WriteString(block.Text())
			}
			if strings.TrimSpace(text.String()) != "" {
				result = append(result, map[string]any{"role": "user", "content": text.String()})
			}
		case llm.UserContentMessage:
			blocks, err := anthropicUserContent(value.Content(), request.Model())
			if err != nil {
				return nil, fmt.Errorf("%w: message %d: %v", ErrAnthropicRequest, index, err)
			}
			if len(blocks) != 0 {
				result = append(result, map[string]any{"role": "user", "content": blocks})
			}
		case llm.AssistantTextMessage:
			blocks, err := anthropicAssistantBlocks(value.Blocks(), value.AssistantProvenance(), request.ReplayTarget(), request.Model(), toolIDs, compat, isOAuth)
			if err != nil {
				return nil, fmt.Errorf("%w: message %d: %v", ErrAnthropicRequest, index, err)
			}
			if len(blocks) != 0 {
				result = append(result, map[string]any{"role": "assistant", "content": blocks})
			}
		case llm.AssistantRichMessage:
			blocks, err := anthropicAssistantBlocks(value.Blocks(), value.AssistantProvenance(), request.ReplayTarget(), request.Model(), toolIDs, compat, isOAuth)
			if err != nil {
				return nil, fmt.Errorf("%w: message %d: %v", ErrAnthropicRequest, index, err)
			}
			if len(blocks) != 0 {
				result = append(result, map[string]any{"role": "assistant", "content": blocks})
			}
		case llm.AssistantToolUseMessage:
			blocks, err := anthropicAssistantBlocks(value.Blocks(), value.AssistantProvenance(), request.ReplayTarget(), request.Model(), toolIDs, compat, isOAuth)
			if err != nil {
				return nil, fmt.Errorf("%w: message %d: %v", ErrAnthropicRequest, index, err)
			}
			if len(blocks) != 0 {
				result = append(result, map[string]any{"role": "assistant", "content": blocks})
			}
		case llm.ToolResultMessage, llm.ToolResultContentMessage:
			toolResults := make([]any, 0)
			sibling := make([]any, 0)
			for index < len(messages) {
				current := messages[index]
				var callID, toolName string
				var content []llm.ToolResultContentBlock
				var isError bool
				var added []string
				switch toolResult := current.(type) {
				case llm.ToolResultMessage:
					callID, toolName, isError, added = toolResult.ToolCallID(), toolResult.ToolName(), toolResult.IsError(), toolResult.AddedToolNames()
					for _, block := range toolResult.Content() {
						content = append(content, block)
					}
				case llm.ToolResultContentMessage:
					callID, toolName, content, isError, added = toolResult.ToolCallID(), toolResult.ToolName(), toolResult.Content(), toolResult.IsError(), toolResult.AddedToolNames()
				default:
					goto toolResultsDone
				}
				if normalized, ok := toolIDs[callID]; ok {
					callID = normalized
				}
				wireContent, err := anthropicToolResultContent(content, request.Model())
				if err != nil {
					return nil, fmt.Errorf("%w: message %d: %v", ErrAnthropicRequest, index, err)
				}
				references := make([]any, 0)
				for _, name := range added {
					normalizedName := normalizeAnthropicToolName(name, isOAuth)
					if _, deferred := deferredNames[normalizedName]; !deferred {
						continue
					}
					if _, loaded := loadedNames[normalizedName]; loaded {
						continue
					}
					loadedNames[normalizedName] = struct{}{}
					references = append(references, map[string]any{"type": "tool_reference", "tool_name": normalizedName})
				}
				visibleContent := wireContent
				if len(references) != 0 {
					visibleContent = references
					if text, ok := wireContent.(string); ok {
						sibling = append(sibling, map[string]any{"type": "text", "text": text})
					} else if blocks, ok := wireContent.([]any); ok {
						sibling = append(sibling, blocks...)
					}
				}
				toolResults = append(toolResults, map[string]any{"type": "tool_result", "tool_use_id": callID, "content": visibleContent, "is_error": isError, "_tool_name": toolName})
				index++
			}
		toolResultsDone:
			index--
			for _, block := range toolResults {
				delete(block.(map[string]any), "_tool_name")
			}
			result = append(result, map[string]any{"role": "user", "content": append(toolResults, sibling...)})
		case llm.AssistantFailureMessage:
			// transformConversationMessages already removes these.
		}
	}
	if cacheControl != nil && len(result) != 0 {
		if last, ok := result[len(result)-1].(map[string]any); ok && last["role"] == "user" {
			if content, ok := last["content"].(string); ok && content != "" {
				last["content"] = []any{map[string]any{"type": "text", "text": content, "cache_control": cacheControl}}
			} else if blocks, ok := last["content"].([]any); ok && len(blocks) != 0 {
				if block, ok := blocks[len(blocks)-1].(map[string]any); ok {
					switch block["type"] {
					case "text", "image", "tool_result":
						block["cache_control"] = cacheControl
					}
				}
			}
		}
	}
	return result, nil
}

func anthropicAssistantBlocks(blocks []llm.AssistantBlock, source, target llm.AssistantProvenance, model Model, toolIDs map[string]string, compat resolvedAnthropicCompat, isOAuth bool) ([]any, error) {
	result := make([]any, 0, len(blocks))
	sameModel := source.Matches(target.Provider, target.API, target.Model)
	for _, block := range blocks {
		switch value := block.(type) {
		case llm.TextBlock:
			if strings.TrimSpace(value.Text()) != "" {
				result = append(result, map[string]any{"type": "text", "text": value.Text()})
			}
		case llm.ThinkingBlock:
			signature, hasSignature := value.ThinkingSignature()
			if value.Redacted() {
				if sameModel && hasSignature {
					result = append(result, map[string]any{"type": "redacted_thinking", "data": signature})
				}
				continue
			}
			if sameModel && (hasSignature || compat.allowEmptySignature) {
				result = append(result, map[string]any{"type": "thinking", "thinking": value.Thinking(), "signature": signature})
			} else if strings.TrimSpace(value.Thinking()) != "" {
				result = append(result, map[string]any{"type": "text", "text": value.Thinking()})
			}
		case llm.ToolCallBlock:
			id := value.ID()
			if !sameModel {
				id = normalizeAnthropicToolCallID(id)
				toolIDs[value.ID()] = id
			}
			var input map[string]any
			if err := json.Unmarshal(value.ArgumentsJSON(), &input); err != nil {
				return nil, err
			}
			result = append(result, map[string]any{"type": "tool_use", "id": id, "name": normalizeAnthropicToolName(value.Name(), isOAuth), "input": input})
		default:
			return nil, fmt.Errorf("unsupported assistant block %T", block)
		}
	}
	return result, nil
}

func normalizeAnthropicToolName(name string, isOAuth bool) string {
	if !isOAuth {
		return name
	}
	if canonical, ok := claudeCodeToolNames[strings.ToLower(name)]; ok {
		return canonical
	}
	return name
}

func normalizeAnthropicToolCallID(value string) string {
	var normalized strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-' {
			normalized.WriteRune(character)
		} else {
			normalized.WriteByte('_')
		}
		if normalized.Len() == 64 {
			break
		}
	}
	if normalized.Len() == 0 {
		return "tool_pi"
	}
	return normalized.String()
}

func anthropicUserContent(blocks []llm.UserContentBlock, model Model) ([]any, error) {
	result := make([]any, 0, len(blocks))
	supportsImages := false
	for _, kind := range model.Input() {
		if kind == InputImage {
			supportsImages = true
		}
	}
	previousPlaceholder := false
	for _, block := range blocks {
		switch value := block.(type) {
		case llm.TextBlock:
			if strings.TrimSpace(value.Text()) != "" {
				result = append(result, map[string]any{"type": "text", "text": value.Text()})
			}
			previousPlaceholder = value.Text() == "(image omitted: model does not support images)"
		case llm.ImageBlock:
			if !supportsImages {
				if !previousPlaceholder {
					result = append(result, map[string]any{"type": "text", "text": "(image omitted: model does not support images)"})
				}
				previousPlaceholder = true
				continue
			}
			result = append(result, anthropicImageBlock(value))
			previousPlaceholder = false
		default:
			return nil, fmt.Errorf("unsupported user content block %T", block)
		}
	}
	return result, nil
}

func anthropicImageBlock(image llm.ImageBlock) map[string]any {
	if image.Source() == llm.ImageSourceURL {
		return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": image.URL()}}
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": image.MediaType(), "data": base64.StdEncoding.EncodeToString(image.Data())}}
}

func anthropicToolResultContent(blocks []llm.ToolResultContentBlock, model Model) (any, error) {
	supportsImages := false
	for _, kind := range model.Input() {
		if kind == InputImage {
			supportsImages = true
			break
		}
	}
	if !supportsImages {
		parts := make([]string, 0, len(blocks))
		previousPlaceholder := false
		for _, block := range blocks {
			switch value := block.(type) {
			case llm.TextBlock:
				parts = append(parts, value.Text())
				previousPlaceholder = value.Text() == "(tool image omitted: model does not support images)"
			case llm.ImageBlock:
				if !previousPlaceholder {
					parts = append(parts, "(tool image omitted: model does not support images)")
				}
				previousPlaceholder = true
			default:
				return nil, fmt.Errorf("unsupported tool result block %T", block)
			}
		}
		return strings.Join(parts, "\n"), nil
	}
	hasImages := false
	for _, block := range blocks {
		if _, ok := block.(llm.ImageBlock); ok {
			hasImages = true
		}
	}
	if !hasImages {
		parts := make([]string, 0, len(blocks))
		for _, block := range blocks {
			text, ok := block.(llm.TextBlock)
			if !ok {
				return nil, fmt.Errorf("unsupported tool result block %T", block)
			}
			parts = append(parts, text.Text())
		}
		return strings.Join(parts, "\n"), nil
	}
	result := make([]any, 0, len(blocks)+1)
	hasText := false
	for _, block := range blocks {
		switch value := block.(type) {
		case llm.TextBlock:
			result = append(result, map[string]any{"type": "text", "text": value.Text()})
			hasText = true
		case llm.ImageBlock:
			result = append(result, anthropicImageBlock(value))
		default:
			return nil, fmt.Errorf("unsupported tool result block %T", block)
		}
	}
	if !hasText {
		result = append([]any{map[string]any{"type": "text", "text": "(see attached image)"}}, result...)
	}
	return result, nil
}

func encodeAnthropicTools(request Request, cacheControl *anthropicCacheControl, compat resolvedAnthropicCompat, isOAuth bool) ([]any, error) {
	immediate, deferred := splitDeferredTools(request, compat.supportsToolReferences)
	if len(immediate) == 0 && len(deferred) != 0 {
		names := make([]string, 0, len(deferred))
		for name := range deferred {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			immediate = append(immediate, deferred[name])
			delete(deferred, name)
		}
	}
	result := make([]any, 0, len(immediate)+len(deferred))
	appendTool := func(tool ToolDefinition, deferred bool, lastImmediate bool) error {
		var schema map[string]any
		if err := json.Unmarshal(tool.ParametersJSON(), &schema); err != nil {
			return err
		}
		strict, includeStrict, err := tool.ResolveJSONSchemaStrict(compat.supportsStrictTools)
		if err != nil {
			return err
		}
		if !includeStrict || !strict {
			legacy := map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}}
			if value, ok := schema["properties"]; ok {
				legacy["properties"] = value
			}
			if value, ok := schema["required"]; ok {
				legacy["required"] = value
			}
			schema = legacy
		}
		wire := map[string]any{"name": normalizeAnthropicToolName(tool.Name(), isOAuth), "description": tool.Description(), "input_schema": schema}
		if compat.supportsEagerToolInputStreaming {
			wire["eager_input_streaming"] = true
		}
		if includeStrict && strict {
			wire["strict"] = true
		}
		if deferred {
			wire["defer_loading"] = true
		}
		if lastImmediate && cacheControl != nil && compat.supportsCacheControlOnTools {
			wire["cache_control"] = cacheControl
		}
		result = append(result, wire)
		return nil
	}
	for index, tool := range immediate {
		if err := appendTool(tool, false, index == len(immediate)-1); err != nil {
			return nil, fmt.Errorf("%w: tool %d: %v", ErrAnthropicRequest, index, err)
		}
	}
	names := make([]string, 0, len(deferred))
	for name := range deferred {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := appendTool(deferred[name], true, false); err != nil {
			return nil, fmt.Errorf("%w: deferred tool %q: %v", ErrAnthropicRequest, name, err)
		}
	}
	return result, nil
}

type anthropicFailureSpec struct {
	kind        FailureKind
	cause       error
	message     string
	httpStatus  *int
	vendorCode  string
	retryAfter  *time.Duration
	shouldRetry *bool
}

type anthropicContentSlot struct {
	kind          string
	contentIndex  int
	providerIndex int
	text          strings.Builder
	signature     strings.Builder
	redactedData  string
	toolID        string
	toolName      string
	arguments     []byte
	rawArguments  []byte
	argumentDelta bool
}

type anthropicStream struct {
	ctx               context.Context
	cancel            context.CancelCauseFunc
	timeoutCancel     context.CancelFunc
	endpoint          string
	apiKey            string
	client            HTTPDoer
	clock             Clock
	timestamp         time.Time
	payload           []byte
	model             Model
	headers           map[string]string
	maxEventBytes     int
	maxErrorBodyBytes int
	onResponse        ResponseHook
	onHeaders         HeaderHook
	headerOverrides   map[string]*string
	configurationFail *anthropicFailureSpec
	preflight         *anthropicFailureSpec
	maxRetries        uint32
	maxRetryDelayMS   *uint64

	mu               sync.Mutex
	body             io.ReadCloser
	closeErr         error
	closed           bool
	finished         bool
	initialized      bool
	started          bool
	decoder          *responsesSSEDecoder
	queue            []llm.StreamEvent
	slots            map[int]*anthropicContentSlot
	toolNames        map[string]string
	nextContentIndex int
	responseID       string
	stopReason       string
	finishReason     llm.FinishReason
	hasFinishReason  bool
	sawMessageStart  bool
	sawMessageStop   bool
	usage            anthropicUsageAccumulator
}

type anthropicUsageAccumulator struct {
	input, output, cacheRead, cacheWrite uint64
	reasoning                            *uint64
	cacheWrite1h                         *uint64
	hasUsage                             bool
}

func newAnthropicFailureStream(ctx context.Context, clock Clock, model Model, kind FailureKind, cause error, message string) EventStream {
	if ctx == nil {
		ctx = context.Background()
	}
	if clock == nil {
		clock = time.Now
	}
	streamContext, cancel := context.WithCancelCause(ctx)
	return &anthropicStream{ctx: streamContext, cancel: cancel, clock: clock, timestamp: clock(), model: model, preflight: &anthropicFailureSpec{kind: kind, cause: cause, message: message}, slots: make(map[int]*anthropicContentSlot)}
}

func (s *anthropicStream) Next() (llm.StreamEvent, error) {
	if s == nil || s.done() {
		return nil, io.EOF
	}
	if cause := context.Cause(s.ctx); cause != nil {
		return s.fail(s.cancelled(cause))
	}
	if len(s.queue) != 0 {
		return s.pop(), nil
	}
	if s.preflight != nil {
		failure := s.preflight
		s.preflight = nil
		return s.fail(failure)
	}
	if !s.initialized {
		s.initialized = true
		if failure := s.initialize(); failure != nil {
			return s.fail(failure)
		}
		s.started = true
		return llm.NewStartEvent(assistantProvenanceForModel(s.model), s.timestamp)
	}
	for {
		if cause := context.Cause(s.ctx); cause != nil {
			return s.fail(s.cancelled(cause))
		}
		data, err := s.decoder.NextData()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if s.sawMessageStart && !s.sawMessageStop {
					return s.fail(&anthropicFailureSpec{kind: FailureInvalidResponse, cause: fmt.Errorf("%w: stream ended before message_stop", ErrAnthropicStream), message: "Anthropic stream ended before message_stop"})
				}
				return s.fail(&anthropicFailureSpec{kind: FailureInvalidResponse, cause: fmt.Errorf("%w: stream ended without a terminal message", ErrAnthropicStream), message: "Anthropic stream ended without a terminal message"})
			}
			if cause := context.Cause(s.ctx); cause != nil {
				return s.fail(s.cancelled(cause))
			}
			return s.fail(&anthropicFailureSpec{kind: FailureTransport, cause: fmt.Errorf("%w: read SSE: %v", ErrAnthropicStream, err), message: "Anthropic stream transport failed"})
		}
		if failure := s.process(data); failure != nil {
			return s.fail(failure)
		}
		if len(s.queue) != 0 {
			return s.pop(), nil
		}
	}
}

func (s *anthropicStream) initialize() *anthropicFailureSpec {
	for retryIndex := uint32(0); ; retryIndex++ {
		failure := s.initializeAttempt()
		if failure == nil || retryIndex >= s.maxRetries || !providerShouldRetry(failure.kind, failure.httpStatus, failure.shouldRetry) {
			return failure
		}
		if err := waitProviderRetry(s.ctx, retryIndex, failure.retryAfter, s.maxRetryDelayMS, failure.message); err != nil {
			if retryWaitCancellation(err) {
				return s.cancelled(err)
			}
			failure.cause = errors.Join(failure.cause, err)
			failure.message = safeResponsesErrorText(err, failure.message)
			return failure
		}
	}
}

func (s *anthropicStream) initializeAttempt() *anthropicFailureSpec {
	request, err := http.NewRequestWithContext(s.ctx, http.MethodPost, s.endpoint, bytes.NewReader(s.payload))
	if err != nil {
		return &anthropicFailureSpec{kind: FailureInvalidRequest, cause: fmt.Errorf("%w: construct HTTP request: %v", ErrAnthropicRequest, err), message: "Could not construct Anthropic Messages request"}
	}
	if validBearerAPIKey(s.apiKey) {
		if strings.Contains(s.apiKey, "sk-ant-oat") {
			request.Header.Set("Authorization", "Bearer "+s.apiKey)
		} else {
			request.Header.Set("x-api-key", s.apiKey)
		}
	}
	request.Header.Set("anthropic-version", "2023-06-01")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	for name, value := range s.headers {
		request.Header.Set(name, value)
	}
	if err := applyFinalHeaders(request.Header, s.model, s.onHeaders, s.headerOverrides); err != nil {
		return &anthropicFailureSpec{kind: FailureInvalidRequest, cause: err, message: "Anthropic header hook failed"}
	}
	if !anthropicHTTPHeadersHaveAuth(request.Header) {
		if s.configurationFail != nil {
			copy := *s.configurationFail
			return &copy
		}
		return &anthropicFailureSpec{kind: FailureConfiguration, cause: fmt.Errorf("%w: final authorization headers are missing", ErrInvalidAnthropicConfig), message: "Anthropic API authorization was removed before the request"}
	}
	response, err := invokeResponsesHTTPDoer(s.client, request)
	if err != nil {
		if cause := context.Cause(s.ctx); cause != nil {
			return s.cancelled(cause)
		}
		return &anthropicFailureSpec{kind: FailureTransport, cause: fmt.Errorf("%w: HTTP request: %v", ErrAnthropicStream, err), message: "Anthropic transport failed"}
	}
	if response == nil || response.Body == nil || isTypedNil(response.Body) {
		return &anthropicFailureSpec{kind: FailureInvalidResponse, cause: fmt.Errorf("%w: HTTP client returned nil response/body", ErrAnthropicStream), message: "Anthropic returned an invalid response"}
	}
	if s.onResponse != nil {
		if err := s.onResponse(s.model, responseInfo(response)); err != nil {
			_ = response.Body.Close()
			return &anthropicFailureSpec{kind: FailureInvalidResponse, cause: fmt.Errorf("response hook: %v", err), message: "Anthropic response hook rejected the response"}
		}
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return s.httpFailure(response)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || !strings.EqualFold(mediaType, "text/event-stream") {
		_ = response.Body.Close()
		return &anthropicFailureSpec{kind: FailureInvalidResponse, cause: fmt.Errorf("%w: response content type %q is not text/event-stream", ErrAnthropicStream, response.Header.Get("Content-Type")), message: "Anthropic returned a non-streaming response"}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = response.Body.Close()
		return s.cancelled(errAnthropicStreamClosed)
	}
	s.body = response.Body
	s.mu.Unlock()
	s.decoder = newResponsesSSEDecoder(response.Body, s.maxEventBytes)
	return nil
}

func anthropicHTTPHeadersHaveAuth(headers http.Header) bool {
	return strings.TrimSpace(headers.Get("x-api-key")) != "" || strings.TrimSpace(headers.Get("Authorization")) != "" || strings.TrimSpace(headers.Get("cf-aig-authorization")) != ""
}

func (s *anthropicStream) httpFailure(response *http.Response) *anthropicFailureSpec {
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, int64(s.maxErrorBodyBytes)+1))
	if len(body) > s.maxErrorBodyBytes {
		body = body[:s.maxErrorBodyBytes]
	}
	message := fmt.Sprintf("Anthropic API request failed with HTTP status %d", response.StatusCode)
	code := ""
	if utf8.Valid(body) {
		var payload struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &payload) == nil {
			if strings.TrimSpace(payload.Error.Message) != "" {
				message = payload.Error.Message
			}
			code = normalizeResponsesVendorCode(payload.Error.Type)
		}
	}
	status := response.StatusCode
	if isAnthropicContextOverflow(status, message) {
		return &anthropicFailureSpec{kind: FailureContextOverflow, cause: fmt.Errorf("%w: context window exceeded", ErrAnthropicStream), message: "Anthropic context window exceeded", httpStatus: &status, vendorCode: "context_length_exceeded"}
	}
	return &anthropicFailureSpec{kind: FailureHTTPStatus, cause: fmt.Errorf("Anthropic API HTTP %d: %s", status, message), message: message, httpStatus: &status, vendorCode: code, retryAfter: providerRetryAfter(response.Header, s.clock()), shouldRetry: providerRetryOverride(response.Header)}
}

func isAnthropicContextOverflow(status int, message string) bool {
	if status != http.StatusBadRequest || !utf8.ValidString(message) {
		return false
	}
	value := strings.ToLower(message)
	return strings.Contains(value, "prompt is too long") || strings.Contains(value, "context window") && (strings.Contains(value, "exceed") || strings.Contains(value, "too many"))
}

type anthropicEventEnvelope struct {
	Type string `json:"type"`
}

func unmarshalAnthropicEvent(data []byte, target any) error {
	if err := json.Unmarshal(data, target); err == nil {
		return nil
	}
	repaired := repairAnthropicJSON(data, true)
	return json.Unmarshal(repaired, target)
}

// repairAnthropicJSON mirrors pi's provider-boundary repair for malformed JSON
// string literals: raw control characters are escaped and a backslash before
// an invalid JSON escape is doubled. With final=false, a trailing backslash is
// retained so streamed input can be repaired monotonically when its next chunk
// arrives.
func repairAnthropicJSON(data []byte, final bool) []byte {
	result := make([]byte, 0, len(data)+8)
	inString := false
	for index := 0; index < len(data); index++ {
		character := data[index]
		if !inString {
			result = append(result, character)
			if character == '"' {
				inString = true
			}
			continue
		}
		if character == '"' {
			result = append(result, character)
			inString = false
			continue
		}
		if character == '\\' {
			if index+1 == len(data) {
				result = append(result, '\\')
				if final {
					result = append(result, '\\')
				}
				continue
			}
			next := data[index+1]
			if next == 'u' && index+5 < len(data) && allJSONHex(data[index+2:index+6]) {
				result = append(result, data[index:index+6]...)
				index += 5
				continue
			}
			if strings.ContainsRune(`"\\/bfnrtu`, rune(next)) {
				result = append(result, character, next)
				index++
				continue
			}
			result = append(result, '\\', '\\')
			continue
		}
		if character <= 0x1f {
			switch character {
			case '\b':
				result = append(result, `\b`...)
			case '\f':
				result = append(result, `\f`...)
			case '\n':
				result = append(result, `\n`...)
			case '\r':
				result = append(result, `\r`...)
			case '\t':
				result = append(result, `\t`...)
			default:
				result = append(result, fmt.Sprintf(`\u%04x`, character)...)
			}
			continue
		}
		result = append(result, character)
	}
	return result
}

func allJSONHex(value []byte) bool {
	if len(value) != 4 {
		return false
	}
	for _, character := range value {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' || character >= 'A' && character <= 'F' {
			continue
		}
		return false
	}
	return true
}

type anthropicUsageWire struct {
	InputTokens              *uint64 `json:"input_tokens"`
	OutputTokens             *uint64 `json:"output_tokens"`
	CacheReadInputTokens     *uint64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *uint64 `json:"cache_creation_input_tokens"`
	CacheCreation            *struct {
		Ephemeral1hInputTokens *uint64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	OutputTokenDetails *struct {
		ThinkingTokens *uint64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

type anthropicMessageStartEvent struct {
	Type    string `json:"type"`
	Message struct {
		ID    string             `json:"id"`
		Usage anthropicUsageWire `json:"usage"`
	} `json:"message"`
}

type anthropicContentEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock struct {
		Type      string         `json:"type"`
		Text      string         `json:"text"`
		Thinking  string         `json:"thinking"`
		Signature string         `json:"signature"`
		Data      string         `json:"data"`
		ID        string         `json:"id"`
		Name      string         `json:"name"`
		Input     map[string]any `json:"input"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		Signature   string `json:"signature"`
	} `json:"delta"`
}

type anthropicMessageDeltaEvent struct {
	Type  string `json:"type"`
	Delta struct {
		StopReason  string `json:"stop_reason"`
		StopDetails *struct {
			Explanation string `json:"explanation"`
		} `json:"stop_details"`
	} `json:"delta"`
	Usage *anthropicUsageWire `json:"usage"`
}

type anthropicErrorEvent struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (s *anthropicStream) process(data []byte) *anthropicFailureSpec {
	if !utf8.Valid(data) {
		return s.invalidEvent(errors.New("SSE data is not valid UTF-8"))
	}
	var envelope anthropicEventEnvelope
	if err := unmarshalAnthropicEvent(data, &envelope); err != nil {
		return s.invalidEvent(fmt.Errorf("decode SSE event: %v", err))
	}
	switch envelope.Type {
	case "ping":
		return nil
	case "message_start":
		var event anthropicMessageStartEvent
		if err := unmarshalAnthropicEvent(data, &event); err != nil {
			return s.invalidEvent(err)
		}
		if s.sawMessageStart {
			return s.invalidEvent(errors.New("duplicate message_start"))
		}
		if !utf8.ValidString(event.Message.ID) || strings.TrimSpace(event.Message.ID) == "" {
			return s.invalidEvent(errors.New("message_start is missing an id"))
		}
		s.sawMessageStart = true
		s.responseID = event.Message.ID
		s.usage.apply(event.Message.Usage)
		return nil
	case "content_block_start":
		var event anthropicContentEvent
		if err := unmarshalAnthropicEvent(data, &event); err != nil {
			return s.invalidEvent(err)
		}
		return s.startContent(event)
	case "content_block_delta":
		var event anthropicContentEvent
		if err := unmarshalAnthropicEvent(data, &event); err != nil {
			return s.invalidEvent(err)
		}
		return s.deltaContent(event)
	case "content_block_stop":
		var event anthropicContentEvent
		if err := unmarshalAnthropicEvent(data, &event); err != nil {
			return s.invalidEvent(err)
		}
		return s.stopContent(event.Index)
	case "message_delta":
		var event anthropicMessageDeltaEvent
		if err := unmarshalAnthropicEvent(data, &event); err != nil {
			return s.invalidEvent(err)
		}
		if event.Usage != nil {
			s.usage.apply(*event.Usage)
		}
		if event.Delta.StopReason != "" {
			s.stopReason = event.Delta.StopReason
			reason, failure := anthropicFinishReason(event.Delta.StopReason, event.Delta.StopDetails)
			if failure != nil {
				return failure
			}
			s.finishReason, s.hasFinishReason = reason, true
		}
		return nil
	case "message_stop":
		if !s.sawMessageStart {
			return s.invalidEvent(errors.New("message_stop arrived before message_start"))
		}
		if len(s.slots) != 0 {
			return s.invalidEvent(errors.New("message_stop arrived with an open content block"))
		}
		if !s.hasFinishReason {
			return s.invalidEvent(errors.New("message_stop arrived without stop_reason"))
		}
		usage, failure := s.usage.normalized(s.model)
		if failure != nil {
			return failure
		}
		metadata := &llm.AssistantResponseMetadata{ResponseID: s.responseID, RawStopReason: s.stopReason}
		done, err := llm.NewDoneEventWithMetadata(s.finishReason, usage, s.timestamp, assistantProvenanceForModel(s.model), metadata, nil)
		if err != nil {
			return s.invalidEvent(err)
		}
		s.sawMessageStop = true
		s.queue = append(s.queue, done)
		return nil
	case "error":
		var event anthropicErrorEvent
		if err := unmarshalAnthropicEvent(data, &event); err != nil {
			return s.invalidEvent(err)
		}
		message := event.Error.Message
		if strings.TrimSpace(message) == "" {
			message = "Anthropic stream returned an error"
		}
		return &anthropicFailureSpec{kind: FailureInvalidResponse, cause: errors.New(message), message: message, vendorCode: normalizeResponsesVendorCode(event.Error.Type)}
	default:
		// The upstream iterator filters unknown SSE event kinds. Ignore them so
		// future metadata events cannot become content or tool execution.
		return nil
	}
}

func (s *anthropicStream) startContent(event anthropicContentEvent) *anthropicFailureSpec {
	if event.Index < 0 {
		return s.invalidEvent(errors.New("negative content block index"))
	}
	if _, duplicate := s.slots[event.Index]; duplicate {
		return s.invalidEvent(fmt.Errorf("content block index %d started twice", event.Index))
	}
	slot := &anthropicContentSlot{kind: event.ContentBlock.Type, contentIndex: s.nextContentIndex, providerIndex: event.Index}
	switch event.ContentBlock.Type {
	case "text":
		start, err := llm.NewTextStartEvent(slot.contentIndex)
		if err != nil {
			return s.invalidEvent(err)
		}
		s.queue = append(s.queue, start)
		if event.ContentBlock.Text != "" {
			slot.text.WriteString(event.ContentBlock.Text)
			// The TypeScript event carries the initial block through `partial`.
			// Go events do not embed a mutable partial message, so surface the same
			// observable content as an immediate delta.
			delta, _ := llm.NewTextDeltaEvent(slot.contentIndex, event.ContentBlock.Text)
			s.queue = append(s.queue, delta)
		}
	case "thinking":
		start, err := llm.NewThinkingStartEvent(slot.contentIndex)
		if err != nil {
			return s.invalidEvent(err)
		}
		s.queue = append(s.queue, start)
		slot.signature.WriteString(event.ContentBlock.Signature)
		if event.ContentBlock.Thinking != "" {
			slot.text.WriteString(event.ContentBlock.Thinking)
			delta, _ := llm.NewThinkingDeltaEvent(slot.contentIndex, event.ContentBlock.Thinking)
			s.queue = append(s.queue, delta)
		}
	case "redacted_thinking":
		slot.redactedData = event.ContentBlock.Data
		if strings.TrimSpace(slot.redactedData) == "" {
			return s.invalidEvent(errors.New("redacted thinking block has no data"))
		}
		slot.text.WriteString("[Reasoning redacted]")
		start, err := llm.NewThinkingStartEvent(slot.contentIndex)
		if err != nil {
			return s.invalidEvent(err)
		}
		delta, _ := llm.NewThinkingDeltaEvent(slot.contentIndex, "[Reasoning redacted]")
		s.queue = append(s.queue, start, delta)
	case "tool_use":
		if strings.TrimSpace(event.ContentBlock.ID) == "" || strings.TrimSpace(event.ContentBlock.Name) == "" {
			return s.invalidEvent(errors.New("tool_use block has no id/name"))
		}
		slot.toolID, slot.toolName = event.ContentBlock.ID, event.ContentBlock.Name
		if original, ok := s.toolNames[strings.ToLower(slot.toolName)]; ok {
			slot.toolName = original
		}
		start, err := llm.NewToolCallStartEvent(slot.contentIndex, slot.toolID, slot.toolName)
		if err != nil {
			return s.invalidEvent(err)
		}
		s.queue = append(s.queue, start)
		if len(event.ContentBlock.Input) != 0 {
			initial, _ := json.Marshal(event.ContentBlock.Input)
			if string(initial) != "{}" {
				slot.arguments = append(slot.arguments, initial...)
			}
		}
	default:
		// Preserve provider index accounting but do not expose unknown blocks as
		// executable or replayable assistant content.
		slot.kind = "ignored"
		s.slots[event.Index] = slot
		return nil
	}
	s.slots[event.Index] = slot
	s.nextContentIndex++
	return nil
}

func (s *anthropicStream) deltaContent(event anthropicContentEvent) *anthropicFailureSpec {
	slot := s.slots[event.Index]
	if slot == nil {
		return s.invalidEvent(fmt.Errorf("delta has no open content block at index %d", event.Index))
	}
	if slot.kind == "ignored" {
		return nil
	}
	switch event.Delta.Type {
	case "text_delta":
		if slot.kind != "text" || !utf8.ValidString(event.Delta.Text) {
			return s.invalidEvent(errors.New("text delta does not match open block"))
		}
		slot.text.WriteString(event.Delta.Text)
		delta, err := llm.NewTextDeltaEvent(slot.contentIndex, event.Delta.Text)
		if err != nil {
			return s.invalidEvent(err)
		}
		s.queue = append(s.queue, delta)
	case "thinking_delta":
		if slot.kind != "thinking" || !utf8.ValidString(event.Delta.Thinking) {
			return s.invalidEvent(errors.New("thinking delta does not match open block"))
		}
		slot.text.WriteString(event.Delta.Thinking)
		delta, err := llm.NewThinkingDeltaEvent(slot.contentIndex, event.Delta.Thinking)
		if err != nil {
			return s.invalidEvent(err)
		}
		s.queue = append(s.queue, delta)
	case "signature_delta":
		if slot.kind != "thinking" || !utf8.ValidString(event.Delta.Signature) {
			return s.invalidEvent(errors.New("signature delta does not match open block"))
		}
		slot.signature.WriteString(event.Delta.Signature)
	case "input_json_delta":
		if slot.kind != "tool_use" || !utf8.ValidString(event.Delta.PartialJSON) {
			return s.invalidEvent(errors.New("tool input delta does not match open block"))
		}
		if !slot.argumentDelta {
			slot.arguments = slot.arguments[:0]
			slot.rawArguments = slot.rawArguments[:0]
			slot.argumentDelta = true
		}
		slot.rawArguments = append(slot.rawArguments, event.Delta.PartialJSON...)
		repaired := repairAnthropicJSON(slot.rawArguments, false)
		if !bytes.HasPrefix(repaired, slot.arguments) {
			return s.invalidEvent(errors.New("tool input repair changed non-monotonically"))
		}
		next := repaired[len(slot.arguments):]
		slot.arguments = append(slot.arguments, next...)
		if len(next) == 0 {
			return nil
		}
		delta, err := llm.NewToolCallDeltaEvent(slot.contentIndex, next)
		if err != nil {
			return s.invalidEvent(err)
		}
		s.queue = append(s.queue, delta)
	default:
		// Unknown deltas never change known content or tool arguments.
		return nil
	}
	return nil
}

func (s *anthropicStream) stopContent(index int) *anthropicFailureSpec {
	slot := s.slots[index]
	if slot == nil {
		return s.invalidEvent(fmt.Errorf("content block stop has no open block at index %d", index))
	}
	delete(s.slots, index)
	switch slot.kind {
	case "text":
		end, err := llm.NewTextEndEvent(slot.contentIndex, slot.text.String())
		if err != nil {
			return s.invalidEvent(err)
		}
		s.queue = append(s.queue, end)
	case "thinking":
		signature := slot.signature.String()
		block, err := llm.NewThinkingBlockWithSignature(slot.text.String(), signature, false)
		if err != nil {
			return s.invalidEvent(err)
		}
		end, err := llm.NewThinkingEndEvent(slot.contentIndex, block)
		if err != nil {
			return s.invalidEvent(err)
		}
		s.queue = append(s.queue, end)
	case "redacted_thinking":
		block, err := llm.NewThinkingBlockWithSignature(slot.text.String(), slot.redactedData, true)
		if err != nil {
			return s.invalidEvent(err)
		}
		end, err := llm.NewThinkingEndEvent(slot.contentIndex, block)
		if err != nil {
			return s.invalidEvent(err)
		}
		s.queue = append(s.queue, end)
	case "tool_use":
		if slot.argumentDelta {
			repaired := repairAnthropicJSON(slot.rawArguments, true)
			if !bytes.HasPrefix(repaired, slot.arguments) {
				return s.invalidEvent(errors.New("tool input repair changed non-monotonically at completion"))
			}
			if suffix := repaired[len(slot.arguments):]; len(suffix) != 0 {
				slot.arguments = append(slot.arguments, suffix...)
				delta, err := llm.NewToolCallDeltaEvent(slot.contentIndex, suffix)
				if err != nil {
					return s.invalidEvent(err)
				}
				s.queue = append(s.queue, delta)
			}
		}
		arguments := slot.arguments
		if len(arguments) == 0 {
			arguments = []byte("{}")
		}
		call, err := llm.NewToolCallBlock(slot.toolID, slot.toolName, arguments)
		if err != nil {
			return s.invalidEvent(fmt.Errorf("completed tool call is invalid: %v", err))
		}
		end, err := llm.NewToolCallEndEvent(slot.contentIndex, call)
		if err != nil {
			return s.invalidEvent(err)
		}
		s.queue = append(s.queue, end)
	case "ignored":
		// Forward-compatible block kinds are intentionally non-observable.
	}
	return nil
}

func anthropicFinishReason(value string, details *struct {
	Explanation string `json:"explanation"`
}) (llm.FinishReason, *anthropicFailureSpec) {
	switch value {
	case "end_turn", "pause_turn", "stop_sequence":
		return llm.FinishStop, nil
	case "max_tokens":
		return llm.FinishLength, nil
	case "tool_use":
		return llm.FinishToolUse, nil
	case "refusal":
		message := "The model refused to complete the request"
		if details != nil && strings.TrimSpace(details.Explanation) != "" {
			message = details.Explanation
		}
		return 0, &anthropicFailureSpec{kind: FailureInvalidResponse, cause: errors.New(message), message: message, vendorCode: "refusal"}
	case "sensitive":
		return 0, &anthropicFailureSpec{kind: FailureInvalidResponse, cause: errors.New("provider stopped with: sensitive"), message: "Provider stopped with: sensitive", vendorCode: "sensitive"}
	default:
		return 0, &anthropicFailureSpec{kind: FailureInvalidResponse, cause: fmt.Errorf("%w: unhandled stop reason %q", ErrAnthropicStream, value), message: "Anthropic returned an unknown stop reason"}
	}
}

func (u *anthropicUsageAccumulator) apply(raw anthropicUsageWire) {
	if raw.InputTokens != nil {
		u.input = *raw.InputTokens
		u.hasUsage = true
	}
	if raw.OutputTokens != nil {
		u.output = *raw.OutputTokens
		u.hasUsage = true
	}
	if raw.CacheReadInputTokens != nil {
		u.cacheRead = *raw.CacheReadInputTokens
		u.hasUsage = true
	}
	if raw.CacheCreationInputTokens != nil {
		u.cacheWrite = *raw.CacheCreationInputTokens
		u.hasUsage = true
	}
	if raw.CacheCreation != nil && raw.CacheCreation.Ephemeral1hInputTokens != nil {
		value := *raw.CacheCreation.Ephemeral1hInputTokens
		u.cacheWrite1h = &value
		u.hasUsage = true
	}
	if raw.OutputTokenDetails != nil && raw.OutputTokenDetails.ThinkingTokens != nil {
		value := *raw.OutputTokenDetails.ThinkingTokens
		u.reasoning = &value
		u.hasUsage = true
	}
}

func (u anthropicUsageAccumulator) normalized(model Model) (llm.Usage, *anthropicFailureSpec) {
	usage, err := llm.NewUsage(llm.UsageSpec{Input: u.input, Output: u.output, CacheRead: u.cacheRead, CacheWrite: u.cacheWrite, Reasoning: u.reasoning, CacheWrite1h: u.cacheWrite1h})
	if err != nil {
		return llm.Usage{}, &anthropicFailureSpec{kind: FailureInvalidResponse, cause: fmt.Errorf("%w: invalid token usage: %v", ErrAnthropicStream, err), message: "Anthropic returned invalid usage"}
	}
	if u.hasUsage {
		usage, err = usage.WithCost(model.CalculateCost(usage))
		if err != nil {
			return llm.Usage{}, &anthropicFailureSpec{kind: FailureInvalidResponse, cause: err, message: "Anthropic returned invalid usage cost"}
		}
	}
	return usage, nil
}

func (s *anthropicStream) invalidEvent(err error) *anthropicFailureSpec {
	return &anthropicFailureSpec{kind: FailureInvalidResponse, cause: fmt.Errorf("%w: %v", ErrAnthropicStream, err), message: "Anthropic stream returned invalid data"}
}

func (s *anthropicStream) cancelled(cause error) *anthropicFailureSpec {
	joined := error(ErrAnthropicAborted)
	if cause != nil {
		joined = errors.Join(joined, cause)
	}
	return &anthropicFailureSpec{kind: FailureCancelled, cause: joined, message: ErrAnthropicAborted.Error()}
}

func (s *anthropicStream) fail(spec *anthropicFailureSpec) (llm.StreamEvent, error) {
	if spec == nil {
		return nil, io.EOF
	}
	message := spec.message
	if strings.TrimSpace(message) == "" {
		message = safeResponsesErrorText(spec.cause, "Anthropic Messages request failed")
	}
	failure, err := NewProviderFailure(ProviderFailureSpec{
		Kind: spec.kind, Message: message, RetryMessage: httpRetryMessage("Anthropic API", spec.kind, spec.httpStatus),
		Cause: spec.cause, HTTPStatus: spec.httpStatus, VendorCode: spec.vendorCode, RetryAfter: spec.retryAfter,
	})
	if err != nil {
		s.finish()
		return nil, closedStreamError(err)
	}
	terminal, err := llm.NewFailure(failure.Error(), failure)
	if err != nil {
		s.finish()
		return nil, closedStreamError(err)
	}
	reason := llm.FinishError
	if spec.kind == FailureCancelled {
		reason = llm.FinishAborted
	}
	usage, _ := s.usage.normalized(s.model)
	var response *llm.AssistantResponseMetadata
	if s.responseID != "" || s.stopReason != "" {
		response = &llm.AssistantResponseMetadata{ResponseID: s.responseID, RawStopReason: s.stopReason}
	}
	event, err := llm.NewErrorEventWithMetadata(reason, terminal, usage, s.timestamp, assistantProvenanceForModel(s.model), response, nil)
	if err != nil {
		s.finish()
		return nil, closedStreamError(err)
	}
	s.finish()
	return event, nil
}

func (s *anthropicStream) pop() llm.StreamEvent {
	event := s.queue[0]
	s.queue[0] = nil
	s.queue = s.queue[1:]
	if _, terminal := event.(llm.DoneEvent); terminal {
		s.finish()
	}
	return event
}

func (s *anthropicStream) done() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed || s.finished
}

func (s *anthropicStream) finish() {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	body := s.body
	s.body = nil
	s.mu.Unlock()
	s.cancel(errAnthropicStreamDone)
	if s.timeoutCancel != nil {
		s.timeoutCancel()
	}
	if body != nil {
		if err := body.Close(); err != nil {
			s.mu.Lock()
			s.closeErr = errors.Join(s.closeErr, err)
			s.mu.Unlock()
		}
	}
}

func (s *anthropicStream) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.closed = true
	body := s.body
	s.body = nil
	s.mu.Unlock()
	s.cancel(errAnthropicStreamClosed)
	if s.timeoutCancel != nil {
		s.timeoutCancel()
	}
	if body != nil {
		if err := body.Close(); err != nil {
			s.mu.Lock()
			s.closeErr = errors.Join(s.closeErr, err)
			s.mu.Unlock()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeErr
}
