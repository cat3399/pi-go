package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	OpenAIResponsesAPI         = "openai-responses"
	OpenAIProviderID           = "openai"
	defaultOpenAIResponsesBase = "https://api.openai.com/v1"
	defaultResponsesEventBytes = 1 << 20
	defaultResponsesErrorBytes = 64 << 10
)

var (
	ErrInvalidOpenAIResponsesConfig  = errors.New("invalid OpenAI Responses configuration")
	ErrOpenAIResponsesRequest        = errors.New("invalid OpenAI Responses request")
	ErrOpenAIResponsesStream         = errors.New("invalid OpenAI Responses stream")
	ErrOpenAIResponsesAborted        = errors.New("OpenAI Responses request aborted")
	ErrOpenAIResponsesUnsupported    = errors.New("unsupported OpenAI Responses behavior")
	errOpenAIResponsesStreamClosed   = errors.New("OpenAI Responses stream closed")
	errOpenAIResponsesStreamFinished = errors.New("OpenAI Responses stream finished")
)

// HTTPDoer is the transport seam used by the production adapter. A normal
// *http.Client satisfies it; deterministic tests can inject a local transport.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type OpenAIResponsesConfig struct {
	// BaseURL is the API root before /responses. Empty selects the standard
	// OpenAI https://api.openai.com/v1 endpoint.
	BaseURL string
	// APIKey is the already-resolved bearer credential. A missing or malformed
	// credential is a FailureConfiguration stream, not a constructor panic/error.
	APIKey string
	// Headers are adapter/provider-level headers. Model headers are applied
	// first and request headers last, so a request can override either.
	Headers map[string]string
	Client  HTTPDoer
	Clock   Clock
	// SystemRole selects the explicit Responses role for a non-empty system
	// prompt. Zero selects "system"; reasoning-model assembly may select
	// OpenAIResponsesDeveloperRole without guessing from a model ID.
	SystemRole OpenAIResponsesSystemRole

	// Zero selects bounded production defaults. Negative values are invalid.
	MaxEventBytes     int
	MaxErrorBodyBytes int
}

// OpenAIResponsesProvider implements the standard OpenAI Responses text
// dialect. Tool calls, thinking/reasoning replay, images, retries, and prompt
// cache policy remain explicit later milestones.
type OpenAIResponsesProvider struct {
	endpoint          string
	apiKey            string
	headers           map[string]string
	client            HTTPDoer
	clock             Clock
	systemRole        OpenAIResponsesSystemRole
	maxEventBytes     int
	maxErrorBodyBytes int
	configurationFail *responsesFailureSpec
}

type OpenAIResponsesSystemRole uint8

const (
	OpenAIResponsesSystemRoleDefault OpenAIResponsesSystemRole = iota
	OpenAIResponsesSystemRoleSystem
	OpenAIResponsesSystemRoleDeveloper
)

func (r OpenAIResponsesSystemRole) wireValue() (string, error) {
	switch r {
	case OpenAIResponsesSystemRoleDefault, OpenAIResponsesSystemRoleSystem:
		return "system", nil
	case OpenAIResponsesSystemRoleDeveloper:
		return "developer", nil
	default:
		return "", fmt.Errorf("%w: unknown system role %d", ErrInvalidOpenAIResponsesConfig, r)
	}
}

func NewOpenAIResponsesProvider(config OpenAIResponsesConfig) (*OpenAIResponsesProvider, error) {
	baseURL := config.BaseURL
	if baseURL == "" {
		baseURL = defaultOpenAIResponsesBase
	}
	endpoint, err := responsesEndpoint(baseURL)
	if err != nil {
		return nil, err
	}
	var configurationFail *responsesFailureSpec
	if !utf8.ValidString(config.APIKey) || strings.TrimSpace(config.APIKey) == "" {
		cause := fmt.Errorf("%w: API key must be non-empty valid UTF-8", ErrInvalidOpenAIResponsesConfig)
		configurationFail = &responsesFailureSpec{
			kind: FailureConfiguration, cause: cause, message: "OpenAI API key is not configured",
		}
	} else if strings.ContainsFunc(config.APIKey, unicode.IsControl) {
		cause := fmt.Errorf("%w: API key contains a control character", ErrInvalidOpenAIResponsesConfig)
		configurationFail = &responsesFailureSpec{
			kind: FailureConfiguration, cause: cause, message: "OpenAI API key is invalid",
		}
	}
	if config.MaxEventBytes < 0 || config.MaxErrorBodyBytes < 0 {
		return nil, fmt.Errorf("%w: byte limits cannot be negative", ErrInvalidOpenAIResponsesConfig)
	}
	if _, err := config.SystemRole.wireValue(); err != nil {
		return nil, err
	}
	client := config.Client
	if client == nil {
		client = http.DefaultClient
	} else if isTypedNil(client) {
		return nil, fmt.Errorf("%w: HTTP client is a typed nil", ErrInvalidOpenAIResponsesConfig)
	}
	clock := config.Clock
	if clock == nil {
		clock = time.Now
	}
	maxEventBytes := config.MaxEventBytes
	if maxEventBytes == 0 {
		maxEventBytes = defaultResponsesEventBytes
	}
	maxErrorBodyBytes := config.MaxErrorBodyBytes
	if maxErrorBodyBytes == 0 {
		maxErrorBodyBytes = defaultResponsesErrorBytes
	}
	return &OpenAIResponsesProvider{
		endpoint:          endpoint,
		apiKey:            config.APIKey,
		headers:           cloneStrings(config.Headers),
		client:            client,
		clock:             synchronizedClock(clock),
		systemRole:        config.SystemRole,
		maxEventBytes:     maxEventBytes,
		maxErrorBodyBytes: maxErrorBodyBytes,
		configurationFail: configurationFail,
	}, nil
}

