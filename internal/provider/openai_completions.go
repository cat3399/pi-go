package provider

// This file implements the standard OpenAI Chat Completions wire dialect.  It
// intentionally consumes the generic Model/Request values: the provider
// name selects a router registration, while the API dialect selects this
// adapter.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/llm"
)

const (
	OpenAICompletionsAPI         = "openai-completions"
	defaultOpenAICompletionsBase = "https://api.openai.com/v1"
	defaultCompletionsEventBytes = 1 << 20
	defaultCompletionsErrorBytes = 64 << 10
)

var (
	ErrInvalidOpenAICompletionsConfig = errors.New("invalid OpenAI Chat Completions configuration")
	ErrOpenAICompletionsRequest       = errors.New("invalid OpenAI Chat Completions request")
	ErrOpenAICompletionsStream        = errors.New("invalid OpenAI Chat Completions stream")
	ErrOpenAICompletionsAborted       = errors.New("OpenAI Chat Completions request aborted")
	errOpenAICompletionsStreamClosed  = errors.New("OpenAI Chat Completions stream closed")
	errOpenAICompletionsStreamDone    = errors.New("OpenAI Chat Completions stream finished")
)

// OpenAICompletionsConfig is deliberately equivalent to the Responses
// transport configuration. Per-model URL, request credentials, output cap,
// and headers are resolved from Request at stream time.
type OpenAICompletionsConfig struct {
	BaseURL           string
	APIKey            string
	Headers           map[string]string
	Client            HTTPDoer
	Clock             Clock
	MaxEventBytes     int
	MaxErrorBodyBytes int
}

type OpenAICompletionsProvider struct {
	endpoint, apiKey  string
	headers           map[string]string
	client            HTTPDoer
	clock             Clock
	maxEventBytes     int
	maxErrorBodyBytes int
	configurationFail *completionsFailureSpec
}

func NewOpenAICompletionsProvider(config OpenAICompletionsConfig) (*OpenAICompletionsProvider, error) {
	base := config.BaseURL
	if base == "" {
		base = defaultOpenAICompletionsBase
	}
	endpoint, err := completionsEndpoint(base)
	if err != nil {
		return nil, err
	}
	if config.MaxEventBytes < 0 || config.MaxErrorBodyBytes < 0 {
		return nil, fmt.Errorf("%w: byte limits cannot be negative", ErrInvalidOpenAICompletionsConfig)
	}
	client := config.Client
	if client == nil {
		client = http.DefaultClient
	} else if isTypedNil(client) {
		return nil, fmt.Errorf("%w: HTTP client is a typed nil", ErrInvalidOpenAICompletionsConfig)
	}
	clock := config.Clock
	if clock == nil {
		clock = time.Now
	}
	p := &OpenAICompletionsProvider{endpoint: endpoint, apiKey: config.APIKey, headers: cloneStrings(config.Headers), client: client, clock: synchronizedClock(clock), maxEventBytes: config.MaxEventBytes, maxErrorBodyBytes: config.MaxErrorBodyBytes}
	if p.maxEventBytes == 0 {
		p.maxEventBytes = defaultCompletionsEventBytes
	}
	if p.maxErrorBodyBytes == 0 {
		p.maxErrorBodyBytes = defaultCompletionsErrorBytes
	}
	if !utf8.ValidString(p.apiKey) || strings.TrimSpace(p.apiKey) == "" {
		p.configurationFail = &completionsFailureSpec{kind: FailureConfiguration, cause: fmt.Errorf("%w: API key must be non-empty valid UTF-8", ErrInvalidOpenAICompletionsConfig), message: "OpenAI API key is not configured"}
	} else if strings.ContainsFunc(p.apiKey, unicode.IsControl) {
		p.configurationFail = &completionsFailureSpec{kind: FailureConfiguration, cause: fmt.Errorf("%w: API key contains a control character", ErrInvalidOpenAICompletionsConfig), message: "OpenAI API key is invalid"}
	}
	return p, nil
}

