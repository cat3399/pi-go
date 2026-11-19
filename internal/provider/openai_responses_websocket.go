package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
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
)

const (
	openAIResponsesWebSocketIdleTTL         = 5 * time.Minute
	openAIResponsesWebSocketMaximumAge      = 55 * time.Minute
	defaultResponsesWebSocketConnectTimeout = 15 * time.Second
	openAIResponsesConnectionLimitCode      = "websocket_connection_limit_reached"
	openAIResponsesPreviousMissingCode      = "previous_response_not_found"
)

// responsesTransportConfig accepts an already encoded request and stream
// factories. Protocol encoding, authentication and SSE policies remain with
// the adapter; the transport owns only connections, continuation and fallback.
type responsesTransportConfig struct {
	ctx             context.Context
	endpoint        string
	payload         []byte
	model           Model
	client          HTTPDoer
	clock           Clock
	maxEventBytes   int
	options         StreamOptions
	scopeKey        string
	omitHTTPFields  bool
	normalizeEvent  func([]byte) []byte
	prepareHeaders  func(http.Header)
	validateHeaders func(http.Header) error
	newSSEStream    func() EventStream
	newStream       func(HTTPDoer) *openAIResponsesStream
}

func responsesWebSocketEnabled(options StreamOptions, compat *OpenAIResponsesCompat, defaultSupported bool) bool {
	if options.Transport == TransportSSE {
		return false
	}
	if compat != nil && compat.SupportsWebsockets != nil {
		return *compat.SupportsWebsockets
	}
	return defaultSupported || options.Transport == TransportWebsocket || options.Transport == TransportWebsocketCached
}

func (c responsesTransportConfig) newWebSocketStream() EventStream {
	stream := c.newStream(&responsesWebSocketDoer{config: c})
	stream.prepareHeaders = func(headers http.Header) {
		headers.Del("Accept")
		headers.Del("Content-Type")
		if c.prepareHeaders != nil {
			c.prepareHeaders(headers)
		}
		requestID := responsesCacheSessionID(c.options)
		if requestID == "" {
			requestID = newResponsesRequestID(c.clock())
		}
		headers.Set("x-client-request-id", requestID)
		headers.Set("session-id", requestID)
	}
	stream.validateHeaders = c.validateHeaders
	stream.onHeaders = c.options.OnHeaders
	stream.headerOverrides = cloneHeaderOverrides(c.options.HeaderOverrides)
	stream.terminalEndsStream = true
	return stream
}

