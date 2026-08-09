package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const defaultProviderMaxRetryDelay = 60 * time.Second

// OpenAI-compatible gateways may own authentication through Cloudflare's
// gateway header instead of an SDK-style API key. Empty values remain ordinary
// headers and never satisfy the authorization boundary.
func openAIHTTPHeadersHaveAuthorization(headers http.Header) bool {
	return strings.TrimSpace(headers.Get("authorization")) != "" ||
		strings.TrimSpace(headers.Get("cf-aig-authorization")) != ""
}

func providerRetryAfter(headers http.Header, now time.Time) *time.Duration {
	if raw := strings.TrimSpace(headers.Get("retry-after-ms")); raw != "" {
		milliseconds, err := strconv.ParseFloat(raw, 64)
		if err == nil && !math.IsNaN(milliseconds) && !math.IsInf(milliseconds, 0) {
			delay := time.Duration(milliseconds * float64(time.Millisecond))
			return &delay
		}
	}
	return responsesRetryAfter(headers.Get("retry-after"), now)
}

func providerShouldRetry(kind FailureKind, status *int, override *bool) bool {
	if override != nil {
		return *override
	}
	if kind == FailureTransport {
		return true
	}
	if kind != FailureHTTPStatus || status == nil {
		return false
	}
	return *status == http.StatusRequestTimeout || *status == http.StatusConflict || *status == http.StatusTooManyRequests || *status >= 500
}

func codexShouldRetry(failure *responsesFailureSpec) bool {
	if failure == nil {
		return false
	}
	if failure.kind == FailureTransport {
		return true
	}
	if failure.kind != FailureHTTPStatus || failure.httpStatus == nil {
		return false
	}
	text := strings.ToLower(failure.message + " " + failure.vendorCode)
	for _, terminal := range []string{
		"gousagelimiterror", "freeusagelimiterror", "monthly usage limit reached", "available balance",
		"insufficient_quota", "out of budget", "quota exceeded", "billing",
	} {
		if strings.Contains(text, terminal) {
			return false
		}
	}
	switch *failure.httpStatus {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	for _, retryable := range []string{"rate limit", "rate-limit", "ratelimit", "overloaded", "service unavailable", "service-unavailable", "upstream connect", "connection refused"} {
		if strings.Contains(text, retryable) {
			return true
		}
	}
	return false
}

func providerRetryOverride(headers http.Header) *bool {
	switch strings.ToLower(strings.TrimSpace(headers.Get("x-should-retry"))) {
	case "true":
		value := true
		return &value
	case "false":
		value := false
		return &value
	default:
		return nil
	}
}

// waitProviderRetry mirrors pi's pinned SDK policy. retryIndex is zero for the
// first retry. A server-requested delay above the configured cap is an error,
// not a silently shortened wait; zero disables the cap.
func waitProviderRetry(ctx context.Context, retryIndex uint32, requested *time.Duration, maxDelayMS *uint64, providerError string) error {
	return waitProviderRetryWithBase(ctx, retryIndex, requested, maxDelayMS, providerError, 500*time.Millisecond, true)
}

func waitCodexRetry(ctx context.Context, retryIndex uint32, requested *time.Duration, maxDelayMS *uint64, providerError string) error {
	return waitProviderRetryWithBase(ctx, retryIndex, requested, maxDelayMS, providerError, time.Second, false)
}

func waitProviderRetryWithBase(ctx context.Context, retryIndex uint32, requested *time.Duration, maxDelayMS *uint64, providerError string, base time.Duration, midpointJitter bool) error {
	var delay time.Duration
	if requested != nil {
		delay = *requested
		maxDelay := defaultProviderMaxRetryDelay
		if maxDelayMS != nil {
			if *maxDelayMS == 0 {
				maxDelay = 0
			} else if *maxDelayMS > uint64(math.MaxInt64/int64(time.Millisecond)) {
				maxDelay = time.Duration(math.MaxInt64)
			} else {
				maxDelay = time.Duration(*maxDelayMS) * time.Millisecond
			}
		}
		if maxDelay > 0 && delay > maxDelay {
			return fmt.Errorf("server requested %ds retry delay (max: %ds): %s", int64(math.Ceil(delay.Seconds())), int64(math.Ceil(maxDelay.Seconds())), providerError)
		}
	} else {
		// The SDK uses 500ms * 2^retryIndex, capped at eight seconds, then
		// subtracts up to 25% jitter. Use the midpoint (87.5%) so behavior is
		// bounded and deterministic under Go's race/replay tests.
		multiplier := math.Pow(2, float64(retryIndex))
		const maxDuration = time.Duration(1<<63 - 1)
		if math.IsInf(multiplier, 0) || multiplier > float64(maxDuration)/float64(base) {
			delay = maxDuration
		} else {
			delay = time.Duration(float64(base) * multiplier)
		}
		if midpointJitter {
			if delay > 8*time.Second {
				delay = 8 * time.Second
			}
			delay = time.Duration(float64(delay) * 0.875)
		}
	}
	if delay <= 0 {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		cause := context.Cause(ctx)
		if cause == nil {
			cause = ctx.Err()
		}
		return cause
	}
}

func streamContextWithTimeout(parent context.Context, timeoutMS *uint64) (context.Context, context.CancelCauseFunc, context.CancelFunc) {
	base := parent
	timeoutCancel := func() {}
	if timeoutMS != nil && *timeoutMS > 0 {
		if *timeoutMS > uint64(math.MaxInt64/int64(time.Millisecond)) {
			base, timeoutCancel = context.WithTimeout(parent, time.Duration(math.MaxInt64))
		} else {
			base, timeoutCancel = context.WithTimeout(parent, time.Duration(*timeoutMS)*time.Millisecond)
		}
	}
	ctx, cancel := context.WithCancelCause(base)
	return ctx, cancel, timeoutCancel
}

func retryWaitCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
