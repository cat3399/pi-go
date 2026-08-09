package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	OpenAICodexProviderID     = "openai-codex"
	OpenAICodexResponsesAPI   = "openai-codex-responses"
	defaultOpenAICodexBaseURL = "https://chatgpt.com/backend-api"
)

var (
	ErrInvalidOpenAICodexConfig = errors.New("invalid OpenAI Codex configuration")
	ErrOpenAICodexRequest       = errors.New("invalid OpenAI Codex request")
	ErrOpenAICodexUnsupported   = errors.New("unsupported OpenAI Codex behavior")
)

type OpenAICodexResponsesConfig struct {
	BaseURL           string
	AccessToken       string
	AccountID         string
	Headers           map[string]string
	Client            HTTPDoer
	Clock             Clock
	MaxEventBytes     int
	MaxErrorBodyBytes int
}

type OpenAICodexResponsesProvider struct {
	endpoint          string
	accessToken       string
	accountID         string
	headers           map[string]string
	client            HTTPDoer
	clock             Clock
	maxEventBytes     int
	maxErrorBodyBytes int
	configurationFail *responsesFailureSpec
}

func NewOpenAICodexResponsesProvider(config OpenAICodexResponsesConfig) (*OpenAICodexResponsesProvider, error) {
	baseURL := config.BaseURL
	if baseURL == "" {
		baseURL = defaultOpenAICodexBaseURL
	}
	endpoint, err := codexResponsesEndpoint(baseURL)
	if err != nil {
		return nil, err
	}
	if config.MaxEventBytes < 0 || config.MaxErrorBodyBytes < 0 {
		return nil, fmt.Errorf("%w: byte limits cannot be negative", ErrInvalidOpenAICodexConfig)
	}
	client := config.Client
	if client == nil {
		client = http.DefaultClient
	} else if isTypedNil(client) {
		return nil, fmt.Errorf("%w: HTTP client is a typed nil", ErrInvalidOpenAICodexConfig)
	}
	clock := config.Clock
	if clock == nil {
		clock = time.Now
	}
	provider := &OpenAICodexResponsesProvider{endpoint: endpoint, accessToken: config.AccessToken, accountID: config.AccountID, headers: cloneStrings(config.Headers), client: client, clock: synchronizedClock(clock), maxEventBytes: config.MaxEventBytes, maxErrorBodyBytes: config.MaxErrorBodyBytes}
	if provider.maxEventBytes == 0 {
		provider.maxEventBytes = defaultResponsesEventBytes
	}
	if provider.maxErrorBodyBytes == 0 {
		provider.maxErrorBodyBytes = defaultResponsesErrorBytes
	}
	if !validBearerAPIKey(provider.accessToken) {
		provider.configurationFail = &responsesFailureSpec{kind: FailureConfiguration, cause: fmt.Errorf("%w: access token must be non-empty valid UTF-8", ErrInvalidOpenAICodexConfig), message: "OpenAI Codex OAuth is not configured"}
	}
	if provider.accountID != "" && !validCodexAccountID(provider.accountID) {
		return nil, fmt.Errorf("%w: account ID is invalid", ErrInvalidOpenAICodexConfig)
	}
	return provider, nil
}

func codexResponsesEndpoint(base string) (string, error) {
	if !utf8.ValidString(base) || strings.TrimSpace(base) == "" {
		return "", fmt.Errorf("%w: base URL must be non-empty valid UTF-8", ErrInvalidOpenAICodexConfig)
	}
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", fmt.Errorf("%w: base URL must be an absolute HTTP(S) URL without user info, query, or fragment", ErrInvalidOpenAICodexConfig)
	}
	path := strings.TrimRight(parsed.Path, "/")
	switch {
	case strings.HasSuffix(path, "/codex/responses"):
	case strings.HasSuffix(path, "/codex"):
		path += "/responses"
	default:
		path += "/codex/responses"
	}
	parsed.Path, parsed.RawPath = path, ""
	return parsed.String(), nil
}

func (*OpenAICodexResponsesProvider) SupportsModel(model Model) bool {
	return model.API() == OpenAICodexResponsesAPI
}

