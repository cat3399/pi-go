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
	AzureOpenAIResponsesAPI    = "azure-openai-responses"
	OpenAIProviderID           = "openai"
	AzureOpenAIProviderID      = "azure-openai-responses"
	defaultOpenAIResponsesBase = "https://api.openai.com/v1"
	defaultAzureOpenAPIVersion = "v1"
	defaultResponsesEventBytes = 1 << 20
	defaultResponsesErrorBytes = 64 << 10
)

var (
	ErrInvalidOpenAIResponsesConfig      = errors.New("invalid OpenAI Responses configuration")
	ErrOpenAIResponsesRequest            = errors.New("invalid OpenAI Responses request")
	ErrOpenAIResponsesStream             = errors.New("invalid OpenAI Responses stream")
	ErrOpenAIResponsesAborted            = errors.New("OpenAI Responses request aborted")
	ErrOpenAIResponsesUnsupported        = errors.New("unsupported OpenAI Responses behavior")
	ErrInvalidAzureOpenAIResponsesConfig = errors.New("invalid Azure OpenAI Responses configuration")
	errOpenAIResponsesStreamClosed       = errors.New("OpenAI Responses stream closed")
	errOpenAIResponsesStreamFinished     = errors.New("OpenAI Responses stream finished")
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
	// prompt. Zero selects "developer" for reasoning models whose compatibility
	// contract permits it and "system" otherwise.
	SystemRole OpenAIResponsesSystemRole

	// Zero selects bounded production defaults. Negative values are invalid.
	MaxEventBytes     int
	MaxErrorBodyBytes int
}

// AzureOpenAIResponsesConfig contains construction-time fallbacks. The typed
// StreamOptions Azure fields and request environment take precedence, matching
// the upstream per-call AzureOpenAIResponsesOptions contract.
type AzureOpenAIResponsesConfig struct {
	BaseURL           string
	ResourceName      string
	APIVersion        string
	DeploymentName    string
	APIKey            string
	Headers           map[string]string
	Client            HTTPDoer
	Clock             Clock
	SystemRole        OpenAIResponsesSystemRole
	MaxEventBytes     int
	MaxErrorBodyBytes int
}

type responsesDialect uint8

const (
	responsesDialectOpenAI responsesDialect = iota
	responsesDialectAzure
)

type azureOpenAIResponsesDefaults struct {
	baseURL, resourceName, apiVersion, deploymentName string
}

// OpenAIResponsesProvider owns the shared bounded Responses transport and
// state machine. Constructors select the standard OpenAI or Azure wire
// dialect without duplicating replay, tool, image, retry, or stream behavior.
type OpenAIResponsesProvider struct {
	api                string
	displayName        string
	dialect            responsesDialect
	configurationError error
	endpoint           string
	azure              azureOpenAIResponsesDefaults
	apiKey             string
	headers            map[string]string
	client             HTTPDoer
	clock              Clock
	systemRole         OpenAIResponsesSystemRole
	maxEventBytes      int
	maxErrorBodyBytes  int
	configurationFail  *responsesFailureSpec
}

// AzureOpenAIResponsesProvider uses the same bounded Responses parser and
// replay machinery with Azure's distinct endpoint, deployment, and api-key
// wire contract.
type AzureOpenAIResponsesProvider struct{ *OpenAIResponsesProvider }

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
		return "", fmt.Errorf("unknown system role %d", r)
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
	return newOpenAIResponsesProvider(config, endpoint, OpenAIResponsesAPI, "OpenAI", responsesDialectOpenAI, ErrInvalidOpenAIResponsesConfig)
}

func NewAzureOpenAIResponsesProvider(config AzureOpenAIResponsesConfig) (*AzureOpenAIResponsesProvider, error) {
	common := OpenAIResponsesConfig{
		APIKey: config.APIKey, Headers: config.Headers, Client: config.Client, Clock: config.Clock,
		SystemRole: config.SystemRole, MaxEventBytes: config.MaxEventBytes, MaxErrorBodyBytes: config.MaxErrorBodyBytes,
	}
	implementation, err := newOpenAIResponsesProvider(common, "", AzureOpenAIResponsesAPI, "Azure OpenAI", responsesDialectAzure, ErrInvalidAzureOpenAIResponsesConfig)
	if err != nil {
		return nil, err
	}
	implementation.azure = azureOpenAIResponsesDefaults{
		baseURL: config.BaseURL, resourceName: config.ResourceName, apiVersion: config.APIVersion, deploymentName: config.DeploymentName,
	}
	return &AzureOpenAIResponsesProvider{OpenAIResponsesProvider: implementation}, nil
}