func responsesTransportScope(endpoint string, model Model, key string, headers map[string]string, overrides map[string]*string) string {
	encoded, _ := json.Marshal([]any{endpoint, model.Provider(), model.API(), key, headers, overrides})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func responsesConnectionKey(c responsesTransportConfig, headers http.Header) string {
	encoded, _ := json.Marshal([]any{c.scopeKey, headers})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func durationFromMilliseconds(value uint64) time.Duration {
	if value > uint64((1<<63-1)/int64(time.Millisecond)) {
		return time.Duration(1<<63 - 1)
	}
	return time.Duration(value) * time.Millisecond
}

type openAIResponsesHybridStream struct {
	config responsesTransportConfig

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

func newOpenAIResponsesHybridStream(config responsesTransportConfig) EventStream {
	return &openAIResponsesHybridStream{config: config}
}

func (s *openAIResponsesHybridStream) Next() (llm.StreamEvent, error) {
	if s == nil {
		return nil, io.EOF
	}
	for {
		active, usingWS, closed := s.current()
		if closed || s.isTerminal() {
			return nil, io.EOF
		}
		if active == nil {
			sessionID := responsesCacheSessionID(s.config.options)
			if responsesWebSocketFallbackActive(sessionID, s.config.scopeKey) {
				recordResponsesWebSocketSSEFallback(sessionID, s.config.scopeKey)
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
			classification, code := classifyResponsesWebSocketFailure(cause)
			switch classification {
			case responsesWebSocketFailurePreviousMissing:
				if !s.previous {
					s.previous = true
					_ = active.Close()
					s.setActive(s.config.newWebSocketStream(), true, false)
					continue
				}
			case responsesWebSocketFailureConnectionLimit:
				if !s.limited {
					s.limited = true
					_ = active.Close()
					s.setActive(s.config.newWebSocketStream(), true, false)
					continue
				}
			case responsesWebSocketFailureAPI, responsesWebSocketFailureProtocol, responsesWebSocketFailureConfiguration:
				s.markTerminal()
				return rewriteResponsesWebSocketError(failed, classification, code), nil
			case responsesWebSocketFailureCancelled:
				s.markTerminal()
				return event, nil
			}
			s.fallback(active, cause)
			continue
		}
		if !usingWS {
			if responsesTerminalStreamEvent(event) {
				s.markTerminal()
			}
			return s.attachFallbackDiagnostic(event), nil
		}
		if responsesTerminalStreamEvent(event) {
			s.markTerminal()
		}
		return event, nil
	}
}

func (s *openAIResponsesHybridStream) current() (EventStream, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, s.usingWS, s.closed
}

func (s *openAIResponsesHybridStream) setActive(active EventStream, usingWS, started bool) {
	s.mu.Lock()
	s.active, s.usingWS, s.started = active, usingWS, started
	s.mu.Unlock()
}

func (s *openAIResponsesHybridStream) markStarted() {
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()
}

func (s *openAIResponsesHybridStream) hasStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

func (s *openAIResponsesHybridStream) isTerminal() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminal
}

func (s *openAIResponsesHybridStream) markTerminal() {
	s.mu.Lock()
	s.terminal = true
	s.mu.Unlock()
}

func responsesTerminalStreamEvent(event llm.StreamEvent) bool {
	switch event.(type) {
	case llm.DoneEvent, llm.ErrorEvent:
		return true
	default:
		return false
	}
}

func (s *openAIResponsesHybridStream) fallback(active EventStream, cause error) {
	var evidence []llm.AssistantDiagnostic
	if stream, ok := active.(*openAIResponsesStream); ok {
		evidence = stream.diagnostics.failure(FailureTransport, safeResponsesErrorText(cause, "WebSocket transport failed"), cause, "")
	}
	_ = active.Close()
	sessionID := responsesCacheSessionID(s.config.options)
	recordResponsesWebSocketFailure(sessionID, s.config.scopeKey, cause)
	diagnostic := responsesFallbackDiagnostic(s.config, cause, evidence)
	s.mu.Lock()
	s.diag = diagnostic
	s.active = s.config.newSSEStream()
	s.usingWS = false
	s.started = false
	s.mu.Unlock()
}

func (s *openAIResponsesHybridStream) attachFallbackDiagnostic(event llm.StreamEvent) llm.StreamEvent {
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

func (s *openAIResponsesHybridStream) Close() error {
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

func responsesFallbackDiagnostic(config responsesTransportConfig, cause error, evidence []llm.AssistantDiagnostic) *llm.AssistantDiagnostic {
	transport := config.options.Transport
	if transport == "" {
		transport = TransportAuto
	}
	detailFields := map[string]any{}
	if len(evidence) != 0 {
		_ = json.Unmarshal(evidence[0].Details(), &detailFields)
	}
	for key, value := range map[string]any{
		"configuredTransport": transport, "fallbackTransport": "sse", "eventsEmitted": false,
		"phase": "before_message_stream_start", "requestBytes": len(config.payload),
	} {
		detailFields[key] = value
	}
	details, _ := json.Marshal(detailFields)
	timestamp := config.clock().UTC().Truncate(time.Millisecond)
	if timestamp.IsZero() {
		timestamp = time.UnixMilli(1).UTC()
	}
	message := "OpenAI Responses WebSocket transport failed"
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

type responsesWebSocketFailureClass uint8

const (
	responsesWebSocketFailureTransport responsesWebSocketFailureClass = iota + 1
	responsesWebSocketFailureAPI
	responsesWebSocketFailureProtocol
	responsesWebSocketFailureConfiguration
	responsesWebSocketFailureConnectionLimit
	responsesWebSocketFailurePreviousMissing
	responsesWebSocketFailureCancelled
)

type responsesWebSocketAPIError struct{ code, message string }

func (e *responsesWebSocketAPIError) Error() string {
	if e == nil {
		return "OpenAI Responses WebSocket API error"
	}
	if e.message != "" {
		return e.message
	}
	return e.code
}

type responsesWebSocketProtocolError struct{ message string }

func (e *responsesWebSocketProtocolError) Error() string { return e.message }

type responsesWebSocketConfigurationError struct{ message string }

func (e *responsesWebSocketConfigurationError) Error() string { return e.message }

func classifyResponsesWebSocketFailure(cause error) (responsesWebSocketFailureClass, string) {
	if errors.Is(cause, context.Canceled) {
		return responsesWebSocketFailureCancelled, ""
	}
	var apiError *responsesWebSocketAPIError
	if errors.As(cause, &apiError) {
		switch apiError.code {
		case openAIResponsesConnectionLimitCode:
			return responsesWebSocketFailureConnectionLimit, apiError.code
		case openAIResponsesPreviousMissingCode:
			return responsesWebSocketFailurePreviousMissing, apiError.code
		default:
			return responsesWebSocketFailureAPI, apiError.code
		}
	}
	var protocolError *responsesWebSocketProtocolError
	if errors.As(cause, &protocolError) {
		return responsesWebSocketFailureProtocol, ""
	}
	var configurationError *responsesWebSocketConfigurationError
	if errors.As(cause, &configurationError) {
		return responsesWebSocketFailureConfiguration, ""
	}
	var failure *ProviderFailure
	if errors.As(cause, &failure) && (failure.Kind() == FailureConfiguration || failure.Kind() == FailureInvalidRequest) {
		return responsesWebSocketFailureConfiguration, ""
	}
	return responsesWebSocketFailureTransport, ""
}

func rewriteResponsesWebSocketError(event llm.ErrorEvent, classification responsesWebSocketFailureClass, code string) llm.ErrorEvent {
	kind := FailureInvalidResponse
	if classification == responsesWebSocketFailureConfiguration {
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

type responsesWebSocketDoer struct{ config responsesTransportConfig }

func (d *responsesWebSocketDoer) Do(request *http.Request) (*http.Response, error) {
	if d == nil {
		return nil, errors.New("OpenAI Responses WebSocket transport is nil")
	}
	config := d.config
	headers := request.Header.Clone()

	var fullBody map[string]any
	if err := json.Unmarshal(config.payload, &fullBody); err != nil || fullBody == nil {
		return nil, &responsesWebSocketProtocolError{message: "OpenAI Responses WebSocket request payload is invalid"}
	}
	if config.omitHTTPFields {
		if background := fullBody["background"]; background != nil && background != false {
			return nil, &responsesWebSocketConfigurationError{message: "OpenAI Responses WebSocket does not support background requests"}
		}
	}
	useCachedContext := config.options.Transport == TransportWebsocketCached || config.options.Transport == TransportAuto || config.options.Transport == ""
	lease, err := acquireResponsesWebSocket(request.Context(), config, headers)
	if err != nil {
		return nil, err
	}
	requestBody := fullBody
	if useCachedContext && lease.entry != nil {
		requestBody = buildCachedResponsesWebSocketRequest(lease.entry, fullBody)
	}
	frame := map[string]any{"type": "response.create"}
	for key, value := range requestBody {
		if config.omitHTTPFields && (key == "stream" || key == "background") {
			continue
		}
		frame[key] = value
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		lease.release(false)
		return nil, &responsesWebSocketProtocolError{message: "encode OpenAI Responses WebSocket request: " + err.Error()}
	}
	if err := lease.conn.Write(request.Context(), websocket.MessageText, encoded); err != nil {
		lease.release(false)
		return nil, fmt.Errorf("write OpenAI Responses WebSocket request: %w", err)
	}
	captureFromContext(request.Context()).request(encoded)
	recordResponsesWebSocketRequest(lease, requestBody, useCachedContext)
	first, err := readFirstResponsesWebSocketEvent(request.Context(), lease.conn, config.options, config.maxEventBytes)
	if err != nil {
		if lease.entry != nil {
			lease.entry.clearContinuation()
		}
		lease.release(false)
		return nil, err
	}
	reader, writer := io.Pipe()
	body := &responsesWebSocketBody{
		reader: reader, writer: writer, ctx: request.Context(), lease: lease, first: first,
		normalizeEvent: config.normalizeEvent, maxEventBytes: config.maxEventBytes, fullBody: cloneJSONMap(fullBody), useCachedContext: useCachedContext,
	}
	go body.run()
	return &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: body, Request: request,
	}, nil
}

func readFirstResponsesWebSocketEvent(ctx context.Context, conn *websocket.Conn, options StreamOptions, maxBytes int) ([]byte, error) {
	for {
		data, err := readResponsesWebSocketMessage(ctx, conn, options.TimeoutMS)
		if err != nil {
			return nil, fmt.Errorf("read first OpenAI Responses WebSocket event: %w", err)
		}
		if maxBytes > 0 && len(data) > maxBytes {
			return nil, &responsesWebSocketProtocolError{message: "OpenAI Responses WebSocket event exceeds the configured size limit"}
		}
		typeName, apiError, protocolError := inspectResponsesWebSocketEvent(data)
		if protocolError != nil {
			return nil, protocolError
		}
		if typeName == "" {
			continue
		}
		if apiError != nil {
			return nil, apiError
		}
		return data, nil
	}
}

func readResponsesWebSocketMessage(ctx context.Context, conn *websocket.Conn, timeoutMS *uint64) ([]byte, error) {
	readContext := ctx
	cancel := func() {}
	if timeoutMS != nil && *timeoutMS != 0 {
		readContext, cancel = context.WithTimeout(ctx, durationFromMilliseconds(*timeoutMS))
	}
	defer cancel()
	_, data, err := conn.Read(readContext)
	captureFromContext(ctx).write(data, err, true)
	return data, err
}

func inspectResponsesWebSocketEvent(data []byte) (string, *responsesWebSocketAPIError, *responsesWebSocketProtocolError) {
	if !utf8.Valid(data) {
		return "", nil, &responsesWebSocketProtocolError{message: "OpenAI Responses WebSocket event is not valid UTF-8"}
	}
	var event map[string]any
	if err := json.Unmarshal(data, &event); err != nil || event == nil {
		if err == nil {
			err = errors.New("top-level event is not an object")
		}
		return "", nil, &responsesWebSocketProtocolError{message: "Invalid OpenAI Responses WebSocket JSON: " + err.Error()}
	}
	typeName, _ := event["type"].(string)
	if typeName == "error" {
		code, message := responsesWebSocketEventError(event)
		return typeName, &responsesWebSocketAPIError{code: code, message: firstNonBlank(message, code, "OpenAI Responses WebSocket returned an error")}, nil
	}
	if typeName == "response.failed" {
		response, _ := event["response"].(map[string]any)
		nested, _ := response["error"].(map[string]any)
		code, _ := nested["code"].(string)
		message, _ := nested["message"].(string)
		return typeName, &responsesWebSocketAPIError{code: code, message: firstNonBlank(message, code, "OpenAI Responses response failed")}, nil
	}
	return typeName, nil, nil
}

func responsesWebSocketEventError(event map[string]any) (string, string) {
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
	return "OpenAI Responses error"
}

type responsesWebSocketBody struct {
	reader *io.PipeReader
	writer *io.PipeWriter
	ctx    context.Context
	lease  *responsesWebSocketLease
	first  []byte

	normalizeEvent   func([]byte) []byte
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

func (b *responsesWebSocketBody) Read(buffer []byte) (int, error) { return b.reader.Read(buffer) }

func (b *responsesWebSocketBody) Close() error {
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

func (b *responsesWebSocketBody) run() {
	defer b.writer.Close()
	data := b.first
	for {
		typeName, apiError, protocolError := inspectResponsesWebSocketEvent(data)
		if protocolError != nil {
			b.markFailed()
			_ = b.writer.CloseWithError(protocolError)
			return
		}
		if typeName == "" {
			var err error
			data, err = readResponsesWebSocketMessage(b.ctx, b.lease.conn, b.lease.options.TimeoutMS)
			if err != nil {
				b.failWriter(fmt.Errorf("read OpenAI Responses WebSocket event: %w", err))
				return
			}
			continue
		}
		if apiError != nil {
			b.markFailed()
		}
		if b.normalizeEvent != nil {
			data = b.normalizeEvent(data)
		}
		b.observeOutputItem(data)
		if b.maxEventBytes > 0 && len(data) > b.maxEventBytes {
			b.failWriter(&responsesWebSocketProtocolError{message: "OpenAI Responses WebSocket event exceeds the configured size limit"})
			return
		}
		if _, err := b.writer.Write(append(append([]byte("data: "), data...), '\n', '\n')); err != nil {
			b.markFailed()
			return
		}
		if apiError != nil {
			return
		}
		if responsesWebSocketTerminalEvent(typeName) {
			b.observeTerminal(data)
			return
		}
		var err error
		data, err = readResponsesWebSocketMessage(b.ctx, b.lease.conn, b.lease.options.TimeoutMS)
		if err != nil {
			b.failWriter(fmt.Errorf("read OpenAI Responses WebSocket event: %w", err))
			return
		}
	}
}

func (b *responsesWebSocketBody) failWriter(err error) {
	b.markFailed()
	_ = b.writer.CloseWithError(err)
}

func (b *responsesWebSocketBody) markFailed() {
	b.mu.Lock()
	b.failed = true
	b.mu.Unlock()
	if b.lease.entry != nil {
		b.lease.entry.clearContinuation()
	}
}

func (b *responsesWebSocketBody) observeTerminal(_ []byte) {
	b.mu.Lock()
	b.terminal = true
	b.mu.Unlock()
	// The Responses parser commits the connection/continuation only after it has
	// validated this terminal event. Merely observing terminal bytes is not enough
	// to make the connection reusable.
}

func (b *responsesWebSocketBody) commitResponsesTerminal(responseID string) {
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
		canonical, ok := canonicalResponsesContinuationItem(item)
		if !ok {
			entry.clearContinuation()
			return
		}
		items = append(items, canonical)
	}
	entry.setContinuation(responsesWebSocketContinuation{
		lastRequestBody: cloneJSONMap(b.fullBody), lastResponseID: responseID, lastResponseItems: cloneJSONSlice(items),
	})
}

// canonicalResponsesContinuationItem mirrors the Responses replay converter used
// for the normalized final assistant message. output_item.done is the only
// authoritative wire source available to the WebSocket body, and its raw item
// may contain status/future fields or multipart summaries that the next
// request's converter intentionally rewrites. Canonicalizing here makes the
// cached baseline byte-for-value equivalent to that next request. Reasoning is
// the exception: its opaque signature is the complete output item, so preserve
// all raw fields and apply only the same legacy-session repair as replay.
func canonicalResponsesContinuationItem(value any) (any, bool) {
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
		reasoning, ok := decodeResponsesReasoningSignature(string(raw))
		if !ok {
			return nil, false
		}
		var result any
		if json.Unmarshal(reasoning, &result) != nil {
			return nil, false
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

func (b *responsesWebSocketBody) observeOutputItem(data []byte) {
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

func responsesWebSocketTerminalEvent(typeName string) bool {
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

type responsesWebSocketContinuation struct {
	lastRequestBody   map[string]any
	lastResponseID    string
	lastResponseItems []any
}

type responsesWebSocketEntry struct {
	mu           sync.Mutex
	conn         *websocket.Conn
	busy         bool
	createdAt    time.Time
	idleTimer    *time.Timer
	continuation *responsesWebSocketContinuation
}

func (e *responsesWebSocketEntry) clearContinuation() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.continuation = nil
	e.mu.Unlock()
}

func (e *responsesWebSocketEntry) setContinuation(value responsesWebSocketContinuation) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.continuation = &value
	e.mu.Unlock()
}

type responsesWebSocketLease struct {
	conn          *websocket.Conn
	entry         *responsesWebSocketEntry
	sessionID     string
	connectionKey string
	reused        bool
	temporary     bool
	options       StreamOptions
	once          sync.Once
}

func (l *responsesWebSocketLease) release(keep bool) {
	if l == nil {
		return
	}
	l.once.Do(func() { releaseResponsesWebSocket(l, keep) })
}

type OpenAIResponsesWebSocketDebugStats struct {
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

var responsesWebSocketState = struct {
	sync.Mutex
	sessions map[string]map[string]*responsesWebSocketEntry
	fallback map[string]map[string]struct{}
	stats    map[string]*OpenAIResponsesWebSocketDebugStats
}{sessions: map[string]map[string]*responsesWebSocketEntry{}, fallback: map[string]map[string]struct{}{}, stats: map[string]*OpenAIResponsesWebSocketDebugStats{}}

func acquireResponsesWebSocket(ctx context.Context, config responsesTransportConfig, headers http.Header) (*responsesWebSocketLease, error) {
	sessionID := responsesCacheSessionID(config.options)
	connectionKey := responsesConnectionKey(config, headers)
	if sessionID != "" {
		responsesWebSocketState.Lock()
		entry := responsesWebSocketState.sessions[sessionID][connectionKey]
		if entry != nil && !entry.busy && config.clock().Sub(entry.createdAt) < openAIResponsesWebSocketMaximumAge {
			entry.busy = true
			if entry.idleTimer != nil {
				entry.idleTimer.Stop()
				entry.idleTimer = nil
			}
			responsesWebSocketState.Unlock()
			entry.conn.SetReadLimit(int64(config.maxEventBytes))
			return &responsesWebSocketLease{conn: entry.conn, entry: entry, sessionID: sessionID, connectionKey: connectionKey, reused: true, options: config.options}, nil
		}
		if entry != nil && !entry.busy {
			delete(responsesWebSocketState.sessions[sessionID], connectionKey)
			if len(responsesWebSocketState.sessions[sessionID]) == 0 {
				delete(responsesWebSocketState.sessions, sessionID)
			}
			entry.conn.CloseNow()
		}
		busy := entry != nil && entry.busy
		responsesWebSocketState.Unlock()
		conn, err := dialResponsesWebSocket(ctx, config, headers)
		if err != nil {
			return nil, err
		}
		if busy {
			return &responsesWebSocketLease{conn: conn, sessionID: sessionID, connectionKey: connectionKey, temporary: true, options: config.options}, nil
		}
		entry = &responsesWebSocketEntry{conn: conn, busy: true, createdAt: config.clock()}
		responsesWebSocketState.Lock()
		connections := responsesWebSocketState.sessions[sessionID]
		if connections == nil {
			connections = map[string]*responsesWebSocketEntry{}
			responsesWebSocketState.sessions[sessionID] = connections
		}
		if existing := connections[connectionKey]; existing != nil {
			responsesWebSocketState.Unlock()
			return &responsesWebSocketLease{conn: conn, sessionID: sessionID, connectionKey: connectionKey, temporary: true, options: config.options}, nil
		}
		connections[connectionKey] = entry
		responsesWebSocketState.Unlock()
		return &responsesWebSocketLease{conn: conn, entry: entry, sessionID: sessionID, connectionKey: connectionKey, options: config.options}, nil
	}
	conn, err := dialResponsesWebSocket(ctx, config, headers)
	if err != nil {
		return nil, err
	}
	return &responsesWebSocketLease{conn: conn, connectionKey: connectionKey, temporary: true, options: config.options}, nil
}

func dialResponsesWebSocket(ctx context.Context, config responsesTransportConfig, headers http.Header) (*websocket.Conn, error) {
	websocketURL, err := responsesWebSocketEndpoint(config.endpoint)
	if err != nil {
		return nil, &responsesWebSocketConfigurationError{message: err.Error()}
	}
	connectContext := ctx
	cancel := func() {}
	timeout := defaultResponsesWebSocketConnectTimeout
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
	if captureFromContext(ctx) != nil {
		dialOptions.HTTPClient = diagnosticWebSocketClient(dialOptions.HTTPClient)
	}
	conn, response, err := websocket.Dial(connectContext, websocketURL, dialOptions)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("connect OpenAI Responses WebSocket: %w", err)
	}
	conn.SetReadLimit(int64(config.maxEventBytes))
	return conn, nil
}

func responsesWebSocketEndpoint(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("%w: invalid Responses WebSocket endpoint", ErrInvalidOpenAIResponsesConfig)
	}
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	default:
		return "", fmt.Errorf("%w: invalid Responses WebSocket endpoint scheme", ErrInvalidOpenAIResponsesConfig)
	}
	return parsed.String(), nil
}

func releaseResponsesWebSocket(lease *responsesWebSocketLease, keep bool) {
	if lease == nil || lease.conn == nil {
		return
	}
	if lease.temporary || lease.entry == nil || !keep {
		responsesWebSocketState.Lock()
		if lease.entry != nil {
			if connections := responsesWebSocketState.sessions[lease.sessionID]; connections != nil && connections[lease.connectionKey] == lease.entry {
				delete(connections, lease.connectionKey)
				if len(connections) == 0 {
					delete(responsesWebSocketState.sessions, lease.sessionID)
				}
			}
		}
		responsesWebSocketState.Unlock()
		lease.conn.CloseNow()
		return
	}
	responsesWebSocketState.Lock()
	connections := responsesWebSocketState.sessions[lease.sessionID]
	if connections == nil || connections[lease.connectionKey] != lease.entry {
		responsesWebSocketState.Unlock()
		lease.conn.CloseNow()
		return
	}
	lease.entry.busy = false
	entry := lease.entry
	entry.idleTimer = time.AfterFunc(openAIResponsesWebSocketIdleTTL, func() {
		responsesWebSocketState.Lock()
		connections := responsesWebSocketState.sessions[lease.sessionID]
		if connections != nil && connections[lease.connectionKey] == entry && !entry.busy {
			delete(connections, lease.connectionKey)
			if len(connections) == 0 {
				delete(responsesWebSocketState.sessions, lease.sessionID)
			}
			responsesWebSocketState.Unlock()
			entry.conn.CloseNow()
			return
		}
		responsesWebSocketState.Unlock()
	})
	responsesWebSocketState.Unlock()
}

func buildCachedResponsesWebSocketRequest(entry *responsesWebSocketEntry, body map[string]any) map[string]any {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.continuation == nil {
		return body
	}
	continuation := entry.continuation
	if !responsesBodiesMatchExceptInput(body, continuation.lastRequestBody) {
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

func responsesBodiesMatchExceptInput(left, right map[string]any) bool {
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

func responsesCacheSessionID(options StreamOptions) string {
	if resolveOpenAICacheRetention(options) == CacheRetentionNone || options.SessionID == "" {
		return ""
	}
	return clampOpenAIPromptCacheKey(options.SessionID)
}

func newResponsesRequestID(now time.Time) string {
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

func recordResponsesWebSocketRequest(lease *responsesWebSocketLease, body map[string]any, cached bool) {
	if lease == nil || lease.sessionID == "" {
		return
	}
	responsesWebSocketState.Lock()
	stats := responsesStatsLocked(lease.sessionID)
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
	responsesWebSocketState.Unlock()
}

func responsesStatsLocked(sessionID string) *OpenAIResponsesWebSocketDebugStats {
	stats := responsesWebSocketState.stats[sessionID]
	if stats == nil {
		stats = &OpenAIResponsesWebSocketDebugStats{}
		responsesWebSocketState.stats[sessionID] = stats
	}
	return stats
}

func responsesWebSocketFallbackActive(sessionID, scopeKey string) bool {
	if sessionID == "" {
		return false
	}
	responsesWebSocketState.Lock()
	_, active := responsesWebSocketState.fallback[sessionID][scopeKey]
	responsesWebSocketState.Unlock()
	return active
}

func recordResponsesWebSocketFailure(sessionID, scopeKey string, cause error) {
	if sessionID == "" {
		return
	}
	responsesWebSocketState.Lock()
	if responsesWebSocketState.fallback[sessionID] == nil {
		responsesWebSocketState.fallback[sessionID] = make(map[string]struct{})
	}
	responsesWebSocketState.fallback[sessionID][scopeKey] = struct{}{}
	stats := responsesStatsLocked(sessionID)
	stats.WebSocketFailures++
	stats.WebSocketFallbackActive = true
	if cause != nil {
		stats.LastWebSocketError = cause.Error()
	}
	responsesWebSocketState.Unlock()
}

func recordResponsesWebSocketSSEFallback(sessionID, scopeKey string) {
	if sessionID == "" {
		return
	}
	responsesWebSocketState.Lock()
	stats := responsesStatsLocked(sessionID)
	stats.SSEFallbacks++
	_, stats.WebSocketFallbackActive = responsesWebSocketState.fallback[sessionID][scopeKey]
	responsesWebSocketState.Unlock()
}

func GetOpenAIResponsesWebSocketDebugStats(sessionID string) (OpenAIResponsesWebSocketDebugStats, bool) {
	responsesWebSocketState.Lock()
	defer responsesWebSocketState.Unlock()
	stats := responsesWebSocketState.stats[sessionID]
	if stats == nil {
		return OpenAIResponsesWebSocketDebugStats{}, false
	}
	copy := *stats
	if stats.LastDeltaInputItems != nil {
		value := *stats.LastDeltaInputItems
		copy.LastDeltaInputItems = &value
	}
	return copy, true
}

func ResetOpenAIResponsesWebSocketDebugStats(sessionID string) {
	responsesWebSocketState.Lock()
	defer responsesWebSocketState.Unlock()
	if sessionID != "" {
		delete(responsesWebSocketState.stats, sessionID)
		delete(responsesWebSocketState.fallback, sessionID)
		return
	}
	responsesWebSocketState.stats = map[string]*OpenAIResponsesWebSocketDebugStats{}
	responsesWebSocketState.fallback = map[string]map[string]struct{}{}
}

// CloseOpenAIResponsesWebSocketSessions releases cached transport resources.
// An empty ID closes every cached Responses connection.
func CloseOpenAIResponsesWebSocketSessions(sessionID string) {
	responsesWebSocketState.Lock()
	entries := make([]*responsesWebSocketEntry, 0)
	if sessionID != "" {
		for _, entry := range responsesWebSocketState.sessions[sessionID] {
			entries = append(entries, entry)
		}
		delete(responsesWebSocketState.sessions, sessionID)
	} else {
		for _, connections := range responsesWebSocketState.sessions {
			for _, entry := range connections {
				entries = append(entries, entry)
			}
		}
		responsesWebSocketState.sessions = map[string]map[string]*responsesWebSocketEntry{}
	}
	responsesWebSocketState.Unlock()
	for _, entry := range entries {
		if entry.idleTimer != nil {
			entry.idleTimer.Stop()
		}
		entry.conn.CloseNow()
	}
}
