package provider

import (
	"bytes"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"unicode/utf8"
)

var ErrInvalidProviderFailure = errors.New("invalid provider failure")

// FailureKind is the stable category used by the agent runtime for decisions.
// It is intentionally narrower than any vendor's error taxonomy.
type FailureKind uint8

const (
	FailureConfiguration FailureKind = iota + 1
	FailureInvalidRequest
	FailureQueueExhausted
	FailureFactory
	FailureTransport
	FailureHTTPStatus
	FailureInvalidResponse
	FailureCancelled
)

func (k FailureKind) String() string {
	switch k {
	case FailureConfiguration:
		return "configuration"
	case FailureInvalidRequest:
		return "invalidRequest"
	case FailureQueueExhausted:
		return "queueExhausted"
	case FailureFactory:
		return "factory"
	case FailureTransport:
		return "transport"
	case FailureHTTPStatus:
		return "httpStatus"
	case FailureInvalidResponse:
		return "invalidResponse"
	case FailureCancelled:
		return "cancelled"
	default:
		return "unknown"
	}
}

func (k FailureKind) valid() bool {
	return k >= FailureConfiguration && k <= FailureCancelled
}

type ProviderFailureSpec struct {
	Kind       FailureKind
	Message    string
	Cause      error
	HTTPStatus *int
	VendorCode string
}

// ProviderFailure is immutable structured provider failure information. HTTP
// status and vendor code are optional slots for real adapters; scripted errors
// use Kind and Cause without inventing transport metadata.
type ProviderFailure struct {
	kind          FailureKind
	message       string
	cause         error
	httpStatus    int
	hasHTTPStatus bool
	vendorCode    string
}

func NewProviderFailure(spec ProviderFailureSpec) (*ProviderFailure, error) {
	if !spec.Kind.valid() {
		return nil, fmt.Errorf("%w: unknown kind %d", ErrInvalidProviderFailure, spec.Kind)
	}
	if !utf8.ValidString(spec.Message) || strings.TrimSpace(spec.Message) == "" {
		return nil, fmt.Errorf("%w: message must be non-empty valid UTF-8", ErrInvalidProviderFailure)
	}
	if spec.Cause == nil {
		return nil, fmt.Errorf("%w: cause is required", ErrInvalidProviderFailure)
	}
	if spec.HTTPStatus != nil && (*spec.HTTPStatus < 100 || *spec.HTTPStatus > 599) {
		return nil, fmt.Errorf("%w: HTTP status %d is outside 100..599", ErrInvalidProviderFailure, *spec.HTTPStatus)
	}
	if !utf8.ValidString(spec.VendorCode) || (spec.VendorCode != "" && strings.TrimSpace(spec.VendorCode) == "") {
		return nil, fmt.Errorf("%w: vendor code must be empty or non-blank valid UTF-8", ErrInvalidProviderFailure)
	}

	failure := &ProviderFailure{
		kind:       spec.Kind,
		message:    spec.Message,
		cause:      spec.Cause,
		vendorCode: spec.VendorCode,
	}
	if spec.HTTPStatus != nil {
		failure.httpStatus = *spec.HTTPStatus
		failure.hasHTTPStatus = true
	}
	return failure, nil
}

func (e *ProviderFailure) Error() string {
	if e == nil {
		return "<nil provider failure>"
	}
	return e.message
}

func (e *ProviderFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *ProviderFailure) Kind() FailureKind {
	if e == nil {
		return 0
	}
	return e.kind
}

func (e *ProviderFailure) Cause() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *ProviderFailure) HTTPStatus() (int, bool) {
	if e == nil {
		return 0, false
	}
	return e.httpStatus, e.hasHTTPStatus
}

func (e *ProviderFailure) VendorCode() (string, bool) {
	if e == nil || e.vendorCode == "" {
		return "", false
	}
	return e.vendorCode, true
}

// FactoryPanicError retains a recovered factory panic as a normal error cause.
// Stack returns a copy so diagnostics cannot mutate the retained evidence.
type FactoryPanicError struct {
	description string
	panicType   string
	cause       error
	stack       []byte
}

func newFactoryPanicError(value any) *FactoryPanicError {
	panicType := fmt.Sprintf("%T", value)
	description := "value of type " + panicType
	if text, ok := value.(string); ok && utf8.ValidString(text) && strings.TrimSpace(text) != "" {
		description = text
	}
	cause, _ := value.(error)
	return &FactoryPanicError{
		description: description,
		panicType:   panicType,
		cause:       cause,
		stack:       debug.Stack(),
	}
}

func (e *FactoryPanicError) Error() string {
	if e == nil {
		return "scripted response factory panicked"
	}
	return "scripted response factory panicked: " + e.description
}

func (e *FactoryPanicError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *FactoryPanicError) PanicType() string {
	if e == nil {
		return ""
	}
	return e.panicType
}

func (e *FactoryPanicError) Stack() []byte {
	if e == nil {
		return nil
	}
	return bytes.Clone(e.stack)
}