func completionsEndpoint(base string) (string, error) {
	if !utf8.ValidString(base) || strings.TrimSpace(base) == "" {
		return "", fmt.Errorf("%w: base URL must be non-empty valid UTF-8", ErrInvalidOpenAICompletionsConfig)
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("%w: parse base URL: %v", ErrInvalidOpenAICompletionsConfig, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("%w: base URL must be an absolute HTTP(S) URL without user info, query, or fragment", ErrInvalidOpenAICompletionsConfig)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/chat/completions"
	u.RawPath = ""
	return u.String(), nil
}

func (*OpenAICompletionsProvider) SupportsModel(model Model) bool {
	return model.API() == OpenAICompletionsAPI
}

func (p *OpenAICompletionsProvider) Stream(ctx context.Context, request Request) EventStream {
	clock := Clock(time.Now)
	if p != nil && p.clock != nil {
		clock = p.clock
	}
	if ctx == nil {
		return newCompletionsFailureStream(context.Background(), clock, request.Model(), FailureInvalidRequest, fmt.Errorf("%w: nil context", ErrInvalidRequest), "OpenAI Chat Completions request requires a context")
	}
	if p == nil {
		return newCompletionsFailureStream(ctx, clock, request.Model(), FailureConfiguration, fmt.Errorf("%w: nil provider", ErrInvalidOpenAICompletionsConfig), "OpenAI Chat Completions provider is not configured")
	}
	if err := request.validate(); err != nil {
		return newCompletionsFailureStream(ctx, clock, request.Model(), FailureInvalidRequest, err, "")
	}
	if request.Model().API() != OpenAICompletionsAPI {
		return newCompletionsFailureStream(ctx, clock, request.Model(), FailureConfiguration, fmt.Errorf("%w: model routes to provider %q API %q", ErrOpenAICompletionsRequest, request.Model().Provider(), request.Model().API()), "")
	}
	payload, err := encodeOpenAICompletionsRequest(request)
	if err != nil {
		return newCompletionsFailureStream(ctx, clock, request.Model(), FailureInvalidRequest, err, "")
	}
	options := request.StreamOptions()
	if payload, err = applyPayloadHook(options.OnPayload, request.Model(), payload); err != nil {
		return newCompletionsFailureStream(ctx, clock, request.Model(), FailureInvalidRequest, err, "")
	}
	endpoint := p.endpoint
	if base := request.Model().BaseURL(); base != "" {
		endpoint, err = completionsEndpoint(base)
		if err != nil {
			return newCompletionsFailureStream(ctx, clock, request.Model(), FailureInvalidRequest, err, "")
		}
	}
	streamCtx, cancel := context.WithCancelCause(ctx)
	headers := mergeResponseHeaders(request.Model().Headers(), p.headers, options.Headers)
	if sessionID := options.SessionID; sessionID != "" && options.CacheRetention != CacheRetentionNone && completionsSendSessionAffinity(request.Model()) {
		switch completionsSessionAffinityFormat(request.Model()) {
		case "openrouter":
			headers["x-session-id"] = sessionID
		case "openai-nosession":
			headers["x-client-request-id"] = sessionID
			headers["x-session-affinity"] = sessionID
		default:
			headers["session_id"] = sessionID
			headers["x-client-request-id"] = sessionID
			headers["x-session-affinity"] = sessionID
		}
	}
	client := p.client
	if options.Fetch != nil {
		client = options.Fetch
	}
	return &openAICompletionsStream{ctx: streamCtx, cancel: cancel, endpoint: endpoint, apiKey: requestAPIKey(request, p.apiKey), client: client, clock: clock, timestamp: clock(), payload: payload, model: request.Model(), headers: headers, maxEventBytes: p.maxEventBytes, maxErrorBodyBytes: p.maxErrorBodyBytes, onResponse: options.OnResponse, onHeaders: options.OnHeaders, headerOverrides: cloneHeaderOverrides(options.HeaderOverrides), configurationFail: p.configurationFail, tools: make(map[int]*completionsToolSlot), pendingReasoningDetails: make(map[string]completionsReasoningDetail)}
}

func completionsHasAuthorization(groups ...map[string]string) bool {
	for _, group := range groups {
		for name, value := range group {
			if strings.EqualFold(name, "authorization") && strings.TrimSpace(value) != "" {
				return true
			}
		}
	}
	return false
}

func completionsCompat(model Model) *OpenAICompletionsCompat {
	return model.Compat().OpenAICompletions
}
func completionsSupportsUsage(model Model) bool {
	compat := completionsCompat(model)
	return compat == nil || compat.SupportsUsageInStreaming == nil || *compat.SupportsUsageInStreaming
}
func completionsSupportsFinishReason(model Model) bool {
	compat := completionsCompat(model)
	return compat == nil || compat.SupportsFinishReason == nil || *compat.SupportsFinishReason
}
func completionsSupportsStore(model Model) bool {
	compat := completionsCompat(model)
	return compat == nil || compat.SupportsStore == nil || *compat.SupportsStore
}
func completionsSupportsDeveloperRole(model Model) bool {
	compat := completionsCompat(model)
	return compat == nil || compat.SupportsDeveloperRole == nil || *compat.SupportsDeveloperRole
}
func completionsSupportsStrict(model Model) bool {
	compat := completionsCompat(model)
	return compat == nil || compat.SupportsStrictMode == nil || *compat.SupportsStrictMode
}
func completionsRequiresToolResultName(model Model) bool {
	compat := completionsCompat(model)
	return compat != nil && compat.RequiresToolResultName != nil && *compat.RequiresToolResultName
}
func completionsRequiresAssistantAfterToolResult(model Model) bool {
	compat := completionsCompat(model)
	return compat != nil && compat.RequiresAssistantAfterToolResult != nil && *compat.RequiresAssistantAfterToolResult
}
func completionsMaxTokensField(model Model) string {
	compat := completionsCompat(model)
	if compat != nil && compat.MaxTokensField != nil {
		return *compat.MaxTokensField
	}
	return "max_completion_tokens"
}
func completionsThinkingFormat(model Model) string {
	compat := completionsCompat(model)
	if compat != nil && compat.ThinkingFormat != nil {
		return *compat.ThinkingFormat
	}
	return "openai"
}
func completionsSendSessionAffinity(model Model) bool {
	compat := completionsCompat(model)
	return compat != nil && compat.SendSessionAffinityHeaders != nil && *compat.SendSessionAffinityHeaders
}
func completionsSessionAffinityFormat(model Model) string {
	compat := completionsCompat(model)
	if compat != nil && compat.SessionAffinityFormat != nil {
		return *compat.SessionAffinityFormat
	}
	return "openai"
}
func completionsSupportsReasoningEffort(model Model) bool {
	compat := completionsCompat(model)
	return compat == nil || compat.SupportsReasoningEffort == nil || *compat.SupportsReasoningEffort
}
func completionsRequiresThinkingAsText(model Model) bool {
	compat := completionsCompat(model)
	return compat != nil && compat.RequiresThinkingAsText != nil && *compat.RequiresThinkingAsText
}
func completionsRequiresReasoningContent(model Model) bool {
	compat := completionsCompat(model)
	return compat != nil && compat.RequiresReasoningContentOnAssistantMessages != nil && *compat.RequiresReasoningContentOnAssistantMessages
}
func completionsSupportsImages(model Model) bool {
	for _, input := range model.Input() {
		if input == InputImage {
			return true
		}
	}
	return false
}

type completionsRequestPayload struct {
	Model               string                    `json:"model"`
	Messages            []any                     `json:"messages"`
	Tools               []completionsFunctionTool `json:"tools,omitempty"`
	ToolChoice          any                       `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool                     `json:"parallel_tool_calls,omitempty"`
	Stream              bool                      `json:"stream"`
	StreamOptions       *completionsStreamOptions `json:"stream_options,omitempty"`
	MaxCompletionTokens uint64                    `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     string                    `json:"reasoning_effort,omitempty"`
	Thinking            *completionsThinking      `json:"thinking,omitempty"`
	MaxTokens           uint64                    `json:"max_tokens,omitempty"`
	Store               *bool                     `json:"store,omitempty"`
}
type completionsStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}
type completionsThinking struct {
	Type string `json:"type"`
}
type completionsFunctionTool struct {
	Type     string              `json:"type"`
	Function completionsFunction `json:"function"`
}
type completionsFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict,omitempty"`
}

