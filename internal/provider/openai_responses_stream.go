package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/llm"
)

type responsesFailureSpec struct {
	kind        FailureKind
	cause       error
	message     string
	httpStatus  *int
	vendorCode  string
	retryAfter  *time.Duration
	shouldRetry *bool
}

type responsesTextSlot struct {
	contentIndex int
	itemID       string
	phase        string
	text         strings.Builder
}

type responsesToolSlot struct {
	contentIndex   int
	itemID         string
	callID         string
	name           string
	arguments      []byte
	argumentsDone  bool
	customProperty string
	customCurrent  string
	customEncoded  string
	customStarted  bool
	customClosed   bool
}
type responsesReasoningSlot struct {
	contentIndex int
	itemID       string
	text         strings.Builder
	summaryParts map[int]*strings.Builder
}

type responsesCompletedReasoning struct {
	contentIndex int
	itemID       string
	text         string
	rawItem      json.RawMessage
}

type responsesDeferredEvent struct {
	event          llm.StreamEvent
	reasoningIndex int
	reasoningEnd   bool
}

type openAIResponsesStream struct {
	ctx                     context.Context
	cancel                  context.CancelCauseFunc
	timeoutCancel           context.CancelFunc
	endpoint                string
	apiKey                  string
	authHeader              string
	displayName             string
	configurationError      error
	client                  HTTPDoer
	clock                   Clock
	timestamp               time.Time
	payload                 []byte
	model                   Model
	headers                 map[string]string
	maxEventBytes           int
	maxErrorBodyBytes       int
	onResponse              ResponseHook
	onHeaders               HeaderHook
	headerOverrides         map[string]*string
	maxRetries              uint32
	maxRetryDelayMS         *uint64
	serviceTier             string
	applyServiceTierPricing bool
	codexServiceTier        bool
	codexRetry              bool
	terminalEndsStream      bool
	grammarProperties       map[string]string
	configurationFail       *responsesFailureSpec
	preflight               *responsesFailureSpec

	lifecycleMu sync.Mutex
	body        io.ReadCloser
	bodyClosed  bool
	closeErr    error
	closed      bool
	finished    bool

	initialized      bool
	started          bool
	decoder          *responsesSSEDecoder
	queue            []llm.StreamEvent
	slots            map[int]*responsesTextSlot
	reasoningSlots   map[int]*responsesReasoningSlot
	toolSlots        map[int]*responsesToolSlot
	completedOutputs map[int]struct{}
	completedItemIDs map[int]string
	completedPhases  map[int]string
	pendingReasoning map[int]*responsesCompletedReasoning
	deferredEvents   []responsesDeferredEvent
	deferOutput      bool
	nextContentIndex int
	nextOutputIndex  int
	sawFunctionCall  bool
	sawFinalAnswer   bool
	pendingDone      *llm.DoneEvent
}

type responsesTerminalCommitter interface {
	commitResponsesTerminal(responseID string)
}

// commitResponsesTerminal is deliberately invoked only after all terminal
// response validation and usage normalization succeeds. WebSocket-cached Codex
// must not retain a connection or continuation for a response the adapter
// ultimately rejects.
func (s *openAIResponsesStream) commitResponsesTerminal(responseID string) {
	s.lifecycleMu.Lock()
	body := s.body
	s.lifecycleMu.Unlock()
	if committer, ok := body.(responsesTerminalCommitter); ok {
		committer.commitResponsesTerminal(responseID)
	}
}