func responsesEndpoint(rawBaseURL string) (string, error) {
	if !utf8.ValidString(rawBaseURL) || strings.TrimSpace(rawBaseURL) == "" {
		return "", fmt.Errorf("%w: base URL must be non-empty valid UTF-8", ErrInvalidOpenAIResponsesConfig)
	}
	parsed, err := url.Parse(rawBaseURL)
	if err != nil {
		return "", fmt.Errorf("%w: parse base URL: %v", ErrInvalidOpenAIResponsesConfig, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("%w: base URL must be an absolute HTTP(S) URL", ErrInvalidOpenAIResponsesConfig)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", fmt.Errorf("%w: base URL cannot contain user info, query, or fragment", ErrInvalidOpenAIResponsesConfig)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/responses"
	parsed.RawPath = ""
	return parsed.String(), nil
}

func (p *OpenAIResponsesProvider) Stream(ctx context.Context, request Request) EventStream {
	clock := Clock(time.Now)
	if p != nil && p.clock != nil {
		clock = p.clock
	}
	if ctx == nil {
		return newResponsesFailureStream(
			context.Background(),
			clock,
			request.Model(),
			FailureInvalidRequest,
			fmt.Errorf("%w: nil context", ErrInvalidRequest),
			"OpenAI Responses request requires a context",
		)
	}
	if p == nil {
		return newResponsesFailureStream(
			ctx,
			clock,
			request.Model(),
			FailureConfiguration,
			fmt.Errorf("%w: nil provider", ErrInvalidOpenAIResponsesConfig),
			"OpenAI Responses provider is not configured",
		)
	}
	if err := request.validate(); err != nil {
		return newResponsesFailureStream(ctx, clock, request.Model(), FailureInvalidRequest, err, "")
	}
	if request.Model().API() != OpenAIResponsesAPI {
		cause := fmt.Errorf(
			"%w: model routes to provider %q API %q",
			ErrOpenAIResponsesRequest,
			request.Model().Provider(),
			request.Model().API(),
		)
		return newResponsesFailureStream(ctx, clock, request.Model(), FailureConfiguration, cause, "")
	}
	systemRole, err := p.systemRole.wireValue()
	if err != nil {
		return newResponsesFailureStream(ctx, clock, request.Model(), FailureConfiguration, err, "")
	}
	if compat := request.Model().Compat().OpenAIResponses; compat != nil && compat.SupportsDeveloperRole != nil && !*compat.SupportsDeveloperRole {
		systemRole = "system"
	}
	payload, err := encodeOpenAIResponsesRequest(request, systemRole)
	if err != nil {
		return newResponsesFailureStream(ctx, clock, request.Model(), FailureInvalidRequest, err, "")
	}
	options := request.StreamOptions()
	if payload, err = applyPayloadHook(options.OnPayload, request.Model(), payload); err != nil {
		return newResponsesFailureStream(ctx, clock, request.Model(), FailureInvalidRequest, err, "")
	}
	endpoint := p.endpoint
	if baseURL := request.Model().BaseURL(); baseURL != "" {
		endpoint, err = responsesEndpoint(baseURL)
		if err != nil {
			return newResponsesFailureStream(ctx, clock, request.Model(), FailureInvalidRequest, err, "")
		}
	}
	streamContext, cancel := context.WithCancelCause(ctx)
	headers := mergeResponseHeaders(request.Model().Headers(), p.headers, options.Headers)
	if sessionID := options.SessionID; sessionID != "" && options.CacheRetention != CacheRetentionNone {
		format := "openai"
		if compat := request.Model().Compat().OpenAIResponses; compat != nil && compat.SessionAffinityFormat != nil {
			format = *compat.SessionAffinityFormat
		}
		switch format {
		case "openrouter":
			headers["x-session-id"] = sessionID
		case "openai-nosession":
			headers["x-client-request-id"] = sessionID
		default:
			headers["session_id"] = sessionID
			headers["x-client-request-id"] = sessionID
		}
	}
	client := p.client
	if options.Fetch != nil {
		client = options.Fetch
	}
	return &openAIResponsesStream{
		ctx:               streamContext,
		cancel:            cancel,
		endpoint:          endpoint,
		apiKey:            requestAPIKey(request, p.apiKey),
		client:            client,
		clock:             clock,
		timestamp:         clock(),
		payload:           payload,
		model:             request.Model(),
		headers:           headers,
		maxEventBytes:     p.maxEventBytes,
		maxErrorBodyBytes: p.maxErrorBodyBytes,
		onResponse:        options.OnResponse,
		onHeaders:         options.OnHeaders,
		headerOverrides:   cloneHeaderOverrides(options.HeaderOverrides),
		configurationFail: p.configurationFail,
		slots:             make(map[int]*responsesTextSlot),
		reasoningSlots:    make(map[int]*responsesReasoningSlot),
		toolSlots:         make(map[int]*responsesToolSlot),
		completedOutputs:  make(map[int]struct{}),
		completedItemIDs:  make(map[int]string),
		completedPhases:   make(map[int]string),
		pendingReasoning:  make(map[int]*responsesCompletedReasoning),
	}
}

func (*OpenAIResponsesProvider) SupportsModel(model Model) bool {
	return model.API() == OpenAIResponsesAPI
}

func requestAPIKey(request Request, fallback string) string {
	if key := request.StreamOptions().APIKey; key != "" {
		return key
	}
	return fallback
}

func validBearerAPIKey(value string) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && !strings.ContainsFunc(value, unicode.IsControl)
}
func mergeResponseHeaders(groups ...map[string]string) map[string]string {
	merged := map[string]string{}
	for _, group := range groups {
		for key, value := range group {
			for existing := range merged {
				if strings.EqualFold(existing, key) {
					delete(merged, existing)
				}
			}
			merged[key] = value
		}
	}
	return merged
}

func applyPayloadHook(hook PayloadHook, model Model, payload []byte) ([]byte, error) {
	if hook == nil {
		return payload, nil
	}
	result, err := hook(model, append([]byte(nil), payload...))
	if err != nil {
		return nil, fmt.Errorf("payload hook: %w", err)
	}
	if !json.Valid(result) {
		return nil, fmt.Errorf("payload hook returned invalid JSON")
	}
	return append([]byte(nil), result...), nil
}

func applyFinalHeaders(headers http.Header, model Model, hook HeaderHook, overrides map[string]*string) error {
	// HeaderOverrides is the Go representation of the original
	// Record<string, string | null> request option. Apply it while assembling
	// the request so before_provider_headers remains the final transform and
	// can observe, restore, or replace a static deletion.
	for name, value := range overrides {
		headers.Del(name)
		if value != nil {
			headers.Set(name, *value)
		}
	}
	if hook != nil {
		values := make(map[string]*string, len(headers))
		for name := range headers {
			value := headers.Get(name)
			copy := value
			values[name] = &copy
		}
		if err := hook(model, values); err != nil {
			return fmt.Errorf("header hook: %w", err)
		}
		for name := range headers {
			headers.Del(name)
		}
		for name, value := range values {
			if value != nil {
				headers.Set(name, *value)
			}
		}
	}
	return nil
}

func responseInfo(response *http.Response) ResponseInfo {
	info := ResponseInfo{StatusCode: response.StatusCode, Headers: make(map[string][]string, len(response.Header))}
	for key, values := range response.Header {
		info.Headers[key] = append([]string(nil), values...)
	}
	return info
}

func isTypedNil(value any) bool {
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// responsesRetryAfter accepts the two RFC-defined forms. Invalid or past
// values are intentionally omitted: retry policy must not turn malformed
// remote input into an unbounded wait.
func responsesRetryAfter(value string, now time.Time) *time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	allASCIIDigits := true
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			allASCIIDigits = false
			break
		}
	}
	if allASCIIDigits {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds > uint64(math.MaxInt64/time.Second) {
			return nil
		}
		delay := time.Duration(seconds) * time.Second
		return &delay
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return nil
	}
	delay := when.Sub(now)
	if delay <= 0 {
		return nil
	}
	return &delay
}