func encodeOpenAICompletionsRequest(request Request) ([]byte, error) {
	messages := make([]any, 0, len(request.Messages())+1)
	if request.SystemPrompt() != "" {
		role := "system"
		if request.Model().Reasoning() && completionsSupportsDeveloperRole(request.Model()) {
			role = "developer"
		}
		messages = append(messages, map[string]any{"role": role, "content": request.SystemPrompt()})
	}
	thinkingFormat := completionsThinkingFormat(request.Model())
	if request.Model().Reasoning() && thinkingFormat != "openai" && thinkingFormat != "deepseek" {
		return nil, fmt.Errorf("%w: thinking format %q is not implemented", ErrOpenAICompletionsRequest, thinkingFormat)
	}
	lastToolResult := false
	pendingToolImages := make([]any, 0)
	toolCallIDs := make(map[string]string)
	deferredEnabled := false
	if compat := completionsCompat(request.Model()); compat != nil && compat.DeferredToolsMode != nil && *compat.DeferredToolsMode == "kimi" {
		deferredEnabled = true
	}
	immediateDefinitions, deferredDefinitions := splitDeferredTools(request, deferredEnabled)
	pendingDeferred := map[string]struct{}{}
	flushDeferred := func() error {
		if len(pendingDeferred) == 0 {
			return nil
		}
		definitions := make([]ToolDefinition, 0, len(pendingDeferred))
		for _, tool := range request.Tools() {
			if _, ok := pendingDeferred[tool.Name()]; ok {
				definitions = append(definitions, tool)
			}
		}
		pendingDeferred = map[string]struct{}{}
		if len(definitions) == 0 {
			return nil
		}
		tools, err := encodeCompletionsTools(definitions, request.Model())
		if err != nil {
			return err
		}
		messages = append(messages, map[string]any{"role": "system", "tools": tools})
		return nil
	}
	flushToolImages := func() {
		if len(pendingToolImages) == 0 {
			return
		}
		if lastToolResult && completionsRequiresAssistantAfterToolResult(request.Model()) {
			messages = append(messages, map[string]any{"role": "assistant", "content": "I have processed the tool results."})
		}
		messages = append(messages, map[string]any{"role": "user", "content": append([]any{map[string]string{"type": "text", "text": "Attached image(s) from tool result:"}}, pendingToolImages...)})
		pendingToolImages = pendingToolImages[:0]
		lastToolResult = false
	}
	for i, message := range request.Messages() {
		_, toolText := message.(llm.ToolResultMessage)
		_, toolContent := message.(llm.ToolResultContentMessage)
		if !toolText && !toolContent {
			if err := flushDeferred(); err != nil {
				return nil, err
			}
		}
		if !toolText && !toolContent && len(pendingToolImages) != 0 {
			flushToolImages()
		}
		if _, user := message.(llm.UserTextMessage); user && lastToolResult && completionsRequiresAssistantAfterToolResult(request.Model()) {
			messages = append(messages, map[string]any{"role": "assistant", "content": "I have processed the tool results."})
		}
		if _, user := message.(llm.UserContentMessage); user && lastToolResult && completionsRequiresAssistantAfterToolResult(request.Model()) {
			messages = append(messages, map[string]any{"role": "assistant", "content": "I have processed the tool results."})
		}
		encoded, include, err := encodeCompletionsMessage(message, request.ReplayTarget(), request.Model(), toolCallIDs)
		if err != nil {
			return nil, fmt.Errorf("%w: message %d: %w", ErrOpenAICompletionsRequest, i, err)
		}
		if include {
			messages = append(messages, encoded)
		}
		switch message.(type) {
		case llm.ToolResultMessage, llm.ToolResultContentMessage:
			lastToolResult = true
		default:
			lastToolResult = false
		}
		if result, ok := message.(llm.ToolResultContentMessage); ok {
			parts, hasImages, err := completionsToolResultImageParts(result.Content())
			if err != nil {
				return nil, fmt.Errorf("%w: message %d: %w", ErrOpenAICompletionsRequest, i, err)
			}
			if hasImages && completionsSupportsImages(request.Model()) {
				pendingToolImages = append(pendingToolImages, parts...)
			}
		}
		if deferredEnabled {
			if result, ok := message.(llm.ToolResultMessage); ok {
				for _, name := range result.AddedToolNames() {
					if _, exists := deferredDefinitions[name]; exists {
						pendingDeferred[name] = struct{}{}
					}
				}
			}
			if result, ok := message.(llm.ToolResultContentMessage); ok {
				for _, name := range result.AddedToolNames() {
					if _, exists := deferredDefinitions[name]; exists {
						pendingDeferred[name] = struct{}{}
					}
				}
			}
		}
	}
	flushToolImages()
	if err := flushDeferred(); err != nil {
		return nil, err
	}
	tools, err := encodeCompletionsTools(immediateDefinitions, request.Model())
	if err != nil {
		return nil, err
	}
	p := completionsRequestPayload{Model: request.Model().ID(), Messages: messages, Tools: tools, Stream: true}
	if completionsSupportsUsage(request.Model()) {
		p.StreamOptions = &completionsStreamOptions{IncludeUsage: true}
	}
	if len(tools) > 0 {
		parallel := request.ParallelToolCalls()
		p.ParallelToolCalls = &parallel
	}
	if choice, ok := request.ToolChoice(); ok {
		if choice.Name != "" {
			p.ToolChoice = map[string]any{"type": "function", "function": map[string]string{"name": choice.Name}}
		} else if choice.Mode != "" {
			p.ToolChoice = choice.Mode
		}
	}
	max := request.Model().MaxTokens()
	if configured := request.StreamOptions().MaxTokens; configured != nil {
		max = *configured
	}
	if completionsMaxTokensField(request.Model()) == "max_tokens" {
		p.MaxTokens = max
	} else {
		p.MaxCompletionTokens = max
	}
	if completionsSupportsStore(request.Model()) {
		value := false
		p.Store = &value
	}
	if request.Model().Reasoning() && thinkingFormat == "deepseek" {
		level := request.ThinkingLevel()
		if level == "" {
			level = ThinkingOff
		}
		if level == ThinkingOff {
			off, configured := request.Model().ThinkingLevelMap()[ThinkingOff]
			if !configured || off != nil {
				p.Thinking = &completionsThinking{Type: "disabled"}
			}
		} else {
			p.Thinking = &completionsThinking{Type: "enabled"}
			if completionsSupportsReasoningEffort(request.Model()) {
				effort := string(level)
				if mapped, configured := request.Model().ThinkingLevelMap()[level]; configured && mapped != nil {
					effort = *mapped
				}
				p.ReasoningEffort = effort
			}
		}
	} else if effort, ok := request.Model().ThinkingEffort(request.ThinkingLevel()); ok && effort != "" && completionsSupportsReasoningEffort(request.Model()) {
		p.ReasoningEffort = effort
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("%w: encode JSON: %w", ErrOpenAICompletionsRequest, err)
	}
	return encoded, nil
}

func encodeCompletionsTools(definitions []ToolDefinition, model Model) ([]completionsFunctionTool, error) {
	tools := make([]completionsFunctionTool, 0, len(definitions))
	for _, tool := range definitions {
		if err := tool.validate(); err != nil {
			return nil, err
		}
		function := completionsFunction{Name: tool.Name(), Description: tool.Description(), Parameters: json.RawMessage(tool.ParametersJSON())}
		if completionsSupportsStrict(model) {
			strict := tool.Strict()
			function.Strict = &strict
		}
		tools = append(tools, completionsFunctionTool{Type: "function", Function: function})
	}
	return tools, nil
}