func newResponsesFailureStream(
	ctx context.Context,
	clock Clock,
	model Model,
	kind FailureKind,
	cause error,
	message string,
) EventStream {
	if ctx == nil {
		ctx = context.Background()
	}
	if clock == nil {
		clock = time.Now
	}
	streamContext, cancel := context.WithCancelCause(ctx)
	return &openAIResponsesStream{
		ctx:       streamContext,
		cancel:    cancel,
		clock:     clock,
		timestamp: clock(),
		model:     model,
		preflight: &responsesFailureSpec{
			kind:    kind,
			cause:   cause,
			message: message,
		},
		slots:            make(map[int]*responsesTextSlot),
		reasoningSlots:   make(map[int]*responsesReasoningSlot),
		toolSlots:        make(map[int]*responsesToolSlot),
		completedOutputs: make(map[int]struct{}),
		completedItemIDs: make(map[int]string),
		completedPhases:  make(map[int]string),
		pendingReasoning: make(map[int]*responsesCompletedReasoning),
	}
}

func (s *openAIResponsesStream) Next() (event llm.StreamEvent, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			cause := &responsesStreamPanicError{valueType: fmt.Sprintf("%T", recovered), stack: debug.Stack()}
			if recoveredError, ok := recovered.(error); ok {
				cause.cause = recoveredError
			}
			if s.isClosedOrFinished() {
				event = nil
				err = closedStreamError(cause)
				return
			}
			event, err = s.finishFailure(responsesFailureSpec{
				kind:    FailureInvalidResponse,
				cause:   cause,
				message: "OpenAI Responses stream panicked",
			})
		}
	}()

	if s.isClosedOrFinished() {
		return nil, io.EOF
	}
	if cause := context.Cause(s.ctx); cause != nil {
		return s.finishCancellation(cause)
	}
	if len(s.queue) != 0 {
		return s.popEvent(), nil
	}
	if s.terminalEndsStream && s.pendingDone != nil {
		done := *s.pendingDone
		s.finishTransport()
		return done, nil
	}
	if s.preflight != nil {
		spec := *s.preflight
		s.preflight = nil
		return s.finishFailure(spec)
	}
	if !s.initialized {
		s.initialized = true
		if failure := s.initialize(); failure != nil {
			if s.isClosed() {
				return nil, io.EOF
			}
			return s.finishFailure(*failure)
		}
		if s.isClosed() {
			return nil, io.EOF
		}
		if cause := context.Cause(s.ctx); cause != nil {
			return s.finishCancellation(cause)
		}
	}
	if !s.started {
		s.started = true
		return llm.NewStartEvent(assistantProvenanceForModel(s.model), s.timestamp)
	}

	for {
		if cause := context.Cause(s.ctx); cause != nil {
			return s.finishCancellation(cause)
		}
		data, readErr := s.decoder.NextData()
		if readErr != nil {
			if s.isClosed() {
				return nil, io.EOF
			}
			if cause := context.Cause(s.ctx); cause != nil {
				return s.finishCancellation(cause)
			}
			if errors.Is(readErr, io.EOF) {
				if s.pendingDone != nil {
					done := *s.pendingDone
					s.finishTransport()
					return done, nil
				}
				readErr = fmt.Errorf("%w: stream ended before a terminal response event", ErrOpenAIResponsesStream)
				return s.finishFailure(responsesFailureSpec{
					kind:    FailureInvalidResponse,
					cause:   readErr,
					message: safeResponsesErrorText(readErr, "OpenAI Responses stream failed"),
				})
			}
			if errors.Is(readErr, errResponsesEventTooLarge) || errors.Is(readErr, errResponsesIncompleteFrame) {
				readErr = fmt.Errorf("%w: read SSE: %w", ErrOpenAIResponsesStream, readErr)
				return s.finishFailure(responsesFailureSpec{
					kind:    FailureInvalidResponse,
					cause:   readErr,
					message: safeResponsesErrorText(readErr, "OpenAI Responses stream failed"),
				})
			}
			readErr = fmt.Errorf("%w: read SSE transport: %w", ErrOpenAIResponsesStream, readErr)
			return s.finishFailure(responsesFailureSpec{
				kind:    FailureTransport,
				cause:   readErr,
				message: safeResponsesErrorText(readErr, "OpenAI Responses stream failed"),
			})
		}
		if cause := context.Cause(s.ctx); cause != nil {
			return s.finishCancellation(cause)
		}
		if !utf8.Valid(data) {
			cause := fmt.Errorf("%w: SSE data is not valid UTF-8", ErrOpenAIResponsesStream)
			return s.finishFailure(responsesFailureSpec{
				kind:    FailureInvalidResponse,
				cause:   cause,
				message: cause.Error(),
			})
		}
		if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			if s.pendingDone != nil {
				done := *s.pendingDone
				s.finishTransport()
				return done, nil
			}
			cause := fmt.Errorf("%w: DONE arrived before a terminal response event", ErrOpenAIResponsesStream)
			return s.finishFailure(responsesFailureSpec{
				kind:    FailureInvalidResponse,
				cause:   cause,
				message: cause.Error(),
			})
		}
		if failure := s.processResponsesEvent(data); failure != nil {
			return s.finishFailure(*failure)
		}
		if cause := context.Cause(s.ctx); cause != nil {
			return s.finishCancellation(cause)
		}
		if len(s.queue) != 0 {
			return s.popEvent(), nil
		}
		if s.terminalEndsStream && s.pendingDone != nil {
			done := *s.pendingDone
			s.finishTransport()
			return done, nil
		}
	}
}

