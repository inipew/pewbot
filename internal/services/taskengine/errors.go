package taskengine

import "errors"

var (
	ErrDisabled    = errors.New("task engine disabled")
	ErrStopped     = errors.New("task engine stopped")
	ErrQueueFull   = errors.New("task engine queue full")
	ErrOverlapSkip = errors.New("task skipped due to overlap policy")
)
