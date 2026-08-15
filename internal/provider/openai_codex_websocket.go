package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/coder/websocket"
	"github.com/klauspost/compress/zstd"
)

const (
	openAICodexWebSocketBeta           = "responses_websockets=2026-02-06"
	openAICodexWebSocketIdleTTL        = 5 * time.Minute
	openAICodexWebSocketMaximumAge     = 55 * time.Minute
	defaultCodexWebSocketConnectTimout = 15 * time.Second
	openAICodexConnectionLimitCode     = "websocket_connection_limit_reached"
	openAICodexPreviousMissingCode     = "previous_response_not_found"
)

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
	if compressed, ok := compressCodexRequestZstd(c.payload); ok {
		c.payload = compressed
		c.headers = cloneStrings(c.headers)
		c.headers["content-encoding"] = "zstd"
	}
	client := c.client
	if c.options.TimeoutMS != nil && *c.options.TimeoutMS != 0 {
		client = &codexHeaderTimeoutDoer{next: client, timeout: durationFromMilliseconds(*c.options.TimeoutMS)}
	}
	stream := c.newResponsesStream(client, c.options.OnResponse, c.options.OnHeaders, c.options.HeaderOverrides, valueOrZero32(c.options.MaxRetries), true)
	if responses, ok := stream.(*openAIResponsesStream); ok {
		responses.terminalEndsStream = true
	}
	return stream
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

func (c openAICodexStreamConfig) newWebSocketStream() EventStream {
	doer := &codexWebSocketDoer{config: c}
	// WebSocket request retries are owned by the hybrid transport because it
	// must distinguish pre-stream connection failures from provider events.
	return c.newResponsesStream(doer, nil, nil, nil, 0, false)
}

func (c openAICodexStreamConfig) newResponsesStream(client HTTPDoer, onResponse ResponseHook, onHeaders HeaderHook, overrides map[string]*string, maxRetries uint32, retry bool) EventStream {
	streamContext, cancel := context.WithCancelCause(c.ctx)
	return &openAIResponsesStream{
		ctx: streamContext, cancel: cancel, timeoutCancel: func() {}, endpoint: c.endpoint, apiKey: c.token,
		client: client, clock: c.clock, timestamp: c.clock(), payload: append([]byte(nil), c.payload...), model: c.model,
		headers: cloneStrings(c.headers), maxEventBytes: c.maxEventBytes, maxErrorBodyBytes: c.maxErrorBodyBytes,
		onResponse: onResponse, onHeaders: onHeaders, headerOverrides: cloneHeaderOverrides(overrides),
		configurationFail: c.configurationFail, maxRetries: maxRetries, maxRetryDelayMS: cloneUint64(c.options.MaxRetryDelayMS),
		serviceTier: c.options.ServiceTier, codexServiceTier: true, codexRetry: retry,
		grammarProperties: cloneStrings(c.grammarProperties), slots: make(map[int]*responsesTextSlot),
		reasoningSlots: make(map[int]*responsesReasoningSlot), toolSlots: make(map[int]*responsesToolSlot),
		completedOutputs: make(map[int]struct{}), completedItemIDs: make(map[int]string), completedPhases: make(map[int]string),
		pendingReasoning: make(map[int]*responsesCompletedReasoning),
	}
}

func durationFromMilliseconds(value uint64) time.Duration {
	if value > uint64((1<<63-1)/int64(time.Millisecond)) {
		return time.Duration(1<<63 - 1)
	}
	return time.Duration(value) * time.Millisecond
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

type openAICodexHybridStream struct {
	config openAICodexStreamConfig

	mu       sync.Mutex
	active   EventStream
	closed   bool
	usingWS  bool
	started  bool
	previous bool
	limited  bool
	diag     *llm.AssistantDiagnostic
	terminal bool
}

func newOpenAICodexHybridStream(config openAICodexStreamConfig) EventStream {
	return &openAICodexHybridStream{config: config}
}

func (s *openAICodexHybridStream) Next() (llm.StreamEvent, error) {
	if s == nil {
		return nil, io.EOF
	}
	for {
		active, usingWS, closed := s.current()
		if closed || s.isTerminal() {
			return nil, io.EOF
		}
		if active == nil {
			sessionID := codexCacheSessionID(s.config.options)
			if codexWebSocketFallbackActive(sessionID) {
				recordCodexWebSocketSSEFallback(sessionID)
				active = s.config.newSSEStream()
				usingWS = false
			} else {
				active = s.config.newWebSocketStream()
				usingWS = true
			}
			s.setActive(active, usingWS, false)
		}

		event, err := active.Next()
		if err != nil {
			if usingWS && !s.hasStarted() && context.Cause(s.config.ctx) == nil {
				s.fallback(active, err)
				continue
			}
			return nil, err
		}
		if _, ok := event.(llm.StartEvent); ok {
			s.markStarted()
			return event, nil
		}
		if failed, ok := event.(llm.ErrorEvent); ok && usingWS && !s.hasStarted() {
			if context.Cause(s.config.ctx) != nil {
				return event, nil
			}
			cause := failed.Failure().Cause()
			classification, code := classifyCodexWebSocketFailure(cause)
			switch classification {
			case codexWebSocketFailurePreviousMissing:
				if !s.previous {
					s.previous = true
					_ = active.Close()
					s.setActive(s.config.newWebSocketStream(), true, false)
					continue
				}
			case codexWebSocketFailureConnectionLimit:
				if !s.limited {
					s.limited = true
					_ = active.Close()
					s.setActive(s.config.newWebSocketStream(), true, false)
					continue
				}
			case codexWebSocketFailureAPI, codexWebSocketFailureProtocol, codexWebSocketFailureConfiguration:
				s.markTerminal()
				return rewriteCodexWebSocketError(failed, classification, code), nil
			case codexWebSocketFailureCancelled:
				s.markTerminal()
				return event, nil
			}
			s.fallback(active, cause)
			continue
		}
		if !usingWS {
			if codexTerminalStreamEvent(event) {
				s.markTerminal()
			}
			return s.attachFallbackDiagnostic(event), nil
		}
		if codexTerminalStreamEvent(event) {
			s.markTerminal()
		}
		return event, nil
	}
}

func (s *openAICodexHybridStream) current() (EventStream, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, s.usingWS, s.closed
}

func (s *openAICodexHybridStream) setActive(active EventStream, usingWS, started bool) {
	s.mu.Lock()
	s.active, s.usingWS, s.started = active, usingWS, started
	s.mu.Unlock()
}

func (s *openAICodexHybridStream) markStarted() {
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()
}

func (s *openAICodexHybridStream) hasStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

func (s *openAICodexHybridStream) isTerminal() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminal
}

