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
	kind       FailureKind
	cause      error
	message    string
	httpStatus *int
	vendorCode string
	retryAfter *time.Duration
}

type responsesTextSlot struct {
	contentIndex int
	itemID       string
	text         strings.Builder
}

type openAIResponsesStream struct {
	ctx               context.Context
	cancel            context.CancelCauseFunc
	endpoint          string
	apiKey            string
	client            HTTPDoer
	clock             Clock
	timestamp         time.Time
	payload           []byte
	maxEventBytes     int
	maxErrorBodyBytes int
	preflight         *responsesFailureSpec

	lifecycleMu sync.Mutex
	body        io.ReadCloser
	bodyClosed  bool
	closeErr    error
	closed      bool
	finished    bool

	initialized       bool
	started           bool
	decoder           *responsesSSEDecoder
	queue             []llm.StreamEvent
	slots             map[int]*responsesTextSlot
	completedOutputs  map[int]struct{}
	nextContentIndex  int
	unsupportedOutput string
	pendingDone       *llm.DoneEvent
}

func newResponsesFailureStream(
	ctx context.Context,
	clock Clock,
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
		preflight: &responsesFailureSpec{
			kind:    kind,
			cause:   cause,
			message: message,
		},
		slots:            make(map[int]*responsesTextSlot),
		completedOutputs: make(map[int]struct{}),
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
		return llm.NewStartEvent(), nil
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
	}
}

func (s *openAIResponsesStream) initialize() (failure *responsesFailureSpec) {
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
	request.Header.Set("Authorization", "Bearer "+s.apiKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")

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
	if contentTypeErr != nil || !strings.EqualFold(mediaType, "text/event-stream") {
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
			} `json:"error"`
		}
		if json.Unmarshal(body, &payload) == nil {
			if utf8.ValidString(payload.Error.Message) && strings.TrimSpace(payload.Error.Message) != "" {
				message = payload.Error.Message
			}
			vendorCode = normalizeResponsesVendorCode(payload.Error.Code)
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
	retryAfter := responsesRetryAfter(response.Header.Get("Retry-After"), s.clock())
	return &responsesFailureSpec{
		kind:       FailureHTTPStatus,
		cause:      cause,
		message:    message,
		httpStatus: &status,
		vendorCode: vendorCode,
		retryAfter: retryAfter,
	}
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
		Kind:       spec.kind,
		Message:    message,
		Cause:      spec.cause,
		HTTPStatus: spec.httpStatus,
		VendorCode: spec.vendorCode,
		RetryAfter: spec.retryAfter,
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
	event, err := llm.NewErrorEventWithFailure(reason, terminalFailure, usage, s.timestamp)
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
