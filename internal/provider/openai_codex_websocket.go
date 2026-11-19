package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

const openAICodexWebSocketBeta = "responses_websockets=2026-02-06"

var (
	errCodexWebSocketClosed = errors.New("OpenAI Codex WebSocket stream closed")
	errCodexHeaderTimeout   = errors.New("OpenAI Codex SSE response headers timed out")
)

type openAICodexStreamConfig struct {
	ctx               context.Context
	endpoint          string
	token             string
	accountID         string
	payload           []byte
	model             Model
	headers           map[string]string
	client            HTTPDoer
	clock             Clock
	maxEventBytes     int
	maxErrorBodyBytes int
	configurationFail *responsesFailureSpec
	grammarProperties map[string]string
	options           StreamOptions
}

func (c openAICodexStreamConfig) newSSEStream() EventStream {
	var uncompressedPayload []byte
	if compressed, ok := compressCodexRequestZstd(c.payload); ok {
		uncompressedPayload = c.payload
		c.payload = compressed
		c.headers = cloneStrings(c.headers)
		c.headers["content-encoding"] = "zstd"
	}
	client := c.client
	if c.options.TimeoutMS != nil && *c.options.TimeoutMS != 0 {
		client = &codexHeaderTimeoutDoer{next: client, timeout: durationFromMilliseconds(*c.options.TimeoutMS)}
	}
	stream := c.newResponsesStream(client, c.options.OnResponse, c.options.OnHeaders, c.options.HeaderOverrides, valueOrZero32(c.options.MaxRetries), true)
	stream.terminalEndsStream = true
	stream.codexUncompressedPayload = uncompressedPayload
	return stream
}

// Keep this recovery local to one Codex SSE call. The original, post-hook JSON
// is reused byte-for-byte; later calls still start with compression enabled.
func (s *openAIResponsesStream) retryCodexWithoutCompression(failure *responsesFailureSpec) bool {
	if s.codexUncompressedPayload == nil || !failure.codexCompressionRejected {
		return false
	}
	s.payload = s.codexUncompressedPayload
	s.codexUncompressedPayload = nil
	s.codexCompressionFallback = true
	for name := range s.headers {
		if strings.EqualFold(name, "Content-Encoding") || strings.EqualFold(name, "Content-Length") {
			delete(s.headers, name)
		}
	}
	return true
}

func isCodexBareBadRequest(body []byte) bool {
	// Ignore JSON formatting, but reject extra/duplicate fields, other messages,
	// and malformed/trailing data. A generic HTTP 400 alone is not sufficient.
	var compact bytes.Buffer
	return json.Compact(&compact, body) == nil && compact.String() == `{"detail":"Bad Request"}`
}

func compressCodexRequestZstd(payload []byte) ([]byte, bool) {
	encoder, err := zstd.NewWriter(
		nil,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		return nil, false
	}
	defer encoder.Close()
	return encoder.EncodeAll(payload, nil), true
}

func (c openAICodexStreamConfig) newResponsesStream(client HTTPDoer, onResponse ResponseHook, onHeaders HeaderHook, overrides map[string]*string, maxRetries uint32, retry bool) *openAIResponsesStream {
	streamContext, cancel := context.WithCancelCause(c.ctx)
	return &openAIResponsesStream{
		diagnostics: newStreamDiagnostics(c.options, c.model, c.payload),
		ctx:         streamContext, cancel: cancel, timeoutCancel: func() {}, endpoint: c.endpoint, apiKey: c.token,
		authHeader: "authorization", displayName: "OpenAI Codex", configurationError: ErrInvalidOpenAICodexConfig,
		client: client, clock: c.clock, timestamp: c.clock(), payload: append([]byte(nil), c.payload...), model: c.model,
		headers: cloneStrings(c.headers), maxEventBytes: c.maxEventBytes, maxErrorBodyBytes: c.maxErrorBodyBytes,
		onResponse: onResponse, onHeaders: onHeaders, headerOverrides: cloneHeaderOverrides(overrides),
		configurationFail: c.configurationFail, maxRetries: maxRetries, maxRetryDelayMS: cloneUint64(c.options.MaxRetryDelayMS),
		serviceTier: c.options.ServiceTier, applyServiceTierPricing: true, codexServiceTier: true, codexRetry: retry,
		grammarProperties: cloneStrings(c.grammarProperties), slots: make(map[int]*responsesTextSlot),
		reasoningSlots: make(map[int]*responsesReasoningSlot), toolSlots: make(map[int]*responsesToolSlot),
		completedOutputs: make(map[int]struct{}), completedItemIDs: make(map[int]string), completedPhases: make(map[int]string),
		pendingReasoning: make(map[int]*responsesCompletedReasoning),
	}
}