func (s *openAICodexHybridStream) markTerminal() {
	s.mu.Lock()
	s.terminal = true
	s.mu.Unlock()
}

func codexTerminalStreamEvent(event llm.StreamEvent) bool {
	switch event.(type) {
	case llm.DoneEvent, llm.ErrorEvent:
		return true
	default:
		return false
	}
}

func (s *openAICodexHybridStream) fallback(active EventStream, cause error) {
	_ = active.Close()
	sessionID := codexCacheSessionID(s.config.options)
	recordCodexWebSocketFailure(sessionID, cause)
	diagnostic := codexFallbackDiagnostic(s.config, cause)
	s.mu.Lock()
	s.diag = diagnostic
	s.active = s.config.newSSEStream()
	s.usingWS = false
	s.started = false
	s.mu.Unlock()
}

func (s *openAICodexHybridStream) attachFallbackDiagnostic(event llm.StreamEvent) llm.StreamEvent {
	s.mu.Lock()
	diagnostic := s.diag
	s.mu.Unlock()
	if diagnostic == nil {
		return event
	}
	switch value := event.(type) {
	case llm.DoneEvent:
		diagnostics := append(value.Diagnostics(), *diagnostic)
		var response *llm.AssistantResponseMetadata
		if metadata, ok := value.ResponseMetadata(); ok {
			response = &metadata
		}
		rebuilt, err := llm.NewDoneEventWithMetadata(value.Reason(), value.Usage(), value.Timestamp(), value.AssistantProvenance(), response, diagnostics)
		if err == nil {
			return rebuilt
		}
	case llm.ErrorEvent:
		diagnostics := append(value.Diagnostics(), *diagnostic)
		var response *llm.AssistantResponseMetadata
		if metadata, ok := value.ResponseMetadata(); ok {
			response = &metadata
		}
		rebuilt, err := llm.NewErrorEventWithMetadata(value.Reason(), value.Failure(), value.Usage(), value.Timestamp(), value.AssistantProvenance(), response, diagnostics)
		if err == nil {
			return rebuilt
		}
	}
	return event
}

func (s *openAICodexHybridStream) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		active := s.active
		s.mu.Unlock()
		if active != nil {
			return active.Close()
		}
		return nil
	}
	s.closed = true
	active := s.active
	s.mu.Unlock()
	if active != nil {
		return active.Close()
	}
	return nil
}

func codexFallbackDiagnostic(config openAICodexStreamConfig, cause error) *llm.AssistantDiagnostic {
	transport := config.options.Transport
	if transport == "" {
		transport = TransportAuto
	}
	details, _ := json.Marshal(map[string]any{
		"configuredTransport": transport, "fallbackTransport": "sse", "eventsEmitted": false,
		"phase": "before_message_stream_start", "requestBytes": len(config.payload),
	})
	timestamp := config.clock().UTC().Truncate(time.Millisecond)
	if timestamp.IsZero() {
		timestamp = time.UnixMilli(1).UTC()
	}
	message := "OpenAI Codex WebSocket transport failed"
	if cause != nil && utf8.ValidString(cause.Error()) && strings.TrimSpace(cause.Error()) != "" {
		message = cause.Error()
	}
	diagnostic, err := llm.NewAssistantDiagnostic(llm.AssistantDiagnosticSpec{
		Type: "provider_transport_failure", Timestamp: timestamp,
		Error: &llm.AssistantDiagnosticError{Name: fmt.Sprintf("%T", cause), Message: message}, Details: details,
	})
	if err != nil {
		return nil
	}
	return &diagnostic
}

