package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/cat3399/pi-go/internal/llm"
)

type responsesRequestPayload struct {
	Model           string                     `json:"model"`
	Input           []any                      `json:"input"`
	Tools           []any                      `json:"tools,omitempty"`
	ToolChoice      any                        `json:"tool_choice,omitempty"`
	Stream          bool                       `json:"stream"`
	Store           bool                       `json:"store"`
	Reasoning       *responsesReasoningOptions `json:"reasoning,omitempty"`
	Include         []string                   `json:"include,omitempty"`
	MaxOutputTokens uint64                     `json:"max_output_tokens,omitempty"`
	Temperature     *float64                   `json:"temperature,omitempty"`
	ServiceTier     string                     `json:"service_tier,omitempty"`
	PromptCacheKey  string                     `json:"prompt_cache_key,omitempty"`
	PromptCacheTTL  string                     `json:"prompt_cache_retention,omitempty"`
	PromptCacheMode *responsesPromptCacheMode  `json:"prompt_cache_options,omitempty"`
}

type responsesPromptCacheMode struct {
	Mode string `json:"mode"`
}

type responsesReasoningOptions struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary,omitempty"`
}

type responsesFunctionTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict,omitempty"`
}

type responsesCustomTool struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Format      struct {
		Type       string `json:"type"`
		Syntax     string `json:"syntax"`
		Definition string `json:"definition"`
	} `json:"format"`
}

type responsesEasyMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type responsesInputText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type responsesInputImage struct {
	Type     string `json:"type"`
	Detail   string `json:"detail"`
	ImageURL string `json:"image_url"`
}
type responsesOutputText struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

type responsesOutputMessage struct {
	Type    string                `json:"type"`
	Role    string                `json:"role"`
	Content []responsesOutputText `json:"content"`
	Status  string                `json:"status"`
	ID      string                `json:"id"`
	Phase   string                `json:"phase,omitempty"`
}