func encodeCompletionsMessage(message llm.ConversationMessage, target llm.AssistantProvenance, model Model, toolCallIDs map[string]string) (any, bool, error) {
	switch m := message.(type) {
	case llm.UserTextMessage:
		blocks := m.Content()
		if len(blocks) == 0 {
			return nil, false, nil
		}
		var b strings.Builder
		for _, x := range blocks {
			b.WriteString(x.Text())
		}
		return map[string]any{"role": "user", "content": b.String()}, true, nil
	case llm.UserContentMessage:
		parts, err := completionsUserParts(m.Content(), model)
		if err != nil {
			return nil, false, err
		}
		if len(parts) == 0 {
			return nil, false, nil
		}
		return map[string]any{"role": "user", "content": parts}, true, nil
	case llm.AssistantTextMessage:
		return encodeCompletionsAssistant(m.Blocks(), m, target, model, toolCallIDs)
	case llm.AssistantRichMessage:
		return encodeCompletionsAssistant(m.Blocks(), m, target, model, toolCallIDs)
	case llm.AssistantToolUseMessage:
		return encodeCompletionsAssistant(m.Blocks(), m, target, model, toolCallIDs)
	case llm.AssistantFailureMessage:
		return nil, false, nil
	case llm.ToolResultMessage:
		callID := m.ToolCallID()
		if normalized, ok := toolCallIDs[callID]; ok {
			callID = normalized
		}
		out := map[string]any{"role": "tool", "tool_call_id": callID, "content": joinToolTextBlocks(m.Content())}
		if completionsRequiresToolResultName(model) {
			out["name"] = m.ToolName()
		}
		return out, true, nil
	case llm.ToolResultContentMessage:
		text, images, err := completionsToolResult(m.Content(), model)
		if err != nil {
			return nil, false, err
		}
		if text == "" {
			if images {
				text = "(see attached image)"
			} else {
				text = "(no tool output)"
			}
		}
		callID := m.ToolCallID()
		if normalized, ok := toolCallIDs[callID]; ok {
			callID = normalized
		}
		out := map[string]any{"role": "tool", "tool_call_id": callID, "content": text}
		if completionsRequiresToolResultName(model) {
			out["name"] = m.ToolName()
		}
		return out, true, nil
	default:
		return nil, false, fmt.Errorf("unsupported type %T", message)
	}
}

func completionsToolResultImageParts(blocks []llm.ToolResultContentBlock) ([]any, bool, error) {
	parts := make([]any, 0)
	for _, block := range blocks {
		image, ok := block.(llm.ImageBlock)
		if !ok {
			continue
		}
		url := image.URL()
		if image.Source() == llm.ImageSourceData {
			url = "data:" + image.MediaType() + ";base64," + base64.StdEncoding.EncodeToString(image.Data())
		}
		parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": url}})
	}
	return parts, len(parts) != 0, nil
}

type completionsProvenanceCarrier interface {
	AssistantProvenance() llm.AssistantProvenance
}

func encodeCompletionsAssistant(blocks []llm.AssistantBlock, source completionsProvenanceCarrier, target llm.AssistantProvenance, model Model, toolCallIDs map[string]string) (any, bool, error) {
	var text strings.Builder
	calls := make([]any, 0)
	reasoningField, reasoningText := "", ""
	reasoningDetails := make([]any, 0)
	provenance := source.AssistantProvenance()
	same := provenance.Matches(target.Provider, target.API, target.Model)
	for _, block := range blocks {
		switch b := block.(type) {
		case llm.TextBlock:
			text.WriteString(b.Text())
		case llm.ThinkingBlock:
			if b.Redacted() && !same {
				continue
			}
			if signature, ok := b.ThinkingSignature(); ok && same && validCompletionsReasoningField(signature) {
				reasoningField = signature
				if reasoningText != "" {
					reasoningText += "\n"
				}
				reasoningText += b.Thinking()
			} else if !same || completionsRequiresThinkingAsText(model) {
				text.WriteString(b.Thinking())
			}
		case llm.ToolCallBlock:
			callID := b.ID()
			if !same {
				callID = normalizeCompletionsToolCallID(callID, model.Provider() == OpenAIProviderID)
			}
			if callID != b.ID() {
				toolCallIDs[b.ID()] = callID
			}
			calls = append(calls, map[string]any{"id": callID, "type": "function", "function": map[string]string{"name": b.Name(), "arguments": string(b.ArgumentsJSON())}})
			if signature, ok := b.ThoughtSignature(); ok && same {
				if detail, valid := decodeCompletionsReasoningDetail(signature); valid {
					reasoningDetails = append(reasoningDetails, detail)
				}
			}
		default:
			return nil, false, fmt.Errorf("unsupported assistant block %T", block)
		}
	}
	if text.Len() == 0 && len(calls) == 0 {
		return nil, false, nil
	}
	content := any(text.String())
	if text.Len() == 0 {
		content = nil
	}
	m := map[string]any{"role": "assistant", "content": content}
	if len(calls) > 0 {
		m["tool_calls"] = calls
	}
	if reasoningField != "" {
		m[reasoningField] = reasoningText
	}
	if len(reasoningDetails) != 0 {
		m["reasoning_details"] = reasoningDetails
	}
	if completionsRequiresReasoningContent(model) && model.Reasoning() {
		if _, ok := m["reasoning_content"]; !ok {
			m["reasoning_content"] = ""
		}
	}
	return m, true, nil
}

func normalizeCompletionsToolCallID(value string, openAI bool) string {
	if separator := strings.IndexByte(value, '|'); separator >= 0 {
		callID := sanitizeCompletionsToolCallIDPart(value[:separator])
		itemID := sanitizeCompletionsToolCallIDPart(value[separator+1:])
		combined := callID
		if itemID != "" {
			combined += "_" + itemID
		}
		if len(combined) <= 40 {
			return combined
		}
		sum := sha256.Sum256([]byte(value))
		hash := fmt.Sprintf("%x", sum[:4])
		prefixLength := 40 - len(hash) - 1
		if prefixLength < 1 {
			prefixLength = 1
		}
		if len(callID) > prefixLength {
			callID = callID[:prefixLength]
		}
		return callID + "_" + hash
	}
	if !openAI {
		return value
	}
	runes := []rune(value)
	if len(runes) <= 40 {
		return value
	}
	return string(runes[:40])
}

func sanitizeCompletionsToolCallIDPart(value string) string {
	var normalized strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-' {
			normalized.WriteRune(character)
		} else {
			normalized.WriteByte('_')
		}
	}
	return normalized.String()
}