type codexWebSocketFailureClass uint8

const (
	codexWebSocketFailureTransport codexWebSocketFailureClass = iota + 1
	codexWebSocketFailureAPI
	codexWebSocketFailureProtocol
	codexWebSocketFailureConfiguration
	codexWebSocketFailureConnectionLimit
	codexWebSocketFailurePreviousMissing
	codexWebSocketFailureCancelled
)

type codexWebSocketAPIError struct{ code, message string }

func (e *codexWebSocketAPIError) Error() string {
	if e == nil {
		return "OpenAI Codex WebSocket API error"
	}
	if e.message != "" {
		return e.message
	}
	return e.code
}

type codexWebSocketProtocolError struct{ message string }

func (e *codexWebSocketProtocolError) Error() string { return e.message }

type codexWebSocketConfigurationError struct{ message string }

func (e *codexWebSocketConfigurationError) Error() string { return e.message }

func classifyCodexWebSocketFailure(cause error) (codexWebSocketFailureClass, string) {
	if errors.Is(cause, context.Canceled) {
		return codexWebSocketFailureCancelled, ""
	}
	var apiError *codexWebSocketAPIError
	if errors.As(cause, &apiError) {
		switch apiError.code {
		case openAICodexConnectionLimitCode:
			return codexWebSocketFailureConnectionLimit, apiError.code
		case openAICodexPreviousMissingCode:
			return codexWebSocketFailurePreviousMissing, apiError.code
		default:
			return codexWebSocketFailureAPI, apiError.code
		}
	}
	var protocolError *codexWebSocketProtocolError
	if errors.As(cause, &protocolError) {
		return codexWebSocketFailureProtocol, ""
	}
	var configurationError *codexWebSocketConfigurationError
	if errors.As(cause, &configurationError) {
		return codexWebSocketFailureConfiguration, ""
	}
	return codexWebSocketFailureTransport, ""
}

func rewriteCodexWebSocketError(event llm.ErrorEvent, classification codexWebSocketFailureClass, code string) llm.ErrorEvent {
	kind := FailureInvalidResponse
	if classification == codexWebSocketFailureConfiguration {
		kind = FailureConfiguration
	}
	cause := event.Failure().Cause()
	providerFailure, err := NewProviderFailure(ProviderFailureSpec{Kind: kind, Message: event.ErrorMessage(), Cause: cause, VendorCode: code})
	if err != nil {
		return event
	}
	failure, err := llm.NewFailure(providerFailure.Error(), providerFailure)
	if err != nil {
		return event
	}
	var response *llm.AssistantResponseMetadata
	if metadata, ok := event.ResponseMetadata(); ok {
		response = &metadata
	}
	rebuilt, err := llm.NewErrorEventWithMetadata(event.Reason(), failure, event.Usage(), event.Timestamp(), event.AssistantProvenance(), response, event.Diagnostics())
	if err != nil {
		return event
	}
	return rebuilt
}

type codexWebSocketDoer struct{ config openAICodexStreamConfig }

func (d *codexWebSocketDoer) Do(request *http.Request) (*http.Response, error) {
	if d == nil {
		return nil, errors.New("OpenAI Codex WebSocket transport is nil")
	}
	config := d.config
	headers := request.Header.Clone()
	headers.Del("accept")
	headers.Del("content-type")
	headers.Del("OpenAI-Beta")
	headers.Set("OpenAI-Beta", openAICodexWebSocketBeta)
	requestID := codexCacheSessionID(config.options)
	if requestID == "" {
		requestID = newCodexRequestID(config.clock())
	}
	headers.Set("x-client-request-id", requestID)
	headers.Set("session-id", requestID)
	if err := applyFinalHeaders(headers, config.model, config.options.OnHeaders, cloneHeaderOverrides(config.options.HeaderOverrides)); err != nil {
		return nil, &codexWebSocketConfigurationError{message: "OpenAI Codex WebSocket header hook failed: " + err.Error()}
	}
	if strings.TrimSpace(headers.Get("Authorization")) == "" || strings.TrimSpace(headers.Get("chatgpt-account-id")) == "" {
		return nil, &codexWebSocketConfigurationError{message: "OpenAI Codex WebSocket authorization headers are missing"}
	}

	var fullBody map[string]any
	if err := json.Unmarshal(config.payload, &fullBody); err != nil || fullBody == nil {
		return nil, &codexWebSocketProtocolError{message: "OpenAI Codex WebSocket request payload is invalid"}
	}
	useCachedContext := config.options.Transport == TransportWebsocketCached || config.options.Transport == TransportAuto || config.options.Transport == ""
	lease, err := acquireCodexWebSocket(request.Context(), config, headers)
	if err != nil {
		return nil, err
	}
	requestBody := fullBody
	if useCachedContext && lease.entry != nil {
		requestBody = buildCachedCodexWebSocketRequest(lease.entry, fullBody)
	}
	frame := map[string]any{"type": "response.create"}
	for key, value := range requestBody {
		frame[key] = value
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		lease.release(false)
		return nil, &codexWebSocketProtocolError{message: "encode OpenAI Codex WebSocket request: " + err.Error()}
	}
	if err := lease.conn.Write(request.Context(), websocket.MessageText, encoded); err != nil {
		lease.release(false)
		return nil, fmt.Errorf("write OpenAI Codex WebSocket request: %w", err)
	}
	recordCodexWebSocketRequest(lease, requestBody, useCachedContext)
	first, err := readFirstCodexWebSocketEvent(request.Context(), lease.conn, config.options, config.maxEventBytes)
	if err != nil {
		if lease.entry != nil {
			lease.entry.clearContinuation()
		}
		lease.release(false)
		return nil, err
	}
	reader, writer := io.Pipe()
	body := &codexWebSocketBody{
		reader: reader, writer: writer, ctx: request.Context(), lease: lease, first: first,
		maxEventBytes: config.maxEventBytes, fullBody: cloneJSONMap(fullBody), useCachedContext: useCachedContext,
	}
	go body.run()
	return &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: body, Request: request,
	}, nil
}

