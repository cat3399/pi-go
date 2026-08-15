package provider_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/klauspost/compress/zstd"
)

const providerWireUpstreamCommit = "a116523434806910336b9de3e38a41aa5860030b"

var providerWireHeaderNames = []string{
	"accept",
	"content-type",
	"content-encoding",
	"authorization",
	"x-api-key",
	"anthropic-version",
	"anthropic-beta",
	"x-session-affinity",
	"session_id",
	"session-id",
	"x-client-request-id",
	"x-session-id",
	"chatgpt-account-id",
	"openai-beta",
	"originator",
	"x-oracle",
}

type providerWireOracle struct {
	UpstreamCommit string                 `json:"upstreamCommit"`
	Scenarios      []providerWireScenario `json:"scenarios"`
}

type providerWireScenario struct {
	Name    string            `json:"name"`
	API     string            `json:"api"`
	Input   providerWireInput `json:"input"`
	Request any               `json:"request"`
	Events  any               `json:"events"`
	Result  any               `json:"result"`
}

type providerWireInput struct {
	Model        providerWireModel `json:"model"`
	SystemPrompt string            `json:"systemPrompt"`
	UserPrompt   string            `json:"userPrompt"`
	SessionID    string            `json:"sessionId"`
	Tool         providerWireTool  `json:"tool"`
	Options      providerWireOpts  `json:"options"`
}

type providerWireModel struct {
	Provider         string             `json:"provider"`
	API              string             `json:"api"`
	ID               string             `json:"id"`
	Name             string             `json:"name"`
	BaseURL          string             `json:"baseUrl"`
	Reasoning        bool               `json:"reasoning"`
	ThinkingLevelMap map[string]*string `json:"thinkingLevelMap"`
	Input            []string           `json:"input"`
	Cost             provider.CostRates `json:"cost"`
	ContextWindow    uint64             `json:"contextWindow"`
	MaxTokens        uint64             `json:"maxTokens"`
	Compat           json.RawMessage    `json:"compat"`
}

type providerWireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type providerWireOpts struct {
	Temperature          *float64          `json:"temperature"`
	MaxTokens            *uint64           `json:"maxTokens"`
	CacheRetention       string            `json:"cacheRetention"`
	ReasoningEffort      string            `json:"reasoningEffort"`
	MaxRetries           *uint32           `json:"maxRetries"`
	Headers              map[string]string `json:"headers"`
	ThinkingEnabled      *bool             `json:"thinkingEnabled"`
	ThinkingBudgetTokens *uint64           `json:"thinkingBudgetTokens"`
	ThinkingDisplay      string            `json:"thinkingDisplay"`
	InterleavedThinking  *bool             `json:"interleavedThinking"`
	AnthropicEffort      string            `json:"effort"`
	ReasoningSummary     *string           `json:"reasoningSummary"`
	ServiceTier          string            `json:"serviceTier"`
	TextVerbosity        string            `json:"textVerbosity"`
	ToolChoice           string            `json:"toolChoice"`
	Transport            string            `json:"transport"`
	Metadata             map[string]any    `json:"metadata"`
}

