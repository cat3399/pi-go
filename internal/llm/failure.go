package llm

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

var ErrInvalidFailure = errors.New("invalid terminal failure")

// Failure is the immutable, provider-neutral failure carried by an error event
// and its terminal assistant result. Cause is optional for compatibility with
// provider-originated messages that only contain controlled display text.
type Failure struct {
	message string
	cause   error
}

func NewFailure(message string, cause error) (Failure, error) {
	failure := Failure{message: message, cause: cause}
	if err := failure.validate(); err != nil {
		return Failure{}, err
	}
	return failure, nil
}

func (f Failure) validate() error {
	if !utf8.ValidString(f.message) || strings.TrimSpace(f.message) == "" {
		return fmt.Errorf("%w: message must be non-empty valid UTF-8", ErrInvalidFailure)
	}
	return nil
}

func (f Failure) Error() string { return f.message }

func (f Failure) Message() string { return f.message }

func (f Failure) Cause() error { return f.cause }

func (f Failure) Unwrap() error { return f.cause }