func readFirstCodexWebSocketEvent(ctx context.Context, conn *websocket.Conn, options StreamOptions, maxBytes int) ([]byte, error) {
	for {
		data, err := readCodexWebSocketMessage(ctx, conn, options.TimeoutMS)
		if err != nil {
			return nil, fmt.Errorf("read first OpenAI Codex WebSocket event: %w", err)
		}
		if maxBytes > 0 && len(data) > maxBytes {
			return nil, &codexWebSocketProtocolError{message: "OpenAI Codex WebSocket event exceeds the configured size limit"}
		}
		typeName, apiError, protocolError := inspectCodexWebSocketEvent(data)
		if protocolError != nil {
			return nil, protocolError
		}
		if typeName == "" {
			continue
		}
		if apiError != nil {
			return nil, apiError
		}
		return normalizeCodexWebSocketEvent(data), nil
	}
}

func readCodexWebSocketMessage(ctx context.Context, conn *websocket.Conn, timeoutMS *uint64) ([]byte, error) {
	readContext := ctx
	cancel := func() {}
	if timeoutMS != nil && *timeoutMS != 0 {
		readContext, cancel = context.WithTimeout(ctx, durationFromMilliseconds(*timeoutMS))
	}
	defer cancel()
	_, data, err := conn.Read(readContext)
	return data, err
}

func inspectCodexWebSocketEvent(data []byte) (string, *codexWebSocketAPIError, *codexWebSocketProtocolError) {
	if !utf8.Valid(data) {
		return "", nil, &codexWebSocketProtocolError{message: "OpenAI Codex WebSocket event is not valid UTF-8"}
	}
	var event map[string]any
	if err := json.Unmarshal(data, &event); err != nil || event == nil {
		if err == nil {
			err = errors.New("top-level event is not an object")
		}
		return "", nil, &codexWebSocketProtocolError{message: "Invalid OpenAI Codex WebSocket JSON: " + err.Error()}
	}
	typeName, _ := event["type"].(string)
	if typeName == "error" {
		code, message := codexEventError(event)
		return typeName, &codexWebSocketAPIError{code: code, message: firstNonBlank(message, code, "OpenAI Codex WebSocket returned an error")}, nil
	}
	if typeName == "response.failed" {
		response, _ := event["response"].(map[string]any)
		nested, _ := response["error"].(map[string]any)
		code, _ := nested["code"].(string)
		message, _ := nested["message"].(string)
		return typeName, &codexWebSocketAPIError{code: code, message: firstNonBlank(message, code, "OpenAI Codex response failed")}, nil
	}
	return typeName, nil, nil
}