func TestImplementedProviderWireOraclesMatchPinnedPi(t *testing.T) {
	raw, err := os.ReadFile("testdata/upstream_provider_wire_oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var oracle providerWireOracle
	if err := json.Unmarshal(raw, &oracle); err != nil {
		t.Fatal(err)
	}
	if oracle.UpstreamCommit != providerWireUpstreamCommit {
		t.Fatalf("upstream commit = %q, want %q", oracle.UpstreamCommit, providerWireUpstreamCommit)
	}
	if len(oracle.Scenarios) != 4 {
		t.Fatalf("provider wire scenarios = %d, want 4", len(oracle.Scenarios))
	}

	seen := make(map[string]bool, len(oracle.Scenarios))
	for _, scenario := range oracle.Scenarios {
		scenario := scenario
		t.Run(scenario.Name, func(t *testing.T) {
			if seen[scenario.API] {
				t.Fatalf("duplicate API oracle %q", scenario.API)
			}
			seen[scenario.API] = true
			request, events, result := runProviderWireScenario(t, scenario)
			t.Run("request", func(t *testing.T) { assertProviderWireJSON(t, "request", request, scenario.Request) })
			t.Run("events", func(t *testing.T) { assertProviderWireJSON(t, "events", events, scenario.Events) })
			t.Run("result", func(t *testing.T) { assertProviderWireJSON(t, "result", result, scenario.Result) })
		})
	}
	for _, api := range []string{
		provider.AnthropicMessagesAPI,
		provider.OpenAICompletionsAPI,
		provider.OpenAIResponsesAPI,
		provider.OpenAICodexResponsesAPI,
	} {
		if !seen[api] {
			t.Errorf("missing provider wire oracle for %q", api)
		}
	}
}

func runProviderWireScenario(t *testing.T, scenario providerWireScenario) (map[string]any, []map[string]any, map[string]any) {
	t.Helper()
	model := newProviderWireModel(t, scenario.Input.Model)
	tool, err := provider.NewToolDefinition(
		scenario.Input.Tool.Name,
		scenario.Input.Tool.Description,
		false,
		scenario.Input.Tool.Parameters,
	)
	if err != nil {
		t.Fatal(err)
	}

	key := providerWireAPIKey(scenario.API)
	var capturedHeaders http.Header
	var capturedURL, capturedMethod string
	var capturedPayload []byte
	client := responsesDoerFunc(func(request *http.Request) (*http.Response, error) {
		capturedURL = request.URL.String()
		capturedMethod = request.Method
		capturedHeaders = request.Header.Clone()
		var sentBody []byte
		if request.Body != nil {
			sentBody, err = io.ReadAll(request.Body)
			_ = request.Body.Close()
			if err != nil {
				return nil, err
			}
		}
		if request.Header.Get("Content-Encoding") == "zstd" {
			decoder, err := zstd.NewReader(nil)
			if err != nil {
				return nil, err
			}
			sentBody, err = decoder.DecodeAll(sentBody, nil)
			decoder.Close()
			if err != nil {
				return nil, err
			}
		}
		if !bytes.Equal(sentBody, capturedPayload) {
			return nil, fmt.Errorf("HTTP body differs from payload hook: got %q, want %q", sentBody, capturedPayload)
		}
		return responsesHTTPResponse(http.StatusOK, "text/event-stream", providerWireResponseSSE(t, scenario.API)), nil
	})
	implementation := newProviderWireImplementation(t, scenario.API, scenario.Input.Model.BaseURL, key, client)

	streamOptions := provider.StreamOptions{
		Temperature:          scenario.Input.Options.Temperature,
		APIKey:               key,
		Headers:              scenario.Input.Options.Headers,
		MaxTokens:            scenario.Input.Options.MaxTokens,
		SessionID:            scenario.Input.SessionID,
		Transport:            provider.Transport(scenario.Input.Options.Transport),
		CacheRetention:       provider.CacheRetention(scenario.Input.Options.CacheRetention),
		MaxRetries:           scenario.Input.Options.MaxRetries,
		ReasoningEffort:      scenario.Input.Options.ReasoningEffort,
		ReasoningSummary:     scenario.Input.Options.ReasoningSummary,
		ServiceTier:          scenario.Input.Options.ServiceTier,
		TextVerbosity:        scenario.Input.Options.TextVerbosity,
		ThinkingEnabled:      scenario.Input.Options.ThinkingEnabled,
		ThinkingBudgetTokens: scenario.Input.Options.ThinkingBudgetTokens,
		ThinkingDisplay:      scenario.Input.Options.ThinkingDisplay,
		InterleavedThinking:  scenario.Input.Options.InterleavedThinking,
		AnthropicEffort:      scenario.Input.Options.AnthropicEffort,
		Metadata:             scenario.Input.Options.Metadata,
		OnPayload: func(_ provider.Model, payload []byte) ([]byte, error) {
			capturedPayload = bytes.Clone(payload)
			return payload, nil
		},
	}
	choiceMode := scenario.Input.Options.ToolChoice
	if scenario.API == provider.AnthropicMessagesAPI && choiceMode == "any" {
		choiceMode = "required"
	}
	var choice *provider.ToolChoice
	if choiceMode != "" {
		choice = &provider.ToolChoice{Mode: choiceMode}
	}
	message, err := llm.NewUserTextMessage(scenario.Input.UserPrompt, time.UnixMilli(0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequestWithOptions(
		model,
		scenario.Input.SystemPrompt,
		[]llm.ConversationMessage{message},
		provider.RequestOptions{Tools: []provider.ToolDefinition{tool}, ToolChoice: choice, Stream: streamOptions},
	)
	if err != nil {
		t.Fatal(err)
	}
	events, terminal := collectStream(t, implementation.Stream(context.Background(), request))
	if len(capturedPayload) == 0 || capturedHeaders == nil {
		t.Fatal("provider did not expose a payload and issue an HTTP request")
	}
	var body any
	if err := json.Unmarshal(capturedPayload, &body); err != nil {
		t.Fatalf("decode captured provider payload: %v", err)
	}
	wireRequest := map[string]any{
		"url":     capturedURL,
		"method":  capturedMethod,
		"headers": selectProviderWireHeaders(capturedHeaders),
		"body":    body,
	}
	return wireRequest, normalizeProviderWireEvents(t, events), normalizeProviderWireTerminal(t, terminal)
}

func newProviderWireModel(t *testing.T, input providerWireModel) provider.Model {
	t.Helper()
	thinking := make(map[provider.ThinkingLevel]*string, len(input.ThinkingLevelMap))
	for level, value := range input.ThinkingLevelMap {
		thinking[provider.ThinkingLevel(level)] = value
	}
	inputKinds := make([]provider.InputKind, len(input.Input))
	for index, kind := range input.Input {
		inputKinds[index] = provider.InputKind(kind)
	}
	compat := provider.ModelCompat{}
	switch input.API {
	case provider.AnthropicMessagesAPI:
		var value provider.AnthropicMessagesCompat
		if err := json.Unmarshal(input.Compat, &value); err != nil {
			t.Fatal(err)
		}
		compat.AnthropicMessages = &value
	case provider.OpenAICompletionsAPI:
		var value provider.OpenAICompletionsCompat
		if err := json.Unmarshal(input.Compat, &value); err != nil {
			t.Fatal(err)
		}
		compat.OpenAICompletions = &value
	case provider.OpenAIResponsesAPI, provider.OpenAICodexResponsesAPI:
		var value provider.OpenAIResponsesCompat
		if err := json.Unmarshal(input.Compat, &value); err != nil {
			t.Fatal(err)
		}
		compat.OpenAIResponses = &value
	default:
		t.Fatalf("unsupported provider wire API %q", input.API)
	}
	model, err := provider.NewModel(provider.ModelSpec{
		Provider: input.Provider, API: input.API, ID: input.ID, Name: input.Name, BaseURL: input.BaseURL,
		Reasoning: input.Reasoning, ThinkingLevelMap: thinking, Input: inputKinds, Cost: input.Cost,
		ContextWindow: input.ContextWindow, MaxTokens: input.MaxTokens, Compat: compat,
	})
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func newProviderWireImplementation(t *testing.T, api, baseURL, key string, client provider.HTTPDoer) provider.Provider {
	t.Helper()
	clock := func() time.Time { return time.Date(2026, time.August, 16, 0, 0, 0, 0, time.UTC) }
	var (
		implementation provider.Provider
		err            error
	)
	switch api {
	case provider.AnthropicMessagesAPI:
		implementation, err = provider.NewAnthropicProvider(provider.AnthropicConfig{BaseURL: baseURL, APIKey: key, Client: client, Clock: clock})
	case provider.OpenAICompletionsAPI:
		implementation, err = provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{BaseURL: baseURL, APIKey: key, Client: client, Clock: clock})
	case provider.OpenAIResponsesAPI:
		implementation, err = provider.NewOpenAIResponsesProvider(provider.OpenAIResponsesConfig{BaseURL: baseURL, APIKey: key, Client: client, Clock: clock})
	case provider.OpenAICodexResponsesAPI:
		implementation, err = provider.NewOpenAICodexResponsesProvider(provider.OpenAICodexResponsesConfig{BaseURL: baseURL, AccessToken: key, Client: client, Clock: clock})
	default:
		t.Fatalf("unsupported provider wire API %q", api)
	}
	if err != nil {
		t.Fatal(err)
	}
	return implementation
}

func providerWireAPIKey(api string) string {
	if api == provider.OpenAICodexResponsesAPI {
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
		payload := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-wire"}}`))
		return header + "." + payload + ".signature"
	}
	if api == provider.AnthropicMessagesAPI {
		return "oracle-anthropic-key"
	}
	return "oracle-openai-key"
}

func selectProviderWireHeaders(headers http.Header) map[string]any {
	selected := make(map[string]any, len(providerWireHeaderNames))
	for _, name := range providerWireHeaderNames {
		if value := headers.Get(name); value != "" {
			selected[name] = value
		} else {
			selected[name] = nil
		}
	}
	return selected
}

func normalizeProviderWireEvents(t *testing.T, events []llm.StreamEvent) []map[string]any {
	t.Helper()
	result := make([]map[string]any, 0, len(events))
	for _, event := range events {
		normalized := map[string]any{"type": eventKind(event)}
		switch value := event.(type) {
		case llm.TextStartEvent:
			normalized["contentIndex"] = value.ContentIndex()
		case llm.TextDeltaEvent:
			normalized["contentIndex"], normalized["delta"] = value.ContentIndex(), value.Delta()
		case llm.TextEndEvent:
			normalized["contentIndex"], normalized["content"] = value.ContentIndex(), value.Content()
		case llm.ThinkingStartEvent:
			normalized["contentIndex"] = value.ContentIndex()
		case llm.ThinkingDeltaEvent:
			normalized["contentIndex"], normalized["delta"] = value.ContentIndex(), value.Delta()
		case llm.ThinkingEndEvent:
			block := value.Content()
			normalized["contentIndex"], normalized["content"] = value.ContentIndex(), block.Thinking()
			signature, _ := block.ThinkingSignature()
			normalized["signature"], normalized["redacted"] = normalizeProviderWireSignature(signature), block.Redacted()
		case llm.ToolCallStartEvent:
			normalized["contentIndex"], normalized["id"], normalized["name"] = value.ContentIndex(), value.ID(), value.Name()
		case llm.ToolCallDeltaEvent:
			normalized["contentIndex"], normalized["delta"] = value.ContentIndex(), string(value.Delta())
		case llm.ToolCallEndEvent:
			normalized["contentIndex"] = value.ContentIndex()
			normalized["toolCall"] = normalizeProviderWireBlock(t, value.ToolCall())
		case llm.DoneEvent:
			normalized["reason"] = value.Reason().String()
		case llm.ErrorEvent:
			normalized["reason"] = value.Reason().String()
		}
		result = append(result, normalized)
	}
	return result
}

func normalizeProviderWireTerminal(t *testing.T, terminal llm.AssistantTerminal) map[string]any {
	t.Helper()
	provenance := terminal.AssistantProvenance()
	blocks := make([]map[string]any, 0, len(terminal.Blocks()))
	for _, block := range terminal.Blocks() {
		blocks = append(blocks, normalizeProviderWireBlock(t, block))
	}
	result := map[string]any{
		"role":       "assistant",
		"api":        provenance.API,
		"provider":   provenance.Provider,
		"model":      provenance.Model,
		"content":    blocks,
		"usage":      normalizeProviderWireUsage(terminal.Usage()),
		"stopReason": terminal.FinishReason().String(),
	}
	if metadata, ok := terminal.ResponseMetadata(); ok {
		if metadata.ResponseID != "" {
			result["responseId"] = metadata.ResponseID
		}
		if metadata.ResponseModel != "" {
			result["responseModel"] = metadata.ResponseModel
		}
		if metadata.RawStopReason != "" {
			result["rawStopReason"] = metadata.RawStopReason
		}
	}
	if failure, ok := terminal.(llm.AssistantFailureMessage); ok {
		result["errorMessage"] = failure.ErrorMessage()
	}
	return result
}

func normalizeProviderWireBlock(t *testing.T, block llm.AssistantBlock) map[string]any {
	t.Helper()
	switch value := block.(type) {
	case llm.TextBlock:
		signature, _ := value.TextSignature()
		return map[string]any{"type": "text", "text": value.Text(), "signature": normalizeProviderWireSignature(signature)}
	case llm.ThinkingBlock:
		signature, _ := value.ThinkingSignature()
		return map[string]any{
			"type": "thinking", "thinking": value.Thinking(),
			"signature": normalizeProviderWireSignature(signature), "redacted": value.Redacted(),
		}
	case llm.ToolCallBlock:
		var arguments any
		if err := json.Unmarshal(value.ArgumentsJSON(), &arguments); err != nil {
			t.Fatalf("decode tool arguments: %v", err)
		}
		signature, _ := value.ThoughtSignature()
		return map[string]any{
			"type": "toolCall", "id": value.ID(), "name": value.Name(), "arguments": arguments,
			"thoughtSignature": normalizeProviderWireSignature(signature),
		}
	default:
		t.Fatalf("unsupported provider wire block %T", block)
		return nil
	}
}

func normalizeProviderWireSignature(signature string) any {
	if signature == "" {
		return nil
	}
	var decoded any
	if json.Unmarshal([]byte(signature), &decoded) == nil {
		return decoded
	}
	return signature
}

func normalizeProviderWireUsage(usage llm.Usage) map[string]any {
	result := map[string]any{
		"input": usage.Input(), "output": usage.Output(), "cacheRead": usage.CacheRead(),
		"cacheWrite": usage.CacheWrite(), "totalTokens": usage.TotalTokens(),
	}
	if reasoning, ok := usage.Reasoning(); ok {
		result["reasoning"] = reasoning
	}
	if cacheWrite1h, ok := usage.CacheWrite1h(); ok {
		result["cacheWrite1h"] = cacheWrite1h
	}
	return result
}

func assertProviderWireJSON(t *testing.T, label string, got, want any) {
	t.Helper()
	gotJSON, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("%s differs from pinned pi\n--- got ---\n%s\n--- want ---\n%s", label, gotJSON, wantJSON)
	}
}

func providerWireResponseSSE(t *testing.T, api string) string {
	t.Helper()
	switch api {
	case provider.AnthropicMessagesAPI:
		return providerWireAnthropicSSE(t)
	case provider.OpenAICompletionsAPI:
		return providerWireCompletionsSSE(t)
	case provider.OpenAIResponsesAPI:
		return providerWireResponsesSSE(t, "responses")
	case provider.OpenAICodexResponsesAPI:
		return providerWireResponsesSSE(t, "codex")
	default:
		t.Fatalf("unsupported provider wire API %q", api)
		return ""
	}
}

func providerWireAnthropicSSE(t *testing.T) string {
	t.Helper()
	type event struct {
		name string
		data any
	}
	events := []event{
		{"message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_anthropic_wire", "usage": map[string]any{"input_tokens": 12, "output_tokens": 0, "cache_read_input_tokens": 2, "cache_creation_input_tokens": 1, "cache_creation": map[string]any{"ephemeral_1h_input_tokens": 1}}}}},
		{"content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}}},
		{"content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": "plan"}}},
		{"content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "signature_delta", "signature": "anthropic-signature"}}},
		{"content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}},
		{"content_block_start", map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "text", "text": ""}}},
		{"content_block_delta", map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "text_delta", "text": "answer "}}},
		{"content_block_stop", map[string]any{"type": "content_block_stop", "index": 1}},
		{"content_block_start", map[string]any{"type": "content_block_start", "index": 2, "content_block": map[string]any{"type": "tool_use", "id": "call_1", "name": "lookup", "input": map[string]any{}}}},
		{"content_block_delta", map[string]any{"type": "content_block_delta", "index": 2, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"query":"pi-go"}`}}},
		{"content_block_stop", map[string]any{"type": "content_block_stop", "index": 2}},
		{"message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"input_tokens": 12, "output_tokens": 8, "cache_read_input_tokens": 2, "cache_creation_input_tokens": 1, "cache_creation": map[string]any{"ephemeral_1h_input_tokens": 1}, "output_tokens_details": map[string]any{"thinking_tokens": 3}}}},
		{"message_stop", map[string]any{"type": "message_stop"}},
	}
	var output strings.Builder
	for _, item := range events {
		encoded, err := json.Marshal(item.data)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&output, "event: %s\ndata: %s\n\n", item.name, encoded)
	}
	return output.String()
}

func providerWireCompletionsSSE(t *testing.T) string {
	t.Helper()
	chunks := []string{
		`{"id":"chatcmpl-wire","model":"gpt-wire-chat-actual","choices":[{"index":0,"delta":{"reasoning_content":"plan"},"finish_reason":null}]}`,
		`{"id":"chatcmpl-wire","model":"gpt-wire-chat-actual","choices":[{"index":0,"delta":{"content":"answer "},"finish_reason":null}]}`,
		`{"id":"chatcmpl-wire","model":"gpt-wire-chat-actual","choices":[{"index":0,"delta":{"reasoning_details":[{"type":"reasoning.encrypted","id":"call_1","data":"completion-signature"}]},"finish_reason":null}]}`,
		`{"id":"chatcmpl-wire","model":"gpt-wire-chat-actual","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"query\":"}}]},"finish_reason":null}]}`,
		`{"id":"chatcmpl-wire","model":"gpt-wire-chat-actual","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"pi-go\"}"}}]},"finish_reason":null}]}`,
		`{"id":"chatcmpl-wire","model":"gpt-wire-chat-actual","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"chatcmpl-wire","model":"gpt-wire-chat-actual","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":8,"total_tokens":28,"prompt_tokens_details":{"cached_tokens":3,"cache_write_tokens":2},"completion_tokens_details":{"reasoning_tokens":3}}}`,
	}
	var output strings.Builder
	for _, chunk := range chunks {
		if !json.Valid([]byte(chunk)) {
			t.Fatalf("invalid completions fixture %q", chunk)
		}
		fmt.Fprintf(&output, "data: %s\n\n", chunk)
	}
	output.WriteString("data: [DONE]\n\n")
	return output.String()
}

