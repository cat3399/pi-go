// Package auth owns durable API-key credentials and their request-lifetime
// resolution. It deliberately has no provider, CLI, or OAuth dependency.
package auth

import (
	"errors"
	"fmt"
)

var (
	// ErrCommandBacked marks the intentionally disabled shell-command form.
	ErrCommandBacked = errors.New("command-backed auth value is disabled")
	// ErrCredentialType marks a selected stored credential that the API-key
	// subset cannot consume. It must not fall through to a lower source.
	ErrCredentialType = errors.New("stored credential type is unsupported")
	// ErrPersistentAuthUnavailable marks a platform where this version cannot
	// safely admit or create a private credential file.
	ErrPersistentAuthUnavailable = errors.New("persistent auth is unavailable on this platform")
	// ErrPrivateAdmissionUnavailable marks an existing credential file whose
	// private access cannot be proven by this version.
	ErrPrivateAdmissionUnavailable = errors.New("credential file privacy cannot be verified on this platform")
)

// Kind is a stable, secret-safe failure category. Error strings must never
// contain credential values, raw JSON, command text, or an environment value.
type Kind string

const (
	KindInvalid       Kind = "invalid"
	KindMalformed     Kind = "malformed"
	KindPermission    Kind = "permission"
	KindLock          Kind = "lock"
	KindIO            Kind = "io"
	KindUnsupported   Kind = "unsupported"
	KindNotConfigured Kind = "not_configured"
	KindCancelled     Kind = "cancelled"
	KindOAuth         Kind = "oauth"
	KindTimeout       Kind = "timeout"
)

// Error reports an operation without exposing sensitive input. Cause is kept
// for errors.Is/errors.As, but its text is intentionally not included in Error.
type Error struct {
	Kind      Kind
	Operation string
	Provider  string
	Cause     error
}

func (e *Error) Error() string {
	if e.Provider != "" {
		return fmt.Sprintf("auth %s for provider %q: %s", e.Operation, e.Provider, e.Kind)
	}
	return fmt.Sprintf("auth %s: %s", e.Operation, e.Kind)
}

func (e *Error) Unwrap() error { return e.Cause }

func failure(kind Kind, operation, provider string, cause error) error {
	return &Error{Kind: kind, Operation: operation, Provider: provider, Cause: cause}
}

func IsKind(err error, kind Kind) bool {
	var target *Error
	return errors.As(err, &target) && target.Kind == kind
}