func codexEventError(event map[string]any) (string, string) {
	code, _ := event["code"].(string)
	message, _ := event["message"].(string)
	if nested, ok := event["error"].(map[string]any); ok {
		if code == "" {
			code, _ = nested["code"].(string)
		}
		if message == "" {
			message, _ = nested["message"].(string)
		}
	}
	return code, message
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return "OpenAI Codex error"
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

type codexWebSocketBody struct {
	reader *io.PipeReader
	writer *io.PipeWriter
	ctx    context.Context
	lease  *codexWebSocketLease
	first  []byte

	maxEventBytes    int
	fullBody         map[string]any
	useCachedContext bool
	responseItems    []any

	mu        sync.Mutex
	terminal  bool
	validated bool
	failed    bool
	once      sync.Once
}

func (b *codexWebSocketBody) Read(buffer []byte) (int, error) { return b.reader.Read(buffer) }

func (b *codexWebSocketBody) Close() error {
	if b == nil {
		return nil
	}
	err := b.reader.Close()
	b.once.Do(func() {
		b.mu.Lock()
		keep := b.terminal && b.validated && !b.failed
		b.mu.Unlock()
		b.lease.release(keep)
	})
	return err
}

func (b *codexWebSocketBody) run() {
	defer b.writer.Close()
	data := b.first
	for {
		typeName, apiError, protocolError := inspectCodexWebSocketEvent(data)
		if protocolError != nil {
			b.markFailed()
			_ = b.writer.CloseWithError(protocolError)
			return
		}
		if typeName == "" {
			var err error
			data, err = readCodexWebSocketMessage(b.ctx, b.lease.conn, b.lease.options.TimeoutMS)
			if err != nil {
				b.failWriter(fmt.Errorf("read OpenAI Codex WebSocket event: %w", err))
				return
			}
			continue
		}
		if apiError != nil {
			b.markFailed()
		}
		data = normalizeCodexWebSocketEvent(data)
		b.observeOutputItem(data)
		if b.maxEventBytes > 0 && len(data) > b.maxEventBytes {
			b.failWriter(&codexWebSocketProtocolError{message: "OpenAI Codex WebSocket event exceeds the configured size limit"})
			return
		}
		if _, err := b.writer.Write(append(append([]byte("data: "), data...), '\n', '\n')); err != nil {
			b.markFailed()
			return
		}
		if apiError != nil {
			return
		}
		if codexTerminalEvent(typeName) {
			b.observeTerminal(data)
			return
		}
		var err error
		data, err = readCodexWebSocketMessage(b.ctx, b.lease.conn, b.lease.options.TimeoutMS)
		if err != nil {
			b.failWriter(fmt.Errorf("read OpenAI Codex WebSocket event: %w", err))
			return
		}
	}
}

func (b *codexWebSocketBody) failWriter(err error) {
	b.markFailed()
	_ = b.writer.CloseWithError(err)
}

func (b *codexWebSocketBody) markFailed() {
	b.mu.Lock()
	b.failed = true
	b.mu.Unlock()
	if b.lease.entry != nil {
		b.lease.entry.clearContinuation()
	}
}

func (b *codexWebSocketBody) observeTerminal(_ []byte) {
	b.mu.Lock()
	b.terminal = true
	b.mu.Unlock()
	// The Responses parser commits the connection/continuation only after it has
	// validated this terminal event. Merely observing terminal bytes is not enough
	// to make the connection reusable.
}

func (b *codexWebSocketBody) commitResponsesTerminal(responseID string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.terminal = true
	b.validated = true
	useCachedContext := b.useCachedContext
	entry := b.lease.entry
	responseItems := append([]any(nil), b.responseItems...)
	b.mu.Unlock()
	if !useCachedContext || entry == nil {
		return
	}
	if strings.TrimSpace(responseID) == "" {
		b.lease.entry.clearContinuation()
		return
	}
	items := make([]any, 0, len(responseItems))
	for _, item := range responseItems {
		canonical, ok := canonicalCodexContinuationItem(item)
		if !ok {
			entry.clearContinuation()
			return
		}
		items = append(items, canonical)
	}
	entry.setContinuation(codexWebSocketContinuation{
		lastRequestBody: cloneJSONMap(b.fullBody), lastResponseID: responseID, lastResponseItems: cloneJSONSlice(items),
	})
}

// canonicalCodexContinuationItem mirrors the Responses replay converter used
// for the normalized final assistant message. output_item.done is the only
// authoritative wire source available to the WebSocket body, and its raw item
// may contain status/future fields or multipart summaries that the next
// request's converter intentionally rewrites. Canonicalizing here makes the
// cached baseline byte-for-value equivalent to that next request.
func canonicalCodexContinuationItem(value any) (any, bool) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var item responsesOutputItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, false
	}
	switch item.Type {
	case "message":
		text, failure := responsesOutputItemText(item)
		if failure != nil || item.ID == "" {
			return nil, false
		}
		return responsesOutputMessage{
			Type: "message", ID: normalizeResponsesMessageItemID(item.ID), Role: "assistant", Status: "completed", Phase: item.Phase,
			Content: []responsesOutputText{{Type: "output_text", Text: text, Annotations: []any{}}},
		}, true
	case "reasoning":
		if item.ID == "" {
			return nil, false
		}
		text := responsesReasoningText(item)
		result := responsesReasoningInput{Type: "reasoning", ID: item.ID, EncryptedContent: item.EncryptedContent}
		if item.EncryptedContent == "" {
			result.Content = text
		} else if text != "" {
			result.Summary = []responsesReasoningSummary{{Type: "summary_text", Text: text}}
		}
		return result, true
	case "function_call":
		if item.CallID == "" || item.Name == "" {
			return nil, false
		}
		return responsesFunctionCall{
			Type: "function_call", ID: normalizeResponsesFunctionItemID(item.ID), CallID: normalizeResponsesCallID(item.CallID),
			Name: item.Name, Arguments: item.Arguments,
		}, true
	case "custom_tool_call":
		if item.CallID == "" || item.Name == "" {
			return nil, false
		}
		return responsesCustomToolCall{
			Type: "custom_tool_call", ID: item.ID, CallID: normalizeResponsesCallID(item.CallID), Name: item.Name, Input: item.Input,
		}, true
	default:
		return nil, false
	}
}