func (s *openAIResponsesStream) initialize() (failure *responsesFailureSpec) {
	for retryIndex := uint32(0); ; retryIndex++ {
		failure = s.initializeAttempt()
		if failure == nil {
			return nil
		}
		shouldRetry := providerShouldRetry(failure.kind, failure.httpStatus, failure.shouldRetry)
		if s.codexRetry {
			shouldRetry = codexShouldRetry(failure)
		}
		if retryIndex >= s.maxRetries || !shouldRetry {
			return failure
		}
		wait := waitProviderRetry
		if s.codexRetry {
			wait = waitCodexRetry
		}
		if err := wait(s.ctx, retryIndex, failure.retryAfter, s.maxRetryDelayMS, failure.message); err != nil {
			if retryWaitCancellation(err) {
				return s.cancellationFailure(err)
			}
			failure.cause = errors.Join(failure.cause, err)
			failure.message = safeResponsesErrorText(err, failure.message)
			return failure
		}
	}
}

func (s *openAIResponsesStream) initializeAttempt() (failure *responsesFailureSpec) {
	var ownedBody io.ReadCloser
	defer func() {
		if ownedBody == nil {
			return
		}
		if closeErr := safeCloseResponsesBody(ownedBody); closeErr != nil && failure != nil {
			failure.cause = errors.Join(failure.cause, closeErr)
		}
	}()

	request, err := http.NewRequestWithContext(s.ctx, http.MethodPost, s.endpoint, bytes.NewReader(s.payload))
	if err != nil {
		return &responsesFailureSpec{
			kind:    FailureInvalidRequest,
			cause:   fmt.Errorf("%w: construct HTTP request: %w", ErrOpenAIResponsesRequest, err),
			message: "Could not construct OpenAI Responses request",
		}
	}
	if validBearerAPIKey(s.apiKey) {
		if s.authHeader == "api-key" {
			request.Header.Set("api-key", s.apiKey)
		} else {
			request.Header.Set("Authorization", "Bearer "+s.apiKey)
		}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	for name, value := range s.headers {
		request.Header.Set(name, value)
	}
	if err := applyFinalHeaders(request.Header, s.model, s.onHeaders, s.headerOverrides); err != nil {
		return &responsesFailureSpec{kind: FailureInvalidRequest, cause: err, message: "OpenAI Responses header hook failed"}
	}
	authorized := openAIHTTPHeadersHaveAuthorization(request.Header)
	if s.authHeader == "api-key" {
		authorized = strings.TrimSpace(request.Header.Get("api-key")) != ""
	}
	if !authorized {
		if s.configurationFail != nil {
			spec := *s.configurationFail
			return &spec
		}
		return &responsesFailureSpec{
			kind:    FailureConfiguration,
			cause:   fmt.Errorf("%w: final %s header is missing", s.configurationError, s.authHeader),
			message: s.displayName + " API authorization was removed before the request",
		}
	}

	response, err := invokeResponsesHTTPDoer(s.client, request)
	if response != nil && response.Body != nil && !isTypedNil(response.Body) {
		ownedBody = response.Body
	}
	if err != nil {
		if cause := context.Cause(s.ctx); cause != nil {
			return s.cancellationFailure(cause)
		}
		cause := fmt.Errorf("%w: HTTP request: %w", ErrOpenAIResponsesStream, err)
		return &responsesFailureSpec{
			kind:    FailureTransport,
			cause:   cause,
			message: safeResponsesErrorText(err, "OpenAI Responses transport failed"),
		}
	}
	if response == nil || response.Body == nil || isTypedNil(response.Body) {
		cause := fmt.Errorf("%w: HTTP client returned a nil response or body", ErrOpenAIResponsesStream)
		return &responsesFailureSpec{kind: FailureInvalidResponse, cause: cause, message: cause.Error()}
	}
	if s.onResponse != nil {
		if err := s.onResponse(s.model, responseInfo(response)); err != nil {
			return &responsesFailureSpec{kind: FailureInvalidResponse, cause: fmt.Errorf("response hook: %w", err), message: "OpenAI Responses response hook rejected the response"}
		}
	}
	if response.StatusCode < 100 || response.StatusCode > 599 {
		cause := fmt.Errorf(
			"%w: HTTP client returned invalid status code %d",
			ErrOpenAIResponsesStream,
			response.StatusCode,
		)
		return &responsesFailureSpec{kind: FailureInvalidResponse, cause: cause, message: cause.Error()}
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return s.httpStatusFailure(response)
	}
	mediaType, _, contentTypeErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.Header.Get("Content-Type") != "" && (contentTypeErr != nil || !strings.EqualFold(mediaType, "text/event-stream")) {
		cause := fmt.Errorf(
			"%w: response content type %q is not text/event-stream",
			ErrOpenAIResponsesStream,
			response.Header.Get("Content-Type"),
		)
		return &responsesFailureSpec{kind: FailureInvalidResponse, cause: cause, message: cause.Error()}
	}

	s.lifecycleMu.Lock()
	if s.closed {
		s.lifecycleMu.Unlock()
		return s.cancellationFailure(errOpenAIResponsesStreamClosed)
	}
	s.body = response.Body
	ownedBody = nil
	s.lifecycleMu.Unlock()
	s.decoder = newResponsesSSEDecoder(response.Body, s.maxEventBytes)
	return nil
}

func (s *openAIResponsesStream) httpStatusFailure(response *http.Response) *responsesFailureSpec {
	limited := io.LimitReader(response.Body, int64(s.maxErrorBodyBytes)+1)
	body, readErr := io.ReadAll(limited)
	truncated := len(body) > s.maxErrorBodyBytes
	if truncated {
		body = body[:s.maxErrorBodyBytes]
	}
	message := fmt.Sprintf("OpenAI API request failed with HTTP status %d", response.StatusCode)
	vendorCode := ""
	if readErr == nil && utf8.Valid(body) {
		var payload struct {
			Error struct {
				Message string `json:"message"`
				Code    string `json:"code"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &payload) == nil {
			if utf8.ValidString(payload.Error.Message) && strings.TrimSpace(payload.Error.Message) != "" {
				message = payload.Error.Message
			}
			vendorCode = normalizeResponsesVendorCode(payload.Error.Code)
			if isOpenAIContextOverflow(response.StatusCode, payload.Error.Type, payload.Error.Code, payload.Error.Message) {
				// Do not retain the vendor message/code on this classification path:
				// providers sometimes echo request fragments. The stable normalized
				// category is sufficient for coordinator policy and diagnostics.
				const safeCode = "context_length_exceeded"
				cause := &OpenAIResponsesHTTPError{status: response.StatusCode, message: "context window exceeded", vendorCode: safeCode, truncated: truncated}
				status := response.StatusCode
				return &responsesFailureSpec{
					kind: FailureContextOverflow, cause: cause,
					message: "OpenAI context window exceeded", httpStatus: &status,
					vendorCode: safeCode,
				}
			}
		}
	}
	cause := &OpenAIResponsesHTTPError{
		status:     response.StatusCode,
		message:    message,
		vendorCode: vendorCode,
		truncated:  truncated,
	}
	if readErr != nil {
		cause.readError = readErr
	}
	status := response.StatusCode
	retryAfter := providerRetryAfter(response.Header, s.clock())
	return &responsesFailureSpec{
		kind:        FailureHTTPStatus,
		cause:       cause,
		message:     message,
		httpStatus:  &status,
		vendorCode:  vendorCode,
		retryAfter:  retryAfter,
		shouldRetry: providerRetryOverride(response.Header),
	}
}

func isOpenAIContextOverflow(status int, errorType, code, message string) bool {
	if status != http.StatusBadRequest {
		return false
	}
	normalizeIdentifier := func(value string) string {
		value = strings.ToLower(strings.TrimSpace(value))
		value = strings.NewReplacer("-", "_", " ", "_", ".", "_").Replace(value)
		return value
	}
	for _, identifier := range []string{normalizeIdentifier(errorType), normalizeIdentifier(code)} {
		switch identifier {
		case "context_length_exceeded", "context_window_exceeded", "maximum_context_length_exceeded",
			"prompt_too_long", "input_too_long", "too_many_input_tokens":
			return true
		}
	}
	if !utf8.ValidString(message) {
		return false
	}
	// Message-only classification is intentionally much narrower than the
	// structured identifiers above. OpenAI 400s also describe output-token and
	// parameter limits with context-related words; treating those as input
	// overflow would make Agent compact an unrelated invalid request.
	lower := strings.Join(strings.Fields(strings.ToLower(message)), " ")
	for _, excluded := range []string{
		"output token", "output-token", "max output", "maximum output", "max_output",
		"completion token", "completion-token", "max completion", "maximum completion", "max_completion",
		"response token", "response-token", "parameter", "invalid", "unsupported",
		"max_tokens", "max tokens", "maximum_tokens", "must be less than", "must be at most",
	} {
		if strings.Contains(lower, excluded) {
			return false
		}
	}
	if strings.Contains(lower, "your input exceeds the context window") ||
		strings.Contains(lower, "your input exceeds this model's context window") ||
		strings.Contains(lower, "the input exceeds the context window") ||
		strings.Contains(lower, "the input exceeds this model's context window") {
		return true
	}
	if strings.Contains(lower, "input length") &&
		(strings.Contains(lower, "exceeds model's maximum context length") ||
			strings.Contains(lower, "exceeds the model's maximum context length") ||
			strings.Contains(lower, "exceeds the context window")) {
		return true
	}
	if strings.Contains(lower, "maximum context length is") &&
		strings.Contains(lower, "your messages resulted in") &&
		strings.Contains(lower, "token") {
		return true
	}
	if strings.Contains(lower, "too many input tokens") &&
		(strings.Contains(lower, "context window") || strings.Contains(lower, "maximum context length")) {
		return true
	}
	return strings.Contains(lower, "prompt is too long") &&
		(strings.Contains(lower, "context window") || strings.Contains(lower, "maximum context length"))
}

func (s *openAIResponsesStream) finishCancellation(cause error) (llm.StreamEvent, error) {
	return s.finishFailure(*s.cancellationFailure(cause))
}

func (s *openAIResponsesStream) cancellationFailure(cause error) *responsesFailureSpec {
	joined := error(ErrOpenAIResponsesAborted)
	if cause != nil {
		joined = errors.Join(ErrOpenAIResponsesAborted, cause)
	}
	return &responsesFailureSpec{
		kind:    FailureCancelled,
		cause:   joined,
		message: ErrOpenAIResponsesAborted.Error(),
	}
}

func (s *openAIResponsesStream) finishFailure(spec responsesFailureSpec) (llm.StreamEvent, error) {
	message := spec.message
	if !utf8.ValidString(message) || strings.TrimSpace(message) == "" {
		message = safeResponsesErrorText(spec.cause, "OpenAI Responses request failed")
	}
	failure, err := NewProviderFailure(ProviderFailureSpec{
		Kind:         spec.kind,
		Message:      message,
		RetryMessage: httpRetryMessage("OpenAI API", spec.kind, spec.httpStatus),
		Cause:        spec.cause,
		HTTPStatus:   spec.httpStatus,
		VendorCode:   spec.vendorCode,
		RetryAfter:   spec.retryAfter,
	})
	if err != nil {
		s.finishTransport()
		return nil, closedStreamError(fmt.Errorf("construct OpenAI Responses failure: %w", err))
	}
	terminalFailure, err := llm.NewFailure(failure.Error(), failure)
	if err != nil {
		s.finishTransport()
		return nil, closedStreamError(fmt.Errorf("construct OpenAI Responses terminal failure: %w", err))
	}
	reason := llm.FinishError
	if spec.kind == FailureCancelled {
		reason = llm.FinishAborted
	}
	usage := llm.Usage{}
	if s.pendingDone != nil {
		usage = s.pendingDone.Usage()
	}
	event, err := llm.NewErrorEventWithFailure(reason, terminalFailure, usage, s.timestamp, assistantProvenanceForModel(s.model))
	if err != nil {
		s.finishTransport()
		return nil, closedStreamError(fmt.Errorf("construct OpenAI Responses error event: %w", err))
	}
	s.queue = nil
	s.finishTransport()
	return event, nil
}

func (s *openAIResponsesStream) finishTransport() {
	s.lifecycleMu.Lock()
	s.finished = true
	body := s.takeBodyForCloseLocked()
	s.lifecycleMu.Unlock()
	s.cancel(errOpenAIResponsesStreamFinished)
	if s.timeoutCancel != nil {
		s.timeoutCancel()
	}
	if body != nil {
		s.recordCloseError(safeCloseResponsesBody(body))
	}
}

func (s *openAIResponsesStream) Close() error {
	if s == nil {
		return nil
	}
	s.lifecycleMu.Lock()
	s.closed = true
	body := s.takeBodyForCloseLocked()
	s.lifecycleMu.Unlock()
	s.cancel(errOpenAIResponsesStreamClosed)
	if s.timeoutCancel != nil {
		s.timeoutCancel()
	}
	if body != nil {
		s.recordCloseError(safeCloseResponsesBody(body))
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	return s.closeErr
}

func (s *openAIResponsesStream) takeBodyForCloseLocked() io.ReadCloser {
	if s.body == nil || s.bodyClosed {
		return nil
	}
	s.bodyClosed = true
	return s.body
}

func (s *openAIResponsesStream) recordCloseError(err error) {
	if err == nil {
		return
	}
	s.lifecycleMu.Lock()
	s.closeErr = errors.Join(s.closeErr, err)
	s.lifecycleMu.Unlock()
}

func (s *openAIResponsesStream) isClosed() bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	return s.closed
}

func (s *openAIResponsesStream) isClosedOrFinished() bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	return s.closed || s.finished
}

func (s *openAIResponsesStream) popEvent() llm.StreamEvent {
	event := s.queue[0]
	s.queue[0] = nil
	s.queue = s.queue[1:]
	return event
}

func (s *openAIResponsesStream) enqueueResponsesEvent(event llm.StreamEvent) {
	if s.deferOutput {
		s.deferredEvents = append(s.deferredEvents, responsesDeferredEvent{event: event})
		return
	}
	s.queue = append(s.queue, event)
}

func (s *openAIResponsesStream) deferResponsesReasoningEnd(index int, reasoning *responsesCompletedReasoning) {
	s.pendingReasoning[index] = reasoning
	s.deferredEvents = append(s.deferredEvents, responsesDeferredEvent{
		reasoningIndex: index,
		reasoningEnd:   true,
	})
	s.deferOutput = true
}

func (s *openAIResponsesStream) flushResponsesDeferredEvents() error {
	if !s.deferOutput {
		return nil
	}
	materialized := make([]llm.StreamEvent, 0, len(s.deferredEvents))
	for _, deferred := range s.deferredEvents {
		if !deferred.reasoningEnd {
			materialized = append(materialized, deferred.event)
			continue
		}
		reasoning := s.pendingReasoning[deferred.reasoningIndex]
		if reasoning == nil {
			return errors.New("missing deferred reasoning item")
		}
		block, err := llm.NewThinkingBlockWithSignature(reasoning.text, string(reasoning.rawItem), false)
		if err != nil {
			return err
		}
		end, err := llm.NewThinkingEndEvent(reasoning.contentIndex, block)
		if err != nil {
			return err
		}
		materialized = append(materialized, end)
		delete(s.pendingReasoning, deferred.reasoningIndex)
	}
	if len(s.pendingReasoning) != 0 {
		return errors.New("deferred reasoning item has no completion marker")
	}
	s.queue = append(s.queue, materialized...)
	s.deferredEvents = nil
	s.deferOutput = false
	return nil
}

func invokeResponsesHTTPDoer(client HTTPDoer, request *http.Request) (response *http.Response, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("HTTP client panicked with value of type %T", recovered)
			if recoveredError, ok := recovered.(error); ok {
				err = errors.Join(err, recoveredError)
			}
			response = nil
		}
	}()
	return client.Do(request)
}

func safeCloseResponsesBody(body io.Closer) (err error) {
	if body == nil {
		return nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("response body Close panicked with value of type %T", recovered)
			if recoveredError, ok := recovered.(error); ok {
				err = errors.Join(err, recoveredError)
			}
		}
	}()
	return body.Close()
}

func safeResponsesErrorText(err error, fallback string) (text string) {
	text = fallback
	if err == nil {
		return text
	}
	defer func() {
		if recover() != nil {
			text = fallback
		}
	}()
	candidate := err.Error()
	if utf8.ValidString(candidate) && strings.TrimSpace(candidate) != "" {
		return candidate
	}
	return text
}

func normalizeResponsesVendorCode(code string) string {
	const maxVendorCodeBytes = 256
	if !utf8.ValidString(code) ||
		strings.TrimSpace(code) == "" ||
		len(code) > maxVendorCodeBytes ||
		strings.ContainsFunc(code, unicode.IsControl) {
		return ""
	}
	return code
}

// OpenAIResponsesHTTPError is the cause retained for non-2xx responses.
type OpenAIResponsesHTTPError struct {
	status     int
	message    string
	vendorCode string
	truncated  bool
	readError  error
}

func (e *OpenAIResponsesHTTPError) Error() string {
	if e == nil {
		return "OpenAI API HTTP failure"
	}
	return fmt.Sprintf("OpenAI API HTTP %d: %s", e.status, e.message)
}

func (e *OpenAIResponsesHTTPError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.readError
}

func (e *OpenAIResponsesHTTPError) Status() int {
	if e == nil {
		return 0
	}
	return e.status
}

func (e *OpenAIResponsesHTTPError) VendorCode() string {
	if e == nil {
		return ""
	}
	return e.vendorCode
}

func (e *OpenAIResponsesHTTPError) BodyTruncated() bool {
	return e != nil && e.truncated
}

type responsesStreamPanicError struct {
	valueType string
	cause     error
	stack     []byte
}

func (e *responsesStreamPanicError) Error() string {
	if e == nil {
		return "OpenAI Responses stream panicked"
	}
	return "OpenAI Responses stream panicked with value of type " + e.valueType
}

func (e *responsesStreamPanicError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *responsesStreamPanicError) Stack() []byte {
	if e == nil {
		return nil
	}
	return bytes.Clone(e.stack)
}