// codexHeaderTimeoutDoer applies pi's Codex timeout only while waiting for
// response headers. A successful streaming body owns the derived context until
// Close, so stopping the header timer does not accidentally cancel the body.
type codexHeaderTimeoutDoer struct {
	next    HTTPDoer
	timeout time.Duration
}

func (d *codexHeaderTimeoutDoer) Do(request *http.Request) (*http.Response, error) {
	if d == nil || d.next == nil || d.timeout <= 0 {
		if d == nil || d.next == nil {
			return nil, errors.New("OpenAI Codex HTTP client is not configured")
		}
		return d.next.Do(request)
	}
	ctx, cancel := context.WithCancelCause(request.Context())
	timedOut := make(chan struct{})
	timer := time.AfterFunc(d.timeout, func() {
		close(timedOut)
		cancel(errCodexHeaderTimeout)
	})
	response, err := d.next.Do(request.Clone(ctx))
	if timer.Stop() {
		// The timeout cannot fire after this point.
	} else {
		<-timedOut
		cancel(errCodexHeaderTimeout)
	}
	if cause := context.Cause(ctx); errors.Is(cause, errCodexHeaderTimeout) {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		cancel(errCodexHeaderTimeout)
		return nil, fmt.Errorf("%w after %s", errCodexHeaderTimeout, d.timeout)
	}
	if err != nil || response == nil || response.Body == nil {
		cancel(err)
		return response, err
	}
	response.Body = &codexCancelBody{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

type codexCancelBody struct {
	io.ReadCloser
	cancel context.CancelCauseFunc
	once   sync.Once
}

func (b *codexCancelBody) Close() error {
	if b == nil {
		return nil
	}
	err := b.ReadCloser.Close()
	b.once.Do(func() { b.cancel(errCodexWebSocketClosed) })
	return err
}

func normalizeCodexWebSocketEvent(data []byte) []byte {
	var event map[string]any
	if json.Unmarshal(data, &event) != nil {
		return data
	}
	typeName, _ := event["type"].(string)
	switch typeName {
	case "response.done", "response.completed", "response.incomplete":
		if response, ok := event["response"].(map[string]any); ok {
			if status, ok := response["status"].(string); ok && !validCodexResponseStatus(status) {
				delete(response, "status")
			}
		}
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return data
	}
	return encoded
}

func validCodexResponseStatus(value string) bool {
	switch value {
	case "completed", "incomplete", "failed", "cancelled", "queued", "in_progress":
		return true
	default:
		return false
	}
}

func (c openAICodexStreamConfig) transportConfig() responsesTransportConfig {
	return responsesTransportConfig{
		ctx: c.ctx, endpoint: c.endpoint, payload: c.payload, model: c.model,
		client: c.client, clock: c.clock, maxEventBytes: c.maxEventBytes, options: c.options,
		scopeKey:       responsesTransportScope(c.endpoint, c.model, c.token, c.headers, c.options.HeaderOverrides),
		normalizeEvent: normalizeCodexWebSocketEvent,
		prepareHeaders: func(headers http.Header) {
			headers.Set("OpenAI-Beta", openAICodexWebSocketBeta)
		},
		validateHeaders: func(headers http.Header) error {
			if strings.TrimSpace(headers.Get("Authorization")) == "" || strings.TrimSpace(headers.Get("chatgpt-account-id")) == "" {
				return &responsesWebSocketConfigurationError{message: "OpenAI Codex WebSocket authorization headers are missing"}
			}
			return nil
		},
		newSSEStream: c.newSSEStream,
		newStream: func(client HTTPDoer) *openAIResponsesStream {
			return c.newResponsesStream(client, nil, nil, nil, 0, false)
		},
	}
}

// Existing Codex diagnostics and cleanup callers retain their entry points.
type OpenAICodexWebSocketDebugStats = OpenAIResponsesWebSocketDebugStats

func GetOpenAICodexWebSocketDebugStats(sessionID string) (OpenAICodexWebSocketDebugStats, bool) {
	return GetOpenAIResponsesWebSocketDebugStats(sessionID)
}

func ResetOpenAICodexWebSocketDebugStats(sessionID string) {
	ResetOpenAIResponsesWebSocketDebugStats(sessionID)
}

func CloseOpenAICodexWebSocketSessions(sessionID string) {
	CloseOpenAIResponsesWebSocketSessions(sessionID)
}