func encodeOpenAIResponsesRequest(request Request, systemRole string) ([]byte, error) {
	messages, err := transformConversationMessages(request.Messages())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOpenAIResponsesRequest, err)
	}
	input := make([]any, 0, len(messages)+1)
	if request.SystemPrompt() != "" {
		input = append(input, responsesEasyMessage{
			Role:    systemRole,
			Content: request.SystemPrompt(),
		})
	}
	grammarProperties, err := responsesGrammarToolProperties(request)
	if err != nil {
		return nil, err
	}
	_, deferredTools := splitDeferredTools(request, responsesSupportsToolSearch(request.Model()))
	loadedDeferred := map[string]struct{}{}
	wireMessageIndex := 0
	for sourceIndex, message := range messages {
		switch message := message.(type) {
		case llm.UserTextMessage:
			blocks := message.Content()
			if len(blocks) == 0 {
				continue
			}
			content := make([]responsesInputText, len(blocks))
			for index, block := range blocks {
				content[index] = responsesInputText{Type: "input_text", Text: block.Text()}
			}
			input = append(input, responsesEasyMessage{Role: "user", Content: content})
			wireMessageIndex++
		case llm.UserContentMessage:
			content, err := responsesInputContent(message.Content(), request.Model())
			if err != nil {
				return nil, fmt.Errorf("%w: message %d: %w", ErrOpenAIResponsesRequest, sourceIndex, err)
			}
			if len(content) != 0 {
				input = append(input, responsesEasyMessage{Role: "user", Content: content})
				wireMessageIndex++
			}

		case llm.AssistantTextMessage:
			blocks := message.Content()
			if len(blocks) == 0 {
				continue
			}
			policy := responsesReplayPolicyFor(message, request.ReplayTarget())
			input = appendResponsesAssistantText(input, wireMessageIndex, blocks, policy.sameModel)
			wireMessageIndex++

		case llm.AssistantFailureMessage:
			// Failed and aborted assistant turns may retain partial text for the
			// transcript, but that text was never a completed model response and
			// must not be acknowledged as one on the next request.
			continue

		case llm.AssistantToolUseMessage:
			encoded, err := appendResponsesAssistantToolUse(input, wireMessageIndex, message, responsesReplayPolicyFor(message, request.ReplayTarget()), grammarProperties)
			if err != nil {
				return nil, fmt.Errorf("%w: message %d: %w", ErrOpenAIResponsesRequest, sourceIndex, err)
			}
			input = encoded
			wireMessageIndex++
		case llm.AssistantRichMessage:
			encoded, err := appendResponsesAssistantBlocks(input, wireMessageIndex, message.Blocks(), responsesReplayPolicyFor(message, request.ReplayTarget()), grammarProperties)
			if err != nil {
				return nil, fmt.Errorf("%w: message %d: %w", ErrOpenAIResponsesRequest, sourceIndex, err)
			}
			input = encoded
			wireMessageIndex++

		case llm.ToolResultMessage:
			output, err := responsesToolResultOutput(message)
			if err != nil {
				return nil, fmt.Errorf("%w: message %d: %w", ErrOpenAIResponsesRequest, sourceIndex, err)
			}
			callID, _ := splitResponsesToolID(message.ToolCallID())
			outputType := "function_call_output"
			if _, custom := grammarProperties[message.ToolName()]; custom {
				outputType = "custom_tool_call_output"
			}
			input = append(input, responsesFunctionCallOutput{Type: outputType, CallID: callID, Output: output})
			input, err = appendResponsesDeferredTools(input, message.ToolCallID(), message.AddedToolNames(), deferredTools, loadedDeferred, request.Model())
			if err != nil {
				return nil, err
			}
			wireMessageIndex++
		case llm.ToolResultContentMessage:
			output, err := responsesToolResultContentOutput(message.Content(), request.Model())
			if err != nil {
				return nil, fmt.Errorf("%w: message %d: %w", ErrOpenAIResponsesRequest, sourceIndex, err)
			}
			callID, _ := splitResponsesToolID(message.ToolCallID())
			outputType := "function_call_output"
			if _, custom := grammarProperties[message.ToolName()]; custom {
				outputType = "custom_tool_call_output"
			}
			input = append(input, responsesFunctionCallOutput{Type: outputType, CallID: callID, Output: output})
			input, err = appendResponsesDeferredTools(input, message.ToolCallID(), message.AddedToolNames(), deferredTools, loadedDeferred, request.Model())
			if err != nil {
				return nil, err
			}
			wireMessageIndex++

		default:
			return nil, fmt.Errorf(
				"%w: message %d has unsupported type %T",
				ErrOpenAIResponsesRequest,
				sourceIndex,
				message,
			)
		}
	}
	immediateTools, _ := splitDeferredTools(request, responsesSupportsToolSearch(request.Model()))
	tools, err := encodeResponsesTools(immediateTools, request.Model())
	if err != nil {
		return nil, err
	}
	payloadValue := responsesRequestPayload{
		Model:  request.Model().ID(),
		Input:  input,
		Tools:  tools,
		Stream: true,
		Store:  false,
	}
	options := request.StreamOptions()
	payloadValue.Temperature = options.Temperature
	payloadValue.ServiceTier = options.ServiceTier
	cacheRetention := resolveOpenAICacheRetention(options)
	if cacheRetention != CacheRetentionNone {
		payloadValue.PromptCacheKey = clampOpenAIPromptCacheKey(options.SessionID)
	}
	compat := request.Model().Compat().OpenAIResponses
	if cacheRetention == CacheRetentionLong && (compat == nil || compat.SupportsLongCacheRetention == nil || *compat.SupportsLongCacheRetention) {
		payloadValue.PromptCacheTTL = "24h"
	}
	if cacheRetention == CacheRetentionNone && compat != nil && compat.SupportsExplicitPromptCacheMode != nil && *compat.SupportsExplicitPromptCacheMode {
		payloadValue.PromptCacheMode = &responsesPromptCacheMode{Mode: "explicit"}
	}
	if choice, ok := request.ToolChoice(); ok {
		if choice.Name != "" {
			payloadValue.ToolChoice = map[string]string{"type": "function", "name": choice.Name}
		} else if choice.Mode != "" {
			payloadValue.ToolChoice = choice.Mode
		}
	}
	if request.Model().Reasoning() {
		effort, explicit := responsesReasoningEffort(request)
		hasSummary := options.ReasoningSummary != nil && *options.ReasoningSummary != ""
		if explicit || hasSummary {
			if effort == "" {
				effort = "medium"
			}
			payloadValue.Reasoning = &responsesReasoningOptions{Effort: effort, Summary: "auto"}
			if options.ReasoningSummary != nil && *options.ReasoningSummary != "" {
				payloadValue.Reasoning.Summary = *options.ReasoningSummary
			}
			payloadValue.Include = []string{"reasoning.encrypted_content"}
		} else if request.Model().Provider() != "github-copilot" {
			if off, enabled := request.Model().ThinkingEffort(ThinkingOff); enabled {
				payloadValue.Reasoning = &responsesReasoningOptions{Effort: off}
			}
		}
	}
	maxTokens := request.Model().MaxTokens()
	if configured := request.StreamOptions().MaxTokens; configured != nil {
		maxTokens = *configured
	}
	if maxTokens != 0 {
		if maxTokens < 16 {
			maxTokens = 16
		}
		payloadValue.MaxOutputTokens = maxTokens
	}
	payload, err := json.Marshal(payloadValue)
	if err != nil {
		return nil, fmt.Errorf("%w: encode JSON: %w", ErrOpenAIResponsesRequest, err)
	}
	return payload, nil
}

