package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
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
	Client HTTPDoer
	Clock  Clock
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
			FailureInvalidRequest,
			fmt.Errorf("%w: nil context", ErrInvalidRequest),
			"OpenAI Responses request requires a context",
		)
	}
	if p == nil {
		return newResponsesFailureStream(
			ctx,
			clock,
			FailureConfiguration,
			fmt.Errorf("%w: nil provider", ErrInvalidOpenAIResponsesConfig),
			"OpenAI Responses provider is not configured",
		)
	}
	if err := request.validate(); err != nil {
		return newResponsesFailureStream(ctx, clock, FailureInvalidRequest, err, "")
	}
	if request.Model().Provider() != OpenAIProviderID || request.Model().API() != OpenAIResponsesAPI {
		cause := fmt.Errorf(
			"%w: model routes to provider %q API %q",
			ErrOpenAIResponsesRequest,
			request.Model().Provider(),
			request.Model().API(),
		)
		return newResponsesFailureStream(ctx, clock, FailureConfiguration, cause, "")
	}
	if p.configurationFail != nil {
		spec := *p.configurationFail
		return newResponsesFailureStream(ctx, clock, spec.kind, spec.cause, spec.message)
	}
	systemRole, err := p.systemRole.wireValue()
	if err != nil {
		return newResponsesFailureStream(ctx, clock, FailureConfiguration, err, "")
	}
	payload, err := encodeOpenAIResponsesRequest(request, systemRole)
	if err != nil {
		return newResponsesFailureStream(ctx, clock, FailureInvalidRequest, err, "")
	}
	streamContext, cancel := context.WithCancelCause(ctx)
	return &openAIResponsesStream{
		ctx:               streamContext,
		cancel:            cancel,
		endpoint:          p.endpoint,
		apiKey:            p.apiKey,
		client:            p.client,
		clock:             clock,
		timestamp:         clock(),
		payload:           payload,
		model:             request.Model(),
		maxEventBytes:     p.maxEventBytes,
		maxErrorBodyBytes: p.maxErrorBodyBytes,
		slots:             make(map[int]*responsesTextSlot),
		reasoningSlots:    make(map[int]*responsesReasoningSlot),
		toolSlots:         make(map[int]*responsesToolSlot),
		completedOutputs:  make(map[int]struct{}),
		completedItemIDs:  make(map[int]string),
		completedPhases:   make(map[int]string),
		pendingReasoning:  make(map[int]*responsesCompletedReasoning),
	}
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