func (p *OpenAICodexResponsesProvider) Stream(ctx context.Context, request Request) EventStream {
	clock := Clock(time.Now)
	if p != nil && p.clock != nil {
		clock = p.clock
	}
	if ctx == nil {
		return newResponsesFailureStream(context.Background(), clock, request.Model(), FailureInvalidRequest, fmt.Errorf("%w: nil context", ErrInvalidRequest), "OpenAI Codex request requires a context")
	}
	if p == nil {
		return newResponsesFailureStream(ctx, clock, request.Model(), FailureConfiguration, fmt.Errorf("%w: nil provider", ErrInvalidOpenAICodexConfig), "OpenAI Codex provider is not configured")
	}
	if err := request.validate(); err != nil {
		return newResponsesFailureStream(ctx, clock, request.Model(), FailureInvalidRequest, err, "")
	}
	if !p.SupportsModel(request.Model()) {
		return newResponsesFailureStream(ctx, clock, request.Model(), FailureConfiguration, fmt.Errorf("%w: model routes to provider %q API %q", ErrOpenAICodexRequest, request.Model().Provider(), request.Model().API()), "")
	}
	options := request.StreamOptions()
	token := requestAPIKey(request, p.accessToken)
	if !validBearerAPIKey(token) {
		return newResponsesFailureStream(ctx, clock, request.Model(), FailureConfiguration, fmt.Errorf("%w: OAuth access token is missing", ErrInvalidOpenAICodexConfig), "OpenAI Codex OAuth is not configured")
	}
	accountID := p.accountID
	if accountID == "" {
		var err error
		accountID, err = extractCodexAccountID(token)
		if err != nil {
			return newResponsesFailureStream(ctx, clock, request.Model(), FailureConfiguration, err, "OpenAI Codex account ID could not be resolved")
		}
	}
	payload, err := encodeOpenAICodexRequest(request)
	if err != nil {
		return newResponsesFailureStream(ctx, clock, request.Model(), FailureInvalidRequest, err, "")
	}
	if payload, err = applyPayloadHook(options.OnPayload, request.Model(), payload); err != nil {
		return newResponsesFailureStream(ctx, clock, request.Model(), FailureInvalidRequest, err, "")
	}
	grammarProperties, err := responsesGrammarToolProperties(request)
	if err != nil {
		return newResponsesFailureStream(ctx, clock, request.Model(), FailureInvalidRequest, err, "")
	}
	endpoint := p.endpoint
	if request.Model().BaseURL() != "" {
		endpoint, err = codexResponsesEndpoint(request.Model().BaseURL())
		if err != nil {
			return newResponsesFailureStream(ctx, clock, request.Model(), FailureInvalidRequest, err, "")
		}
	}
	headers := mergeResponseHeaders(request.Model().Headers(), p.headers, options.Headers)
	// Codex identity headers are authoritative in pi and are applied after
	// model/request headers. HeaderOverrides and the final header hook can still
	// deliberately remove them at the extension boundary.
	headers["Authorization"] = "Bearer " + token
	headers["chatgpt-account-id"] = accountID
	headers["originator"] = "pi"
	headers["User-Agent"] = fmt.Sprintf("pi (%s %s; %s)", runtime.GOOS, runtime.Version(), runtime.GOARCH)
	headers["OpenAI-Beta"] = "responses=experimental"
	if options.SessionID != "" {
		headers["session-id"] = options.SessionID
		headers["x-client-request-id"] = options.SessionID
	}
	streamConfig := openAICodexStreamConfig{
		ctx: ctx, endpoint: endpoint, token: token, accountID: accountID, payload: payload, model: request.Model(), headers: headers,
		client: p.client, clock: clock, maxEventBytes: p.maxEventBytes, maxErrorBodyBytes: p.maxErrorBodyBytes,
		configurationFail: p.configurationFail, grammarProperties: grammarProperties, options: options,
	}
	if options.Fetch != nil {
		streamConfig.client = options.Fetch
	}
	if options.Transport == TransportSSE {
		return streamConfig.newSSEStream()
	}
	return newOpenAICodexHybridStream(streamConfig)
}