func (b *codexWebSocketBody) observeOutputItem(data []byte) {
	if b == nil || !b.useCachedContext {
		return
	}
	var event struct {
		Type string `json:"type"`
		Item any    `json:"item"`
	}
	if json.Unmarshal(data, &event) != nil || event.Type != "response.output_item.done" || event.Item == nil {
		return
	}
	b.mu.Lock()
	b.responseItems = append(b.responseItems, event.Item)
	b.mu.Unlock()
}

func codexTerminalEvent(typeName string) bool {
	return typeName == "response.done" || typeName == "response.completed" || typeName == "response.incomplete"
}

func cloneJSONMap(value map[string]any) map[string]any {
	encoded, _ := json.Marshal(value)
	var cloned map[string]any
	_ = json.Unmarshal(encoded, &cloned)
	return cloned
}

func cloneJSONSlice(value []any) []any {
	encoded, _ := json.Marshal(value)
	var cloned []any
	_ = json.Unmarshal(encoded, &cloned)
	return cloned
}

type codexWebSocketContinuation struct {
	lastRequestBody   map[string]any
	lastResponseID    string
	lastResponseItems []any
}

type codexWebSocketEntry struct {
	mu           sync.Mutex
	conn         *websocket.Conn
	busy         bool
	createdAt    time.Time
	idleTimer    *time.Timer
	continuation *codexWebSocketContinuation
}

func (e *codexWebSocketEntry) clearContinuation() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.continuation = nil
	e.mu.Unlock()
}

func (e *codexWebSocketEntry) setContinuation(value codexWebSocketContinuation) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.continuation = &value
	e.mu.Unlock()
}

type codexWebSocketLease struct {
	conn      *websocket.Conn
	entry     *codexWebSocketEntry
	sessionID string
	accountID string
	reused    bool
	temporary bool
	options   StreamOptions
	once      sync.Once
}

func (l *codexWebSocketLease) release(keep bool) {
	if l == nil {
		return
	}
	l.once.Do(func() { releaseCodexWebSocket(l, keep) })
}

type OpenAICodexWebSocketDebugStats struct {
	Requests                uint64
	ConnectionsCreated      uint64
	ConnectionsReused       uint64
	CachedContextRequests   uint64
	StoreTrueRequests       uint64
	FullContextRequests     uint64
	DeltaRequests           uint64
	LastInputItems          int
	LastDeltaInputItems     *int
	LastPreviousResponseID  string
	WebSocketFailures       uint64
	SSEFallbacks            uint64
	WebSocketFallbackActive bool
	LastWebSocketError      string
}

var codexWebSocketState = struct {
	sync.Mutex
	sessions map[string]map[string]*codexWebSocketEntry
	fallback map[string]struct{}
	stats    map[string]*OpenAICodexWebSocketDebugStats
}{sessions: map[string]map[string]*codexWebSocketEntry{}, fallback: map[string]struct{}{}, stats: map[string]*OpenAICodexWebSocketDebugStats{}}

func acquireCodexWebSocket(ctx context.Context, config openAICodexStreamConfig, headers http.Header) (*codexWebSocketLease, error) {
	sessionID := codexCacheSessionID(config.options)
	if sessionID != "" {
		codexWebSocketState.Lock()
		entry := codexWebSocketState.sessions[sessionID][config.accountID]
		if entry != nil && !entry.busy && config.clock().Sub(entry.createdAt) < openAICodexWebSocketMaximumAge {
			entry.busy = true
			if entry.idleTimer != nil {
				entry.idleTimer.Stop()
				entry.idleTimer = nil
			}
			codexWebSocketState.Unlock()
			entry.conn.SetReadLimit(int64(config.maxEventBytes))
			return &codexWebSocketLease{conn: entry.conn, entry: entry, sessionID: sessionID, accountID: config.accountID, reused: true, options: config.options}, nil
		}
		if entry != nil && !entry.busy {
			delete(codexWebSocketState.sessions[sessionID], config.accountID)
			if len(codexWebSocketState.sessions[sessionID]) == 0 {
				delete(codexWebSocketState.sessions, sessionID)
			}
			entry.conn.CloseNow()
		}
		busy := entry != nil && entry.busy
		codexWebSocketState.Unlock()
		conn, err := dialCodexWebSocket(ctx, config, headers)
		if err != nil {
			return nil, err
		}
		if busy {
			return &codexWebSocketLease{conn: conn, sessionID: sessionID, accountID: config.accountID, temporary: true, options: config.options}, nil
		}
		entry = &codexWebSocketEntry{conn: conn, busy: true, createdAt: config.clock()}
		codexWebSocketState.Lock()
		accounts := codexWebSocketState.sessions[sessionID]
		if accounts == nil {
			accounts = map[string]*codexWebSocketEntry{}
			codexWebSocketState.sessions[sessionID] = accounts
		}
		if existing := accounts[config.accountID]; existing != nil {
			codexWebSocketState.Unlock()
			return &codexWebSocketLease{conn: conn, sessionID: sessionID, accountID: config.accountID, temporary: true, options: config.options}, nil
		}
		accounts[config.accountID] = entry
		codexWebSocketState.Unlock()
		return &codexWebSocketLease{conn: conn, entry: entry, sessionID: sessionID, accountID: config.accountID, options: config.options}, nil
	}
	conn, err := dialCodexWebSocket(ctx, config, headers)
	if err != nil {
		return nil, err
	}
	return &codexWebSocketLease{conn: conn, accountID: config.accountID, temporary: true, options: config.options}, nil
}