func providerWireResponsesSSE(t *testing.T, prefix string) string {
	t.Helper()
	responseID := "resp_" + prefix + "_wire"
	reasoningID := "rs_" + prefix + "_wire"
	reasoningItem := map[string]any{"type": "reasoning", "id": reasoningID, "summary": []any{map[string]any{"type": "summary_text", "text": "plan"}}, "encrypted_content": prefix + "-encrypted"}
	events := []any{
		map[string]any{"type": "response.created", "sequence_number": 0, "response": map[string]any{"id": responseID}},
		map[string]any{"type": "response.output_item.added", "sequence_number": 1, "output_index": 0, "item": map[string]any{"type": "reasoning", "id": reasoningID, "summary": []any{}}},
		map[string]any{"type": "response.reasoning_summary_text.delta", "sequence_number": 2, "output_index": 0, "summary_index": 0, "delta": "plan"},
		map[string]any{"type": "response.output_item.done", "sequence_number": 3, "output_index": 0, "item": reasoningItem},
		map[string]any{"type": "response.output_item.added", "sequence_number": 4, "output_index": 1, "item": map[string]any{"type": "message", "id": "msg_" + prefix + "_wire", "role": "assistant", "status": "in_progress", "content": []any{}}},
		map[string]any{"type": "response.output_text.delta", "sequence_number": 5, "output_index": 1, "content_index": 0, "item_id": "msg_" + prefix + "_wire", "delta": "answer "},
		map[string]any{"type": "response.output_item.done", "sequence_number": 6, "output_index": 1, "item": map[string]any{"type": "message", "id": "msg_" + prefix + "_wire", "role": "assistant", "status": "completed", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": "answer ", "annotations": []any{}}}}},
		map[string]any{"type": "response.output_item.added", "sequence_number": 7, "output_index": 2, "item": map[string]any{"type": "function_call", "id": "fc_" + prefix + "_wire", "call_id": "call_1", "name": "lookup", "arguments": ""}},
		map[string]any{"type": "response.function_call_arguments.delta", "sequence_number": 8, "output_index": 2, "item_id": "fc_" + prefix + "_wire", "delta": `{"query":"pi-go"}`},
		map[string]any{"type": "response.output_item.done", "sequence_number": 9, "output_index": 2, "item": map[string]any{"type": "function_call", "id": "fc_" + prefix + "_wire", "call_id": "call_1", "name": "lookup", "arguments": `{"query":"pi-go"}`}},
		map[string]any{"type": "response.completed", "sequence_number": 10, "response": map[string]any{"id": responseID, "status": "completed", "service_tier": map[bool]string{true: "flex", false: "priority"}[prefix == "codex"], "output": []any{reasoningItem}, "usage": map[string]any{"input_tokens": 20, "output_tokens": 8, "total_tokens": 28, "input_tokens_details": map[string]any{"cached_tokens": 3, "cache_write_tokens": 2}, "output_tokens_details": map[string]any{"reasoning_tokens": 3}}}},
	}
	return responsesSSE(events...) + "data: [DONE]\n\n"
}

func TestProviderWireOracleGeneratorMetadataIsStable(t *testing.T) {
	raw, err := os.ReadFile("testdata/upstream_provider_wire_oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	generator, ok := document["generator"].(map[string]any)
	if !ok || generator["corpus"] != "upstream_provider_wire_corpus.json" {
		t.Fatalf("generator metadata = %#v", document["generator"])
	}
	if got := document["generatedBy"]; got != "pinned packages/ai provider adapters with deterministic HTTP/SSE transport" {
		t.Fatalf("generatedBy = %#v", got)
	}
	if reflect.ValueOf(document["scenarios"]).Len() != 4 {
		t.Fatalf("scenario metadata = %#v", document["scenarios"])
	}
}