func responsesReasoningEffort(request Request) (string, bool) {
	if configured := request.StreamOptions().ReasoningEffort; configured != "" {
		if mapped, present := request.Model().ThinkingLevelMap()[ThinkingLevel(configured)]; present && mapped != nil {
			return *mapped, true
		}
		return configured, true
	}
	if request.ThinkingLevel() == "" || request.ThinkingLevel() == ThinkingOff {
		return "", false
	}
	return request.Model().ThinkingEffort(request.ThinkingLevel())
}

func responsesSupportsToolSearch(model Model) bool {
	compat := model.Compat().OpenAIResponses
	return compat != nil && compat.SupportsToolSearch != nil && *compat.SupportsToolSearch
}

func appendResponsesDeferredTools(input []any, sourceID string, names []string, deferred map[string]ToolDefinition, loaded map[string]struct{}, model Model) ([]any, error) {
	if len(deferred) == 0 || len(names) == 0 {
		return input, nil
	}
	tools := make([]ToolDefinition, 0, len(names))
	for _, name := range names {
		tool, ok := deferred[name]
		if !ok {
			continue
		}
		if _, done := loaded[name]; done {
			continue
		}
		loaded[name] = struct{}{}
		tools = append(tools, tool)
	}
	if len(tools) == 0 {
		return input, nil
	}
	converted, err := encodeResponsesTools(tools, model)
	if err != nil {
		return nil, err
	}
	loadedNames := make([]string, len(tools))
	for index, tool := range tools {
		loadedNames[index] = tool.Name()
	}
	callID := "pi_tool_load_" + responsesShortHash(sourceID+":"+strings.Join(loadedNames, ","))
	call := responsesToolSearchCall{Type: "tool_search_call", CallID: callID, Execution: "client", Status: "completed"}
	call.Arguments.Query, call.Arguments.Limit = strings.Join(loadedNames, " "), len(loadedNames)
	return append(input, call, responsesToolSearchOutput{Type: "tool_search_output", CallID: callID, Execution: "client", Status: "completed", Tools: converted}), nil
}