func dialCodexWebSocket(ctx context.Context, config openAICodexStreamConfig, headers http.Header) (*websocket.Conn, error) {
	websocketURL, err := codexWebSocketEndpoint(config.endpoint)
	if err != nil {
		return nil, &codexWebSocketConfigurationError{message: err.Error()}
	}
	connectContext := ctx
	cancel := func() {}
	timeout := defaultCodexWebSocketConnectTimout
	if config.options.WebsocketConnectTimeoutMS != nil {
		timeout = durationFromMilliseconds(*config.options.WebsocketConnectTimeoutMS)
	}
	if timeout > 0 {
		connectContext, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	dialOptions := &websocket.DialOptions{HTTPHeader: headers, CompressionMode: websocket.CompressionDisabled}
	if client, ok := config.client.(*http.Client); ok {
		dialOptions.HTTPClient = client
	}
	conn, response, err := websocket.Dial(connectContext, websocketURL, dialOptions)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("connect OpenAI Codex WebSocket: %w", err)
	}
	conn.SetReadLimit(int64(config.maxEventBytes))
	return conn, nil
}

func codexWebSocketEndpoint(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("%w: invalid Codex WebSocket endpoint", ErrInvalidOpenAICodexConfig)
	}
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	default:
		return "", fmt.Errorf("%w: invalid Codex WebSocket endpoint scheme", ErrInvalidOpenAICodexConfig)
	}
	return parsed.String(), nil
}

func releaseCodexWebSocket(lease *codexWebSocketLease, keep bool) {
	if lease == nil || lease.conn == nil {
		return
	}
	if lease.temporary || lease.entry == nil || !keep {
		codexWebSocketState.Lock()
		if lease.entry != nil {
			if accounts := codexWebSocketState.sessions[lease.sessionID]; accounts != nil && accounts[lease.accountID] == lease.entry {
				delete(accounts, lease.accountID)
				if len(accounts) == 0 {
					delete(codexWebSocketState.sessions, lease.sessionID)
				}
			}
		}
		codexWebSocketState.Unlock()
		lease.conn.CloseNow()
		return
	}
	codexWebSocketState.Lock()
	accounts := codexWebSocketState.sessions[lease.sessionID]
	if accounts == nil || accounts[lease.accountID] != lease.entry {
		codexWebSocketState.Unlock()
		lease.conn.CloseNow()
		return
	}
	lease.entry.busy = false
	entry := lease.entry
	entry.idleTimer = time.AfterFunc(openAICodexWebSocketIdleTTL, func() {
		codexWebSocketState.Lock()
		accounts := codexWebSocketState.sessions[lease.sessionID]
		if accounts != nil && accounts[lease.accountID] == entry && !entry.busy {
			delete(accounts, lease.accountID)
			if len(accounts) == 0 {
				delete(codexWebSocketState.sessions, lease.sessionID)
			}
			codexWebSocketState.Unlock()
			entry.conn.CloseNow()
			return
		}
		codexWebSocketState.Unlock()
	})
	codexWebSocketState.Unlock()
}

func buildCachedCodexWebSocketRequest(entry *codexWebSocketEntry, body map[string]any) map[string]any {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.continuation == nil {
		return body
	}
	continuation := entry.continuation
	if !codexBodiesMatchExceptInput(body, continuation.lastRequestBody) {
		entry.continuation = nil
		return body
	}
	current, _ := body["input"].([]any)
	previous, _ := continuation.lastRequestBody["input"].([]any)
	baseline := append(cloneJSONSlice(previous), cloneJSONSlice(continuation.lastResponseItems)...)
	if len(current) < len(baseline) || !jsonValuesEqual(current[:len(baseline)], baseline) {
		entry.continuation = nil
		return body
	}
	result := cloneJSONMap(body)
	result["previous_response_id"] = continuation.lastResponseID
	result["input"] = cloneJSONSlice(current[len(baseline):])
	return result
}

func codexBodiesMatchExceptInput(left, right map[string]any) bool {
	copyBody := func(value map[string]any) map[string]any {
		cloned := cloneJSONMap(value)
		delete(cloned, "input")
		delete(cloned, "previous_response_id")
		return cloned
	}
	return jsonValuesEqual(copyBody(left), copyBody(right))
}

func jsonValuesEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func codexCacheSessionID(options StreamOptions) string {
	if resolveOpenAICacheRetention(options) == CacheRetentionNone || options.SessionID == "" {
		return ""
	}
	return clampOpenAIPromptCacheKey(options.SessionID)
}