func joinToolTextBlocks(blocks []llm.TextBlock) string {
	parts := make([]string, len(blocks))
	for index, block := range blocks {
		parts[index] = block.Text()
	}
	return strings.Join(parts, "\n")
}
func completionsUserParts(blocks []llm.UserContentBlock, model Model) ([]any, error) {
	parts := make([]any, 0, len(blocks))
	previousPlaceholder := false
	for _, block := range blocks {
		switch b := block.(type) {
		case llm.TextBlock:
			parts = append(parts, map[string]any{"type": "text", "text": b.Text()})
			previousPlaceholder = b.Text() == "(image omitted: model does not support images)"
		case llm.ImageBlock:
			if !completionsSupportsImages(model) {
				if !previousPlaceholder {
					parts = append(parts, map[string]any{"type": "text", "text": "(image omitted: model does not support images)"})
				}
				previousPlaceholder = true
				continue
			}
			url := b.URL()
			if b.Source() == llm.ImageSourceData {
				url = "data:" + b.MediaType() + ";base64," + base64.StdEncoding.EncodeToString(b.Data())
			}
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": url}})
			previousPlaceholder = false
		default:
			return nil, fmt.Errorf("unsupported user block %T", block)
		}
	}
	return parts, nil
}
func completionsToolResult(blocks []llm.ToolResultContentBlock, model Model) (string, bool, error) {
	parts := make([]string, 0, len(blocks))
	images := false
	previousPlaceholder := false
	for _, block := range blocks {
		switch b := block.(type) {
		case llm.TextBlock:
			parts = append(parts, b.Text())
			previousPlaceholder = b.Text() == "(tool image omitted: model does not support images)"
		case llm.ImageBlock:
			if completionsSupportsImages(model) {
				images = true
				previousPlaceholder = false
			} else if !previousPlaceholder {
				parts = append(parts, "(tool image omitted: model does not support images)")
				previousPlaceholder = true
			}
		default:
			return "", false, fmt.Errorf("unsupported tool result block %T", block)
		}
	}
	return strings.Join(parts, "\n"), images, nil
}

type completionsFailureSpec struct {
	kind       FailureKind
	cause      error
	message    string
	httpStatus *int
	vendorCode string
	retryAfter *time.Duration
}
type completionsReasoningDetail struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Data string `json:"data"`
}

func (d completionsReasoningDetail) valid() bool {
	return d.Type == "reasoning.encrypted" && utf8.ValidString(d.ID) && strings.TrimSpace(d.ID) != "" && utf8.ValidString(d.Data) && strings.TrimSpace(d.Data) != "" && len(d.Data) <= 1<<20
}
func (d completionsReasoningDetail) signature() (string, error) {
	if !d.valid() {
		return "", fmt.Errorf("%w: invalid encrypted reasoning detail", ErrOpenAICompletionsStream)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
func decodeCompletionsReasoningDetail(signature string) (completionsReasoningDetail, bool) {
	var detail completionsReasoningDetail
	if json.Unmarshal([]byte(signature), &detail) != nil || !detail.valid() {
		return completionsReasoningDetail{}, false
	}
	return detail, true
}
func validCompletionsReasoningField(value string) bool {
	return value == "reasoning_content" || value == "reasoning" || value == "reasoning_text"
}

type completionsToolSlot struct {
	contentIndex    int
	id, name        string
	arguments       []byte
	reasoningDetail *completionsReasoningDetail
}
type openAICompletionsStream struct {
	ctx                                    context.Context
	cancel                                 context.CancelCauseFunc
	endpoint, apiKey                       string
	client                                 HTTPDoer
	clock                                  Clock
	timestamp                              time.Time
	payload                                []byte
	model                                  Model
	headers                                map[string]string
	maxEventBytes, maxErrorBodyBytes       int
	onResponse                             ResponseHook
	onHeaders                              HeaderHook
	headerOverrides                        map[string]*string
	configurationFail                      *completionsFailureSpec
	mu                                     sync.Mutex
	body                                   io.ReadCloser
	closed, finished, started, initialized bool
	closeErr                               error
	decoder                                *responsesSSEDecoder
	preflight                              *completionsFailureSpec
	queue                                  []llm.StreamEvent
	text                                   *strings.Builder
	textIndex                              int
	thinking                               *strings.Builder
	thinkingField                          string
	thinkingIndex                          int
	tools                                  map[int]*completionsToolSlot
	pendingReasoningDetails                map[string]completionsReasoningDetail
	nextContentIndex                       int
	usage                                  llm.Usage
	sawFinish                              bool
	terminalReason                         llm.FinishReason
	responseID                             string
	responseModel                          string
	rawStopReason                          string
}

func newCompletionsFailureStream(ctx context.Context, clock Clock, model Model, kind FailureKind, cause error, message string) EventStream {
	if ctx == nil {
		ctx = context.Background()
	}
	if clock == nil {
		clock = time.Now
	}
	c, cancel := context.WithCancelCause(ctx)
	return &openAICompletionsStream{ctx: c, cancel: cancel, clock: clock, timestamp: clock(), model: model, preflight: &completionsFailureSpec{kind: kind, cause: cause, message: message}, tools: make(map[int]*completionsToolSlot), pendingReasoningDetails: make(map[string]completionsReasoningDetail), textIndex: -1, thinkingIndex: -1}
}

func (s *openAICompletionsStream) Next() (llm.StreamEvent, error) {
	if s == nil || s.done() {
		return nil, io.EOF
	}
	if cause := context.Cause(s.ctx); cause != nil {
		return s.failure(s.cancelled(cause))
	}
	if len(s.queue) > 0 {
		return s.pop(), nil
	}
	if s.preflight != nil {
		f := *s.preflight
		s.preflight = nil
		return s.failure(&f)
	}
	if !s.initialized {
		s.initialized = true
		if f := s.initialize(); f != nil {
			return s.failure(f)
		}
		s.started = true
		return llm.NewStartEvent(assistantProvenanceForModel(s.model), s.timestamp)
	}
	for {
		if cause := context.Cause(s.ctx); cause != nil {
			return s.failure(s.cancelled(cause))
		}
		data, err := s.decoder.NextData()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if s.sawFinish || !completionsSupportsFinishReason(s.model) {
					return s.settle()
				}
				return s.failure(&completionsFailureSpec{kind: FailureInvalidResponse, cause: fmt.Errorf("%w: stream ended before [DONE]", ErrOpenAICompletionsStream), message: "OpenAI Chat Completions stream ended before [DONE]"})
			}
			return s.failure(&completionsFailureSpec{kind: FailureInvalidResponse, cause: fmt.Errorf("%w: read SSE: %w", ErrOpenAICompletionsStream, err), message: "OpenAI Chat Completions stream failed"})
		}
		if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			if !s.sawFinish && completionsSupportsFinishReason(s.model) {
				return s.failure(&completionsFailureSpec{kind: FailureInvalidResponse, cause: fmt.Errorf("%w: [DONE] before finish_reason", ErrOpenAICompletionsStream), message: "OpenAI Chat Completions stream ended without finish_reason"})
			}
			return s.settle()
		}
		if f := s.process(data); f != nil {
			return s.failure(f)
		}
		if len(s.queue) > 0 {
			return s.pop(), nil
		}
	}
}

