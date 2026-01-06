package engine

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrDisabled    = errors.New("task engine disabled")
	ErrStopped     = errors.New("task engine stopped")
	ErrStopping    = errors.New("task engine stopping")
	ErrQueueFull   = errors.New("task engine queue full")
	ErrOverlapSkip = errors.New("task skipped due to overlap policy")
	ErrCircuitOpen = errors.New("task skipped: circuit breaker open")
)

// NoRetry marks an error as non-retryable.
//
// Tasks can wrap validation errors or other permanent failures with NoRetry
// so the engine won't waste time retrying.
//
// Example:
//
//	return engine.NoRetry(fmt.Errorf("bad input: %w", err))
func NoRetry(err error) error {
	if err == nil {
		return nil
	}
	return noRetryError{err: err}
}

// IsNoRetry reports whether err is wrapped with NoRetry.
func IsNoRetry(err error) bool {
	var e noRetryError
	return errors.As(err, &e)
}

type noRetryError struct{ err error }

func (e noRetryError) Error() string { return fmt.Sprintf("no-retry: %v", e.err) }
func (e noRetryError) Unwrap() error { return e.err }

// RetryAfter provides a suggested delay before retrying.
//
// This is useful when the downstream system returns a Retry-After value
// (e.g., HTTP 429). The engine will respect the hint (bounded by RetryMaxDelay)
// and still apply jitter.
func RetryAfter(err error, after time.Duration) error {
	if err == nil {
		return nil
	}
	if after < 0 {
		after = 0
	}
	return retryAfterError{err: err, after: after}
}

// RetryAfterError is implemented by errors that carry an explicit retry delay.
type RetryAfterError interface {
	error
	RetryAfter() time.Duration
}

type retryAfterError struct {
	err   error
	after time.Duration
}

func (e retryAfterError) Error() string             { return fmt.Sprintf("retry-after(%s): %v", e.after, e.err) }
func (e retryAfterError) Unwrap() error             { return e.err }
func (e retryAfterError) RetryAfter() time.Duration { return e.after }
