package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"time"
)

const DefaultMaxRetryAfter = 60 * time.Second

var ErrInvalidRetryPolicy = errors.New("invalid provider retry policy")

// RetryPolicy is shared by Agent provider turns and ContextSummarizer. Attempts
// includes the first request. A zero MaxAttempts means one request; callers
// which want retries must opt into a finite larger value.
type RetryPolicy struct {
	MaxAttempts   uint32
	InitialDelay  time.Duration
	MaxDelay      time.Duration
	MaxRetryAfter time.Duration
	Sleep         func(context.Context, time.Duration) error
	Jitter        func(attempt uint32, delay time.Duration) time.Duration
}

// RetryController is an immutable, validated retry policy. Its zero value is
// not usable; construct it with NewRetryController.
type RetryController struct {
	maxAttempts   uint32
	initialDelay  time.Duration
	maxDelay      time.Duration
	maxRetryAfter time.Duration
	sleep         func(context.Context, time.Duration) error
	jitter        func(uint32, time.Duration) time.Duration
}

func NewRetryController(policy RetryPolicy) (RetryController, error) {
	maxAttempts := policy.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = 1
	}
	if policy.InitialDelay < 0 || policy.MaxDelay < 0 || policy.MaxRetryAfter < 0 {
		return RetryController{}, fmt.Errorf("%w: delay cannot be negative", ErrInvalidRetryPolicy)
	}
	if policy.MaxDelay != 0 && policy.InitialDelay > policy.MaxDelay {
		return RetryController{}, fmt.Errorf("%w: initial delay exceeds maximum", ErrInvalidRetryPolicy)
	}
	if maxAttempts > math.MaxUint32/2 {
		return RetryController{}, fmt.Errorf("%w: too many attempts", ErrInvalidRetryPolicy)
	}
	maxRetryAfter := policy.MaxRetryAfter
	if maxRetryAfter == 0 {
		maxRetryAfter = DefaultMaxRetryAfter
	}
	sleep := policy.Sleep
	if sleep == nil {
		sleep = retrySleep
	}
	return RetryController{
		maxAttempts: maxAttempts, initialDelay: policy.InitialDelay,
		maxDelay: policy.MaxDelay, maxRetryAfter: maxRetryAfter,
		sleep: sleep, jitter: policy.Jitter,
	}, nil
}

func (p RetryController) MaxAttempts() uint32 { return p.maxAttempts }

// Delay returns the bounded delay before nextAttempt. nextAttempt is 2 for the
// first retry. Retry-After may raise the exponential delay, but never beyond
// MaxRetryAfter; custom jitter remains under the larger configured hard cap.
func (p RetryController) Delay(nextAttempt uint32, failure *ProviderFailure) time.Duration {
	delay := p.initialDelay
	for attempt := uint32(2); attempt < nextAttempt && delay > 0; attempt++ {
		if delay > time.Duration(math.MaxInt64/2) {
			delay = time.Duration(math.MaxInt64)
			break
		}
		delay *= 2
	}
	if p.maxDelay > 0 && delay > p.maxDelay {
		delay = p.maxDelay
	}
	if failure != nil {
		if retryAfter, ok := failure.RetryAfter(); ok {
			if retryAfter > p.maxRetryAfter {
				retryAfter = p.maxRetryAfter
			}
			if retryAfter > delay {
				delay = retryAfter
			}
		}
	}
	if p.jitter != nil {
		if jittered := p.jitter(nextAttempt, delay); jittered >= 0 {
			delay = jittered
		}
	}
	hardCap := p.maxRetryAfter
	if p.maxDelay > hardCap {
		hardCap = p.maxDelay
	}
	if delay > hardCap {
		delay = hardCap
	}
	return delay
}

func (p RetryController) Wait(ctx context.Context, delay time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return p.sleep(ctx, delay)
}

func retrySleep(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return context.Cause(ctx)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// IsTransientFailure admits only failures which are safe to resend without a
// durable state change. Invalid/auth/config/context-overflow decisions remain
// with the Agent coordinator and are not ordinary retries.
func IsTransientFailure(failure *ProviderFailure) bool {
	if failure == nil {
		return false
	}
	switch failure.Kind() {
	case FailureTransport:
		return true
	case FailureHTTPStatus:
		status, ok := failure.HTTPStatus()
		return ok && (status == 408 || status == 409 || status == 425 || status == 429 || status >= 500)
	default:
		return false
	}
}

// IsTransientStreamError separates transport drops from collector/parser
// failures that share a caller's outer stream wrapper.
func IsTransientStreamError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) && (networkError.Timeout() || networkError.Temporary())
}