func (s *openAICompletionsStream) settle() (llm.StreamEvent, error) {
	if err := s.finishBlocks(); err != nil {
		return s.failure(&completionsFailureSpec{kind: FailureInvalidResponse, cause: err, message: "OpenAI Chat Completions stream returned invalid content"})
	}
	if !s.sawFinish {
		if len(s.tools) != 0 {
			s.terminalReason = llm.FinishToolUse
		} else {
			s.terminalReason = llm.FinishStop
		}
	}
	provenance := assistantProvenanceForModel(s.model)
	var response *llm.AssistantResponseMetadata
	if s.responseID != "" || s.responseModel != "" || s.rawStopReason != "" {
		response = &llm.AssistantResponseMetadata{ResponseID: s.responseID, ResponseModel: s.responseModel, RawStopReason: s.rawStopReason}
	}
	done, err := llm.NewDoneEventWithMetadata(s.finishReason(), s.usage, s.timestamp, provenance, response, nil)
	if err != nil {
		return s.failure(&completionsFailureSpec{kind: FailureInvalidResponse, cause: err, message: "OpenAI Chat Completions stream returned invalid terminal state"})
	}
	s.queue = append(s.queue, done)
	return s.pop(), nil
}

func (s *openAICompletionsStream) initialize() *completionsFailureSpec {
	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, s.endpoint, bytes.NewReader(s.payload))
	if err != nil {
		return &completionsFailureSpec{kind: FailureInvalidRequest, cause: err, message: "Could not construct OpenAI Chat Completions request"}
	}
	if validBearerAPIKey(s.apiKey) {
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range s.headers {
		req.Header.Set(k, v)
	}
	if err := applyFinalHeaders(req.Header, s.model, s.onHeaders, s.headerOverrides); err != nil {
		return &completionsFailureSpec{kind: FailureInvalidRequest, cause: err, message: "OpenAI Chat Completions header hook failed"}
	}
	if strings.TrimSpace(req.Header.Get("Authorization")) == "" {
		if s.configurationFail != nil {
			spec := *s.configurationFail
			return &spec
		}
		return &completionsFailureSpec{
			kind:    FailureConfiguration,
			cause:   fmt.Errorf("%w: final Authorization header is missing", ErrInvalidOpenAICompletionsConfig),
			message: "OpenAI API authorization was removed before the request",
		}
	}
	resp, err := invokeResponsesHTTPDoer(s.client, req)
	if err != nil {
		if cause := context.Cause(s.ctx); cause != nil {
			return s.cancelled(cause)
		}
		return &completionsFailureSpec{kind: FailureTransport, cause: fmt.Errorf("%w: HTTP request: %w", ErrOpenAICompletionsStream, err), message: "OpenAI Chat Completions transport failed"}
	}
	if resp == nil || resp.Body == nil || isTypedNil(resp.Body) {
		return &completionsFailureSpec{kind: FailureInvalidResponse, cause: fmt.Errorf("%w: HTTP client returned nil response/body", ErrOpenAICompletionsStream), message: "OpenAI Chat Completions returned an invalid response"}
	}
	if s.onResponse != nil {
		if err := s.onResponse(s.model, responseInfo(resp)); err != nil {
			_ = resp.Body.Close()
			return &completionsFailureSpec{kind: FailureInvalidResponse, cause: fmt.Errorf("response hook: %w", err), message: "OpenAI Chat Completions response hook rejected the response"}
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return s.httpFailure(resp)
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(media, "text/event-stream") {
		_ = resp.Body.Close()
		return &completionsFailureSpec{kind: FailureInvalidResponse, cause: fmt.Errorf("%w: response content type %q is not text/event-stream", ErrOpenAICompletionsStream, resp.Header.Get("Content-Type")), message: "OpenAI Chat Completions returned a non-streaming response"}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = resp.Body.Close()
		return s.cancelled(errOpenAICompletionsStreamClosed)
	}
	s.body = resp.Body
	s.mu.Unlock()
	s.decoder = newResponsesSSEDecoder(resp.Body, s.maxEventBytes)
	s.textIndex = -1
	s.thinkingIndex = -1
	return nil
}

func (s *openAICompletionsStream) httpFailure(resp *http.Response) *completionsFailureSpec {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, int64(s.maxErrorBodyBytes)+1))
	if len(body) > s.maxErrorBodyBytes {
		body = body[:s.maxErrorBodyBytes]
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	message := fmt.Sprintf("OpenAI API request failed with HTTP status %d", resp.StatusCode)
	code := ""
	if utf8.Valid(body) && json.Unmarshal(body, &payload) == nil {
		if strings.TrimSpace(payload.Error.Message) != "" {
			message = payload.Error.Message
		}
		code = normalizeResponsesVendorCode(payload.Error.Code)
		if isOpenAIContextOverflow(resp.StatusCode, payload.Error.Type, payload.Error.Code, payload.Error.Message) {
			status := resp.StatusCode
			return &completionsFailureSpec{kind: FailureContextOverflow, cause: fmt.Errorf("%w: context window exceeded", ErrOpenAICompletionsStream), message: "OpenAI context window exceeded", httpStatus: &status, vendorCode: "context_length_exceeded"}
		}
	}
	status := resp.StatusCode
	return &completionsFailureSpec{kind: FailureHTTPStatus, cause: fmt.Errorf("OpenAI API HTTP %d: %s", resp.StatusCode, message), message: message, httpStatus: &status, vendorCode: code, retryAfter: responsesRetryAfter(resp.Header.Get("Retry-After"), s.clock())}
}

type completionsChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		FinishReason *string `json:"finish_reason"`
		Delta        struct {
			Content          *string                      `json:"content"`
			ToolCalls        []completionsToolCall        `json:"tool_calls"`
			ReasoningContent string                       `json:"reasoning_content"`
			Reasoning        string                       `json:"reasoning"`
			ReasoningText    string                       `json:"reasoning_text"`
			ReasoningDetails []completionsReasoningDetail `json:"reasoning_details"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     uint64 `json:"prompt_tokens"`
		CompletionTokens uint64 `json:"completion_tokens"`
		PromptDetails    *struct {
			CachedTokens     uint64 `json:"cached_tokens"`
			CacheWriteTokens uint64 `json:"cache_write_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionDetails *struct {
			ReasoningTokens uint64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
}

type completionsToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func (s *openAICompletionsStream) process(data []byte) *completionsFailureSpec {
	var c completionsChunk
	if err := json.Unmarshal(data, &c); err != nil {
		return &completionsFailureSpec{kind: FailureInvalidResponse, cause: fmt.Errorf("%w: decode SSE event: %w", ErrOpenAICompletionsStream, err), message: "OpenAI Chat Completions stream returned invalid JSON"}
	}
	if c.Error != nil {
		return &completionsFailureSpec{kind: FailureInvalidResponse, cause: errors.New(c.Error.Message), message: c.Error.Message}
	}
	if s.responseID == "" && c.ID != "" {
		s.responseID = c.ID
	}
	if s.responseModel == "" && c.Model != "" && c.Model != s.model.ID() {
		s.responseModel = c.Model
	}
	if c.Usage != nil {
		u, err := completionsUsage(c.Usage)
		if err != nil {
			return &completionsFailureSpec{kind: FailureInvalidResponse, cause: err, message: "OpenAI Chat Completions returned invalid usage"}
		}
		u, err = u.WithCost(s.model.CalculateCost(u))
		if err != nil {
			return &completionsFailureSpec{kind: FailureInvalidResponse, cause: err, message: "OpenAI Chat Completions returned invalid usage"}
		}
		s.usage = u
	}
	if len(c.Choices) == 0 {
		return nil
	}
	choice := c.Choices[0]
	if choice.FinishReason != nil {
		s.sawFinish = true
		s.rawStopReason = *choice.FinishReason
		reason, err := completionsFinish(*choice.FinishReason)
		if err != nil {
			return &completionsFailureSpec{kind: FailureInvalidResponse, cause: err, message: "OpenAI Chat Completions returned unsupported finish_reason"}
		}
		s.terminalReason = reason
	}
	d := choice.Delta
	if d.Content != nil && *d.Content != "" {
		if f := s.textDelta(*d.Content); f != nil {
			return f
		}
	}
	thinking, thinkingField := "", ""
	for _, candidate := range []struct{ field, value string }{{"reasoning_content", d.ReasoningContent}, {"reasoning", d.Reasoning}, {"reasoning_text", d.ReasoningText}} {
		if candidate.value != "" {
			thinking, thinkingField = candidate.value, candidate.field
			break
		}
	}
	if thinking != "" {
		if f := s.thinkingDelta(thinking, thinkingField); f != nil {
			return f
		}
	}
	for _, detail := range d.ReasoningDetails {
		if detail.Type != "reasoning.encrypted" {
			continue
		}
		if !detail.valid() {
			return &completionsFailureSpec{kind: FailureInvalidResponse, cause: fmt.Errorf("%w: invalid reasoning detail", ErrOpenAICompletionsStream), message: "OpenAI Chat Completions returned invalid reasoning detail"}
		}
		attached := false
		for _, slot := range s.tools {
			if slot.id == detail.ID {
				copy := detail
				slot.reasoningDetail = &copy
				attached = true
				break
			}
		}
		if !attached {
			s.pendingReasoningDetails[detail.ID] = detail
		}
	}
	for fallback, call := range d.ToolCalls {
		index := fallback
		if call.Index != nil {
			index = *call.Index
		}
		if f := s.toolDelta(index, call); f != nil {
			return f
		}
	}
	return nil
}

func completionsUsage(raw *struct {
	PromptTokens     uint64 `json:"prompt_tokens"`
	CompletionTokens uint64 `json:"completion_tokens"`
	PromptDetails    *struct {
		CachedTokens     uint64 `json:"cached_tokens"`
		CacheWriteTokens uint64 `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails *struct {
		ReasoningTokens uint64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}) (llm.Usage, error) {
	cacheRead, cacheWrite := uint64(0), uint64(0)
	if raw.PromptDetails != nil {
		cacheRead = raw.PromptDetails.CachedTokens
		cacheWrite = raw.PromptDetails.CacheWriteTokens
	}
	input := uint64(0)
	if cacheRead <= raw.PromptTokens && cacheWrite <= raw.PromptTokens-cacheRead {
		input = raw.PromptTokens - cacheRead - cacheWrite
	}
	spec := llm.UsageSpec{Input: input, Output: raw.CompletionTokens, CacheRead: cacheRead, CacheWrite: cacheWrite}
	if raw.CompletionDetails != nil {
		r := raw.CompletionDetails.ReasoningTokens
		spec.Reasoning = &r
	}
	return llm.NewUsage(spec)
}

func (s *openAICompletionsStream) textDelta(delta string) *completionsFailureSpec {
	if err := s.finishThinkingBlock(); err != nil {
		return &completionsFailureSpec{kind: FailureInvalidResponse, cause: err, message: "invalid reasoning stream event"}
	}
	if s.text == nil {
		s.text = &strings.Builder{}
		s.textIndex = s.nextContentIndex
		s.nextContentIndex++
		e, err := llm.NewTextStartEvent(s.textIndex)
		if err != nil {
			return &completionsFailureSpec{kind: FailureInvalidResponse, cause: err, message: "invalid text stream event"}
		}
		s.queue = append(s.queue, e)
	}
	s.text.WriteString(delta)
	e, err := llm.NewTextDeltaEvent(s.textIndex, delta)
	if err != nil {
		return &completionsFailureSpec{kind: FailureInvalidResponse, cause: err, message: "invalid text stream event"}
	}
	s.queue = append(s.queue, e)
	return nil
}
func (s *openAICompletionsStream) thinkingDelta(delta, field string) *completionsFailureSpec {
	if err := s.finishTextBlock(); err != nil {
		return &completionsFailureSpec{kind: FailureInvalidResponse, cause: err, message: "invalid text stream event"}
	}
	if s.thinking == nil {
		s.thinking = &strings.Builder{}
		s.thinkingIndex = s.nextContentIndex
		s.nextContentIndex++
		e, err := llm.NewThinkingStartEvent(s.thinkingIndex)
		if err != nil {
			return &completionsFailureSpec{kind: FailureInvalidResponse, cause: err, message: "invalid reasoning stream event"}
		}
		s.queue = append(s.queue, e)
		s.thinkingField = field
	} else if s.thinkingField != field {
		return &completionsFailureSpec{kind: FailureInvalidResponse, cause: fmt.Errorf("%w: reasoning field changed within one block", ErrOpenAICompletionsStream), message: "OpenAI Chat Completions returned inconsistent reasoning"}
	}
	s.thinking.WriteString(delta)
	e, err := llm.NewThinkingDeltaEvent(s.thinkingIndex, delta)
	if err != nil {
		return &completionsFailureSpec{kind: FailureInvalidResponse, cause: err, message: "invalid reasoning stream event"}
	}
	s.queue = append(s.queue, e)
	return nil
}
func (s *openAICompletionsStream) toolDelta(index int, call completionsToolCall) *completionsFailureSpec {
	if index < 0 {
		return &completionsFailureSpec{kind: FailureInvalidResponse, cause: fmt.Errorf("%w: negative tool call index", ErrOpenAICompletionsStream), message: "OpenAI Chat Completions returned invalid tool call"}
	}
	slot := s.tools[index]
	if slot == nil {
		if call.ID == "" || call.Function.Name == "" {
			return &completionsFailureSpec{kind: FailureInvalidResponse, cause: fmt.Errorf("%w: new tool call %d has no id/name", ErrOpenAICompletionsStream, index), message: "OpenAI Chat Completions returned invalid tool call"}
		}
		slot = &completionsToolSlot{contentIndex: s.nextContentIndex, id: call.ID, name: call.Function.Name}
		if detail, ok := s.pendingReasoningDetails[call.ID]; ok {
			copy := detail
			slot.reasoningDetail = &copy
			delete(s.pendingReasoningDetails, call.ID)
		}
		s.nextContentIndex++
		s.tools[index] = slot
		start, err := llm.NewToolCallStartEvent(slot.contentIndex, slot.id, slot.name)
		if err != nil {
			return &completionsFailureSpec{kind: FailureInvalidResponse, cause: err, message: "invalid tool stream event"}
		}
		s.queue = append(s.queue, start)
	}
	if call.ID != "" && call.ID != slot.id {
		return &completionsFailureSpec{kind: FailureInvalidResponse, cause: fmt.Errorf("%w: tool index %d changed id", ErrOpenAICompletionsStream, index), message: "OpenAI Chat Completions returned inconsistent tool call"}
	}
	if call.Function.Name != "" && call.Function.Name != slot.name {
		return &completionsFailureSpec{kind: FailureInvalidResponse, cause: fmt.Errorf("%w: tool index %d changed name", ErrOpenAICompletionsStream, index), message: "OpenAI Chat Completions returned inconsistent tool call"}
	}
	if call.Function.Arguments != "" {
		slot.arguments = append(slot.arguments, call.Function.Arguments...)
		delta, err := llm.NewToolCallDeltaEvent(slot.contentIndex, []byte(call.Function.Arguments))
		if err != nil {
			return &completionsFailureSpec{kind: FailureInvalidResponse, cause: err, message: "invalid tool stream event"}
		}
		s.queue = append(s.queue, delta)
	}
	return nil
}
func (s *openAICompletionsStream) finishBlocks() error {
	if err := s.finishThinkingBlock(); err != nil {
		return err
	}
	if err := s.finishTextBlock(); err != nil {
		return err
	}
	indices := make([]int, 0, len(s.tools))
	for index := range s.tools {
		indices = append(indices, index)
	}
	sort.Slice(indices, func(left, right int) bool {
		return s.tools[indices[left]].contentIndex < s.tools[indices[right]].contentIndex
	})
	for _, i := range indices {
		slot := s.tools[i]
		signature := ""
		if slot.reasoningDetail != nil {
			var err error
			signature, err = slot.reasoningDetail.signature()
			if err != nil {
				return err
			}
		}
		call, err := llm.NewToolCallBlockWithThoughtSignature(slot.id, slot.name, slot.arguments, signature)
		if err != nil {
			return err
		}
		e, err := llm.NewToolCallEndEvent(slot.contentIndex, call)
		if err != nil {
			return err
		}
		s.queue = append(s.queue, e)
	}
	return nil
}

func (s *openAICompletionsStream) finishThinkingBlock() error {
	if s.thinking == nil {
		return nil
	}
	b, err := llm.NewThinkingBlockWithSignature(s.thinking.String(), s.thinkingField, false)
	if err != nil {
		return err
	}
	e, err := llm.NewThinkingEndEvent(s.thinkingIndex, b)
	if err != nil {
		return err
	}
	s.queue = append(s.queue, e)
	s.thinking = nil
	s.thinkingField = ""
	s.thinkingIndex = -1
	return nil
}

func (s *openAICompletionsStream) finishTextBlock() error {
	if s.text == nil {
		return nil
	}
	e, err := llm.NewTextEndEvent(s.textIndex, s.text.String())
	if err != nil {
		return err
	}
	s.queue = append(s.queue, e)
	s.text = nil
	s.textIndex = -1
	return nil
}
func completionsFinish(value string) (llm.FinishReason, error) {
	switch value {
	case "stop", "end", "":
		return llm.FinishStop, nil
	case "length":
		return llm.FinishLength, nil
	case "tool_calls", "function_call":
		return llm.FinishToolUse, nil
	case "content_filter", "network_error":
		return 0, fmt.Errorf("%w: provider finish_reason %q", ErrOpenAICompletionsStream, value)
	default:
		return 0, fmt.Errorf("%w: unknown finish_reason %q", ErrOpenAICompletionsStream, value)
	}
}
func (s *openAICompletionsStream) finishReason() llm.FinishReason {
	return s.terminalReason
}
func (s *openAICompletionsStream) cancelled(cause error) *completionsFailureSpec {
	joined := error(ErrOpenAICompletionsAborted)
	if cause != nil {
		joined = errors.Join(joined, cause)
	}
	return &completionsFailureSpec{kind: FailureCancelled, cause: joined, message: ErrOpenAICompletionsAborted.Error()}
}
func (s *openAICompletionsStream) failure(spec *completionsFailureSpec) (llm.StreamEvent, error) {
	if spec == nil {
		return nil, io.EOF
	}
	message := spec.message
	if strings.TrimSpace(message) == "" {
		message = safeResponsesErrorText(spec.cause, "OpenAI Chat Completions request failed")
	}
	f, err := NewProviderFailure(ProviderFailureSpec{Kind: spec.kind, Message: message, Cause: spec.cause, HTTPStatus: spec.httpStatus, VendorCode: spec.vendorCode, RetryAfter: spec.retryAfter})
	if err != nil {
		return nil, closedStreamError(err)
	}
	terminal, err := llm.NewFailure(f.Error(), f)
	if err != nil {
		return nil, closedStreamError(err)
	}
	reason := llm.FinishError
	if spec.kind == FailureCancelled {
		reason = llm.FinishAborted
	}
	e, err := llm.NewErrorEventWithFailure(reason, terminal, s.usage, s.timestamp, assistantProvenanceForModel(s.model))
	if err != nil {
		return nil, closedStreamError(err)
	}
	s.finish()
	return e, nil
}
func (s *openAICompletionsStream) pop() llm.StreamEvent {
	e := s.queue[0]
	s.queue = s.queue[1:]
	if _, terminal := e.(llm.DoneEvent); terminal {
		s.finish()
	}
	return e
}
func (s *openAICompletionsStream) done() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed || s.finished
}
func (s *openAICompletionsStream) finish() {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	b := s.body
	s.body = nil
	s.mu.Unlock()
	s.cancel(errOpenAICompletionsStreamDone)
	if b != nil {
		_ = b.Close()
	}
}
func (s *openAICompletionsStream) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.closed = true
	b := s.body
	s.body = nil
	s.mu.Unlock()
	s.cancel(errOpenAICompletionsStreamClosed)
	if b != nil {
		return b.Close()
	}
	return nil
}