func newOpenAIResponsesProvider(config OpenAIResponsesConfig, endpoint, api, displayName string, dialect responsesDialect, configurationError error) (*OpenAIResponsesProvider, error) {
	var configurationFail *responsesFailureSpec
	if !utf8.ValidString(config.APIKey) || strings.TrimSpace(config.APIKey) == "" {
		cause := fmt.Errorf("%w: API key must be non-empty valid UTF-8", configurationError)
		configurationFail = &responsesFailureSpec{
			kind: FailureConfiguration, cause: cause, message: displayName + " API key is not configured",
		}
	} else if strings.ContainsFunc(config.APIKey, unicode.IsControl) {
		cause := fmt.Errorf("%w: API key contains a control character", configurationError)
		configurationFail = &responsesFailureSpec{
			kind: FailureConfiguration, cause: cause, message: displayName + " API key is invalid",
		}
	}
	if config.MaxEventBytes < 0 || config.MaxErrorBodyBytes < 0 {
		return nil, fmt.Errorf("%w: byte limits cannot be negative", configurationError)
	}
	if _, err := config.SystemRole.wireValue(); err != nil {
		return nil, fmt.Errorf("%w: %v", configurationError, err)
	}
	client := config.Client
	if client == nil {
		client = http.DefaultClient
	} else if isTypedNil(client) {
		return nil, fmt.Errorf("%w: HTTP client is a typed nil", configurationError)
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
		api:                api,
		displayName:        displayName,
		dialect:            dialect,
		configurationError: configurationError,
		endpoint:           endpoint,
		apiKey:             config.APIKey,
		headers:            cloneStrings(config.Headers),
		client:             client,
		clock:              synchronizedClock(clock),
		systemRole:         config.SystemRole,
		maxEventBytes:      maxEventBytes,
		maxErrorBodyBytes:  maxErrorBodyBytes,
		configurationFail:  configurationFail,
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
	displayName := "OpenAI"
	configurationError := error(ErrInvalidOpenAIResponsesConfig)
	if p != nil && p.clock != nil {
		clock = p.clock
		displayName = p.displayName
		configurationError = p.configurationError
	}
	if ctx == nil {
		return newResponsesFailureStream(
			context.Background(),
			clock,
			request.Model(),
			FailureInvalidRequest,
			fmt.Errorf("%w: nil context", ErrInvalidRequest),
			displayName+" Responses request requires a context",
		)
	}
	if p == nil {
		return newResponsesFailureStream(
			ctx,
			clock,
			request.Model(),
			FailureConfiguration,
			fmt.Errorf("%w: nil provider", configurationError),
			displayName+" Responses provider is not configured",
		)
	}
	if err := request.validate(); err != nil {
		return newResponsesFailureStream(ctx, clock, request.Model(), FailureInvalidRequest, err, "")
	}
	if request.Model().API() != p.api {
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
	compat := request.Model().Compat().OpenAIResponses
	if p.systemRole == OpenAIResponsesSystemRoleDefault && request.Model().Reasoning() && (compat == nil || compat.SupportsDeveloperRole == nil || *compat.SupportsDeveloperRole) {
		systemRole = "developer"
	}
	if compat != nil && compat.SupportsDeveloperRole != nil && !*compat.SupportsDeveloperRole {
		systemRole = "system"
	}
	options := request.StreamOptions()
	endpoint := p.endpoint
	var payload []byte
	if p.dialect == responsesDialectAzure {
		var deploymentName string
		endpoint, deploymentName, err = p.resolveAzureResponsesTarget(request.Model(), options)
		if err == nil {
			payload, err = encodeAzureOpenAIResponsesRequest(request, systemRole, deploymentName)
		}
	} else {
		if baseURL := request.Model().BaseURL(); baseURL != "" {
			endpoint, err = responsesEndpoint(baseURL)
		}
		if err == nil {
			payload, err = encodeOpenAIResponsesRequest(request, systemRole)
		}
	}
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
	streamContext, cancel, timeoutCancel := streamContextWithTimeout(ctx, options.TimeoutMS)
	headers := mergeResponseHeaders(request.Model().Headers(), p.headers, options.Headers)
	if sessionID := options.SessionID; p.dialect == responsesDialectOpenAI && sessionID != "" && options.CacheRetention != CacheRetentionNone {
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
		ctx:                     streamContext,
		cancel:                  cancel,
		timeoutCancel:           timeoutCancel,
		endpoint:                endpoint,
		apiKey:                  requestAPIKey(request, p.apiKey),
		authHeader:              map[responsesDialect]string{responsesDialectOpenAI: "authorization", responsesDialectAzure: "api-key"}[p.dialect],
		displayName:             displayName,
		configurationError:      configurationError,
		client:                  client,
		clock:                   clock,
		timestamp:               clock(),
		payload:                 payload,
		model:                   request.Model(),
		headers:                 headers,
		maxEventBytes:           p.maxEventBytes,
		maxErrorBodyBytes:       p.maxErrorBodyBytes,
		onResponse:              options.OnResponse,
		onHeaders:               options.OnHeaders,
		headerOverrides:         cloneHeaderOverrides(options.HeaderOverrides),
		maxRetries:              valueOrZero32(options.MaxRetries),
		maxRetryDelayMS:         cloneUint64(options.MaxRetryDelayMS),
		serviceTier:             options.ServiceTier,
		applyServiceTierPricing: p.dialect == responsesDialectOpenAI,
		grammarProperties:       grammarProperties,
		configurationFail:       p.configurationFail,
		slots:                   make(map[int]*responsesTextSlot),
		reasoningSlots:          make(map[int]*responsesReasoningSlot),
		toolSlots:               make(map[int]*responsesToolSlot),
		completedOutputs:        make(map[int]struct{}),
		completedItemIDs:        make(map[int]string),
		completedPhases:         make(map[int]string),
		pendingReasoning:        make(map[int]*responsesCompletedReasoning),
	}
}

func valueOrZero32(value *uint32) uint32 {
	if value == nil {
		return 0
	}
	return *value
}

func (p *OpenAIResponsesProvider) SupportsModel(model Model) bool {
	return p != nil && model.API() == p.api
}

func (p *AzureOpenAIResponsesProvider) Stream(ctx context.Context, request Request) EventStream {
	if p == nil {
		return (*OpenAIResponsesProvider)(nil).Stream(ctx, request)
	}
	return p.OpenAIResponsesProvider.Stream(ctx, request)
}

func (p *AzureOpenAIResponsesProvider) SupportsModel(model Model) bool {
	return p != nil && p.OpenAIResponsesProvider.SupportsModel(model)
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