type responsesFunctionCall struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type responsesCustomToolCall struct {
	Type   string `json:"type"`
	ID     string `json:"id,omitempty"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Input  string `json:"input"`
}

type responsesFunctionCallOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output any    `json:"output"`
}
type responsesToolSearchCall struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Execution string `json:"execution"`
	Status    string `json:"status"`
	Arguments struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	} `json:"arguments"`
}
type responsesToolSearchOutput struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Execution string `json:"execution"`
	Status    string `json:"status"`
	Tools     []any  `json:"tools"`
}

type responsesReplayPolicy struct{ sourced, sameDialect, sameModel bool }
type responsesProvenanceCarrier interface {
	AssistantProvenance() llm.AssistantProvenance
}

func responsesReplayPolicyFor(message responsesProvenanceCarrier, target llm.AssistantProvenance) responsesReplayPolicy {
	source := message.AssistantProvenance()
	sameDialect := source.Provider == target.Provider && source.API == target.API
	return responsesReplayPolicy{
		sourced:     true,
		sameDialect: sameDialect,
		sameModel:   source.Matches(target.Provider, target.API, target.Model),
	}
}

func appendResponsesAssistantToolUse(input []any, messageIndex int, message llm.AssistantToolUseMessage, policy responsesReplayPolicy, grammarProperties map[string]string) ([]any, error) {
	return appendResponsesAssistantBlocks(input, messageIndex, message.Blocks(), policy, grammarProperties)
}
func appendResponsesAssistantBlocks(input []any, messageIndex int, blocks []llm.AssistantBlock, policy responsesReplayPolicy, grammarProperties map[string]string) ([]any, error) {
	textBlockIndex := 0
	seenCallIDs := make(map[string]struct{})
	for _, block := range blocks {
		switch block := block.(type) {
		case llm.TextBlock:
			input = appendResponsesAssistantTextAt(input, messageIndex, textBlockIndex, block, policy.sameModel)
			textBlockIndex++
		case llm.ThinkingBlock:
			signature, hasSignature := block.ThinkingSignature()
			reasoning, validSignature := decodeResponsesReasoningSignature(signature)
			if !hasSignature || !validSignature || !policy.sameModel {
				if block.Redacted() || strings.TrimSpace(block.Thinking()) == "" {
					continue
				}
				text, err := llm.NewTextBlock(block.Thinking())
				if err != nil {
					return nil, err
				}
				input = appendResponsesAssistantTextAt(input, messageIndex, textBlockIndex, text, false)
				textBlockIndex++
				continue
			}
			input = append(input, reasoning)
		case llm.ToolCallBlock:
			callID, itemID := splitResponsesToolID(block.ID())
			if _, duplicate := seenCallIDs[callID]; duplicate {
				return nil, fmt.Errorf("duplicate normalized tool call ID %q", callID)
			}
			seenCallIDs[callID] = struct{}{}
			property, custom := grammarProperties[block.Name()]
			itemID = responsesReplayToolItemID(itemID, policy, custom)
			if custom {
				var arguments map[string]any
				if err := json.Unmarshal(block.ArgumentsJSON(), &arguments); err != nil {
					return nil, err
				}
				value, ok := arguments[property].(string)
				if !ok {
					return nil, fmt.Errorf("grammar tool call %q requires argument %q to be a string", block.Name(), property)
				}
				input = append(input, responsesCustomToolCall{Type: "custom_tool_call", ID: itemID, CallID: callID, Name: block.Name(), Input: value})
			} else {
				input = append(input, responsesFunctionCall{
					Type:      "function_call",
					ID:        itemID,
					CallID:    callID,
					Name:      block.Name(),
					Arguments: string(block.ArgumentsJSON()),
				})
			}
		default:
			return nil, fmt.Errorf("unsupported assistant block %T", block)
		}
	}
	return input, nil
}

func responsesReplayToolItemID(itemID string, policy responsesReplayPolicy, custom bool) string {
	if itemID == "" {
		return ""
	}
	if policy.sameModel {
		// Custom-tool item ids use the ctc_* namespace and are valid only while
		// replaying the exact model that produced them. Function-call admission
		// must not rewrite those native ids into an fc_* shape.
		if custom {
			return itemID
		}
		return normalizeResponsesFunctionItemID(itemID)
	}
	if policy.sourced && policy.sameDialect {
		return ""
	}
	return "fc_" + responsesShortHash(itemID)
}

func responsesInputContent(blocks []llm.UserContentBlock, model Model) ([]any, error) {
	content := make([]any, 0, len(blocks))
	supportsImages := responsesSupportsImages(model)
	previousPlaceholder := false
	for _, block := range blocks {
		switch block := block.(type) {
		case llm.TextBlock:
			content = append(content, responsesInputText{Type: "input_text", Text: block.Text()})
			previousPlaceholder = block.Text() == "(image omitted: model does not support images)"
		case llm.ImageBlock:
			if !supportsImages {
				if !previousPlaceholder {
					content = append(content, responsesInputText{Type: "input_text", Text: "(image omitted: model does not support images)"})
				}
				previousPlaceholder = true
				continue
			}
			content = append(content, responsesImageInput(block))
			previousPlaceholder = false
		default:
			return nil, fmt.Errorf("unsupported user block %T", block)
		}
	}
	return content, nil
}

func responsesSupportsImages(model Model) bool {
	for _, kind := range model.Input() {
		if kind == InputImage {
			return true
		}
	}
	return false
}

func responsesImageInput(image llm.ImageBlock) responsesInputImage {
	url := image.URL()
	if image.Source() == llm.ImageSourceData {
		url = "data:" + image.MediaType() + ";base64," + base64.StdEncoding.EncodeToString(image.Data())
	}
	return responsesInputImage{Type: "input_image", Detail: "auto", ImageURL: url}
}

func responsesToolResultOutput(message llm.ToolResultMessage) (any, error) {
	blocks := message.Content()
	parts := make([]string, len(blocks))
	for index, block := range blocks {
		parts[index] = block.Text()
	}
	output := strings.Join(parts, "\n")
	// OpenAI's function_call_output cannot distinguish no block from an empty
	// text block. Both map to the upstream-compatible explicit placeholder.
	if output == "" {
		output = "(no tool output)"
	}
	return output, nil
}
func responsesToolResultContentOutput(blocks []llm.ToolResultContentBlock, model Model) (any, error) {
	if !responsesSupportsImages(model) {
		texts := make([]string, 0, len(blocks))
		previousPlaceholder := false
		for _, block := range blocks {
			switch block := block.(type) {
			case llm.TextBlock:
				texts = append(texts, block.Text())
				previousPlaceholder = block.Text() == "(tool image omitted: model does not support images)"
			case llm.ImageBlock:
				if !previousPlaceholder {
					texts = append(texts, "(tool image omitted: model does not support images)")
				}
				previousPlaceholder = true
			default:
				return nil, fmt.Errorf("unsupported tool result block %T", block)
			}
		}
		output := strings.Join(texts, "\n")
		if output == "" {
			output = "(no tool output)"
		}
		return output, nil
	}
	texts := make([]string, 0, len(blocks))
	images := make([]llm.ImageBlock, 0)
	for _, block := range blocks {
		switch block := block.(type) {
		case llm.TextBlock:
			texts = append(texts, block.Text())
		case llm.ImageBlock:
			images = append(images, block)
		default:
			return nil, fmt.Errorf("unsupported tool result block %T", block)
		}
	}
	text := strings.Join(texts, "\n")
	if len(images) == 0 {
		if text == "" {
			text = "(no tool output)"
		}
		return text, nil
	}
	out := make([]any, 0, len(images)+1)
	if text != "" {
		out = append(out, responsesInputText{Type: "input_text", Text: text})
	}
	for _, image := range images {
		out = append(out, responsesImageInput(image))
	}
	return out, nil
}

func encodeResponsesTools(definitions []ToolDefinition, model Model) ([]any, error) {
	tools := make([]any, 0, len(definitions))
	compat := model.Compat().OpenAIResponses
	supportsStrict := compat != nil && compat.SupportsStrictMode != nil && *compat.SupportsStrictMode
	supportsGrammar := compat != nil && compat.SupportsOpenAIGrammarTools != nil && *compat.SupportsOpenAIGrammarTools
	for index, definition := range definitions {
		if err := definition.validate(); err != nil {
			return nil, fmt.Errorf("%w: tool %d: %w", ErrOpenAIResponsesRequest, index, err)
		}
		if grammar, ok, err := definition.ResolveGrammar(supportsGrammar); err != nil {
			return nil, fmt.Errorf("%w: tool %d: %w", ErrOpenAIResponsesRequest, index, err)
		} else if ok {
			tool := responsesCustomTool{Type: "custom", Name: definition.Name(), Description: definition.Description()}
			tool.Format.Type, tool.Format.Syntax, tool.Format.Definition = "grammar", grammar.Syntax, grammar.Definition
			tools = append(tools, tool)
			continue
		}
		tool := responsesFunctionTool{
			Type:        "function",
			Name:        definition.Name(),
			Description: definition.Description(),
			Parameters:  json.RawMessage(definition.ParametersJSON()),
		}
		if strict, include, err := definition.ResolveJSONSchemaStrict(supportsStrict); err != nil {
			return nil, fmt.Errorf("%w: tool %d: %w", ErrOpenAIResponsesRequest, index, err)
		} else if include {
			tool.Strict = &strict
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

func responsesGrammarToolProperties(request Request) (map[string]string, error) {
	compat := request.Model().Compat().OpenAIResponses
	supported := compat != nil && compat.SupportsOpenAIGrammarTools != nil && *compat.SupportsOpenAIGrammarTools
	properties := make(map[string]string)
	for index, tool := range request.Tools() {
		grammar, ok, err := tool.ResolveGrammar(supported)
		if err != nil {
			return nil, fmt.Errorf("%w: tool %d: %w", ErrOpenAIResponsesRequest, index, err)
		}
		if ok {
			properties[tool.Name()] = grammar.InputProperty
		}
	}
	return properties, nil
}

func resolveOpenAICacheRetention(options StreamOptions) CacheRetention {
	if options.CacheRetention != "" {
		return options.CacheRetention
	}
	if value := options.Env["PI_CACHE_RETENTION"]; value != "" {
		if value == "long" {
			return CacheRetentionLong
		}
		return CacheRetentionShort
	}
	// PI_CACHE_RETENTION is the only ambient process variable consumed here;
	// the complete process environment is never projected into request hooks.
	if os.Getenv("PI_CACHE_RETENTION") == "long" {
		return CacheRetentionLong
	}
	return CacheRetentionShort
}

func clampOpenAIPromptCacheKey(value string) string {
	runes := []rune(value)
	if len(runes) > 64 {
		runes = runes[:64]
	}
	return string(runes)
}

func splitResponsesToolID(value string) (callID, itemID string) {
	callID, itemID, _ = strings.Cut(value, "|")
	return normalizeResponsesCallID(callID), itemID
}

func normalizeResponsesCallID(value string) string {
	value = normalizeResponsesIDPart(value)
	if value == "" {
		return "call_pi"
	}
	return value
}

func normalizeResponsesFunctionItemID(value string) string {
	if value == "" {
		return ""
	}
	normalized := normalizeResponsesIDPart(value)
	if strings.HasPrefix(normalized, "fc_") {
		return normalized
	}
	// Same-model provenance proves the item is native, but a function_call may
	// only replay an fc_* item id. In particular, omit ctc_* custom-tool ids
	// when the matching grammar tool is no longer present instead of fabricating
	// a new pairing identity.
	return ""
}

// responsesShortHash mirrors pi's JavaScript shortHash, including its UTF-16
// code-unit iteration and uint32 overflow semantics. Responses exposes these
// generated IDs on the wire, so matching the production implementation keeps
// cross-runtime replay stable.
func responsesShortHash(value string) string {
	h1 := uint32(0xdeadbeef)
	h2 := uint32(0x41c6ce57)
	for _, codeUnit := range utf16.Encode([]rune(value)) {
		character := uint32(codeUnit)
		h1 = (h1 ^ character) * uint32(2654435761)
		h2 = (h2 ^ character) * uint32(1597334677)
	}
	h1 = ((h1 ^ (h1 >> 16)) * uint32(2246822507)) ^ ((h2 ^ (h2 >> 13)) * uint32(3266489909))
	h2 = ((h2 ^ (h2 >> 16)) * uint32(2246822507)) ^ ((h1 ^ (h1 >> 13)) * uint32(3266489909))
	return strconv.FormatUint(uint64(h2), 36) + strconv.FormatUint(uint64(h1), 36)
}

func normalizeResponsesIDPart(value string) string {
	var result strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-' {
			result.WriteRune(character)
		} else {
			result.WriteByte('_')
		}
	}
	normalized := result.String()
	if len(normalized) > 64 {
		normalized = normalized[:64]
	}
	return strings.TrimRight(normalized, "_")
}

func appendResponsesAssistantText(
	input []any,
	messageIndex int,
	blocks []llm.TextBlock,
	allowReplay bool,
) []any {
	for blockIndex, block := range blocks {
		input = appendResponsesAssistantTextAt(input, messageIndex, blockIndex, block, allowReplay)
	}
	return input
}
func appendResponsesAssistantTextAt(input []any, messageIndex, blockIndex int, block llm.TextBlock, allowReplay bool) []any {
	id := fmt.Sprintf("msg_pi_%d", messageIndex)
	if blockIndex != 0 {
		id = fmt.Sprintf("msg_pi_%d_%d", messageIndex, blockIndex)
	}
	message := responsesOutputMessage{
		Type: "message",
		Role: "assistant",
		Content: []responsesOutputText{{
			Type:        "output_text",
			Text:        block.Text(),
			Annotations: []any{},
		}},
		Status: "completed",
		ID:     id,
	}
	if signature, ok := block.TextSignature(); ok && allowReplay {
		if replay, valid := decodeResponsesTextSignature(signature); valid {
			message.ID = normalizeResponsesMessageItemID(replay.ID)
			message.Phase = replay.Phase
		}
	}
	return append(input, message)
}

func normalizeResponsesMessageItemID(value string) string {
	if len(utf16.Encode([]rune(value))) <= 64 {
		return value
	}
	return "msg_" + responsesShortHash(value)
}