func encodeOpenAICodexRequest(request Request) ([]byte, error) {
	standard, err := encodeOpenAIResponsesRequest(request, "system")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOpenAICodexRequest, err)
	}
	var body map[string]any
	if err := json.Unmarshal(standard, &body); err != nil {
		return nil, fmt.Errorf("%w: decode shared request: %v", ErrOpenAICodexRequest, err)
	}
	input, _ := body["input"].([]any)
	if len(input) != 0 {
		if first, ok := input[0].(map[string]any); ok && (first["role"] == "system" || first["role"] == "developer") {
			input = input[1:]
		}
	}
	body["input"] = input
	instructions := request.SystemPrompt()
	if instructions == "" {
		instructions = "You are a helpful assistant."
	}
	body["instructions"] = instructions
	verbosity := request.StreamOptions().TextVerbosity
	if verbosity == "" {
		verbosity = "low"
	}
	body["text"] = map[string]any{"verbosity": verbosity}
	body["include"] = []string{"reasoning.encrypted_content"}
	body["parallel_tool_calls"] = true
	if _, configured := body["tool_choice"]; !configured {
		body["tool_choice"] = "auto"
	}
	cacheRetention := resolveOpenAICacheRetention(request.StreamOptions())
	if cacheRetention == CacheRetentionNone || request.StreamOptions().SessionID == "" {
		delete(body, "prompt_cache_key")
	} else {
		body["prompt_cache_key"] = clampOpenAIPromptCacheKey(request.StreamOptions().SessionID)
	}
	delete(body, "prompt_cache_retention")
	delete(body, "prompt_cache_options")
	delete(body, "max_output_tokens")
	effort := request.StreamOptions().ReasoningEffort
	if effort == "" && request.ThinkingLevel() != "" && request.ThinkingLevel() != ThinkingOff {
		if mapped, enabled := request.Model().ThinkingEffort(request.ThinkingLevel()); enabled {
			effort = mapped
		}
	}
	if effort == "" {
		delete(body, "reasoning")
	} else {
		if effort == "none" {
			if mapped, configured := request.Model().ThinkingLevelMap()[ThinkingOff]; configured && mapped != nil {
				effort = *mapped
			}
		} else if mapped, configured := request.Model().ThinkingLevelMap()[ThinkingLevel(effort)]; configured && mapped != nil {
			effort = *mapped
		}
		body["reasoning"] = map[string]any{"effort": effort}
	}
	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		summary := "auto"
		if request.StreamOptions().ReasoningSummary != nil {
			summary = *request.StreamOptions().ReasoningSummary
		}
		reasoning["summary"] = summary
	}
	normalizeCodexToolStrict(body)
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%w: encode JSON: %v", ErrOpenAICodexRequest, err)
	}
	return encoded, nil
}

func normalizeCodexToolStrict(body map[string]any) {
	normalize := func(tools []any) {
		for _, item := range tools {
			tool, ok := item.(map[string]any)
			if !ok || tool["type"] != "function" {
				continue
			}
			if strict, present := tool["strict"]; present && strict == false {
				tool["strict"] = nil
			}
		}
	}
	if tools, ok := body["tools"].([]any); ok {
		normalize(tools)
	}
	if input, ok := body["input"].([]any); ok {
		for _, item := range input {
			output, ok := item.(map[string]any)
			if !ok || output["type"] != "tool_search_output" {
				continue
			}
			if tools, ok := output["tools"].([]any); ok {
				normalize(tools)
			}
		}
	}
}

func extractCodexAccountID(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("%w: failed to extract account ID from OAuth token", ErrInvalidOpenAICodexConfig)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if padded := parts[1] + strings.Repeat("=", (4-len(parts[1])%4)%4); padded != parts[1] {
			payload, err = base64.URLEncoding.DecodeString(padded)
		}
	}
	if err != nil {
		return "", fmt.Errorf("%w: failed to extract account ID from OAuth token", ErrInvalidOpenAICodexConfig)
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return "", fmt.Errorf("%w: failed to extract account ID from OAuth token", ErrInvalidOpenAICodexConfig)
	}
	authClaim, _ := claims["https://api.openai.com/auth"].(map[string]any)
	accountID, _ := authClaim["chatgpt_account_id"].(string)
	if !validCodexAccountID(accountID) {
		return "", fmt.Errorf("%w: failed to extract account ID from OAuth token", ErrInvalidOpenAICodexConfig)
	}
	return accountID, nil
}

func validCodexAccountID(value string) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && len(value) <= 512 && !strings.ContainsFunc(value, unicode.IsControl)
}