func newCodexRequestID(now time.Time) string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		binary.BigEndian.PutUint64(value[8:], uint64(now.UnixNano()))
	}
	milliseconds := uint64(now.UnixMilli())
	value[0] = byte(milliseconds >> 40)
	value[1] = byte(milliseconds >> 32)
	value[2] = byte(milliseconds >> 24)
	value[3] = byte(milliseconds >> 16)
	value[4] = byte(milliseconds >> 8)
	value[5] = byte(milliseconds)
	value[6] = value[6]&0x0f | 0x70
	value[8] = value[8]&0x3f | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

func recordCodexWebSocketRequest(lease *codexWebSocketLease, body map[string]any, cached bool) {
	if lease == nil || lease.sessionID == "" {
		return
	}
	codexWebSocketState.Lock()
	stats := codexStatsLocked(lease.sessionID)
	stats.Requests++
	if lease.reused {
		stats.ConnectionsReused++
	} else {
		stats.ConnectionsCreated++
	}
	if cached {
		stats.CachedContextRequests++
	}
	if stored, _ := body["store"].(bool); stored {
		stats.StoreTrueRequests++
	}
	input, _ := body["input"].([]any)
	stats.LastInputItems = len(input)
	if previous, _ := body["previous_response_id"].(string); previous != "" {
		stats.DeltaRequests++
		value := len(input)
		stats.LastDeltaInputItems = &value
		stats.LastPreviousResponseID = previous
	} else {
		stats.FullContextRequests++
		stats.LastDeltaInputItems = nil
		stats.LastPreviousResponseID = ""
	}
	codexWebSocketState.Unlock()
}

func codexStatsLocked(sessionID string) *OpenAICodexWebSocketDebugStats {
	stats := codexWebSocketState.stats[sessionID]
	if stats == nil {
		stats = &OpenAICodexWebSocketDebugStats{}
		codexWebSocketState.stats[sessionID] = stats
	}
	return stats
}

func codexWebSocketFallbackActive(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	codexWebSocketState.Lock()
	_, active := codexWebSocketState.fallback[sessionID]
	codexWebSocketState.Unlock()
	return active
}

func recordCodexWebSocketFailure(sessionID string, cause error) {
	if sessionID == "" {
		return
	}
	codexWebSocketState.Lock()
	codexWebSocketState.fallback[sessionID] = struct{}{}
	stats := codexStatsLocked(sessionID)
	stats.WebSocketFailures++
	stats.WebSocketFallbackActive = true
	if cause != nil {
		stats.LastWebSocketError = cause.Error()
	}
	codexWebSocketState.Unlock()
}

func recordCodexWebSocketSSEFallback(sessionID string) {
	if sessionID == "" {
		return
	}
	codexWebSocketState.Lock()
	stats := codexStatsLocked(sessionID)
	stats.SSEFallbacks++
	_, stats.WebSocketFallbackActive = codexWebSocketState.fallback[sessionID]
	codexWebSocketState.Unlock()
}

func GetOpenAICodexWebSocketDebugStats(sessionID string) (OpenAICodexWebSocketDebugStats, bool) {
	codexWebSocketState.Lock()
	defer codexWebSocketState.Unlock()
	stats := codexWebSocketState.stats[sessionID]
	if stats == nil {
		return OpenAICodexWebSocketDebugStats{}, false
	}
	copy := *stats
	if stats.LastDeltaInputItems != nil {
		value := *stats.LastDeltaInputItems
		copy.LastDeltaInputItems = &value
	}
	return copy, true
}

func ResetOpenAICodexWebSocketDebugStats(sessionID string) {
	codexWebSocketState.Lock()
	defer codexWebSocketState.Unlock()
	if sessionID != "" {
		delete(codexWebSocketState.stats, sessionID)
		delete(codexWebSocketState.fallback, sessionID)
		return
	}
	codexWebSocketState.stats = map[string]*OpenAICodexWebSocketDebugStats{}
	codexWebSocketState.fallback = map[string]struct{}{}
}

// CloseOpenAICodexWebSocketSessions mirrors pi's session-resource cleanup
// hook. Runtime/session disposal should call it with the durable session ID;
// an empty ID closes every cached Codex connection.
func CloseOpenAICodexWebSocketSessions(sessionID string) {
	codexWebSocketState.Lock()
	entries := make([]*codexWebSocketEntry, 0)
	if sessionID != "" {
		for _, entry := range codexWebSocketState.sessions[sessionID] {
			entries = append(entries, entry)
		}
		delete(codexWebSocketState.sessions, sessionID)
	} else {
		for _, accounts := range codexWebSocketState.sessions {
			for _, entry := range accounts {
				entries = append(entries, entry)
			}
		}
		codexWebSocketState.sessions = map[string]map[string]*codexWebSocketEntry{}
	}
	codexWebSocketState.Unlock()
	for _, entry := range entries {
		if entry.idleTimer != nil {
			entry.idleTimer.Stop()
		}
		entry.conn.CloseNow()
	}
}
