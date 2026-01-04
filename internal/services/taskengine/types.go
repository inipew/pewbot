package taskengine

import (
	"context"
	"sync"
	"time"
)

// Config controls the task execution engine.
//
// NOTE: In Phase 2 we derive this from core.scheduler config for backwards
// compatibility, so there is no separate "taskengine" config block yet.
type Config struct {
	Enabled        bool
	Workers        int
	QueueSize      int
	DefaultTimeout time.Duration
	HistorySize    int
	RetryMax       int
}

type OverlapPolicy int

const (
	OverlapAllow OverlapPolicy = iota
	OverlapSkipIfRunning
)

type TaskOptions struct {
	Overlap       OverlapPolicy
	RetryMax      int
	RetryBase     time.Duration
	RetryMaxDelay time.Duration
	RetryJitter   float64 // 0.2 = 20%
}

func (o TaskOptions) withDefaults(cfg Config) TaskOptions {
	if o.RetryMax <= 0 {
		o.RetryMax = cfg.RetryMax
	}
	if o.RetryBase <= 0 {
		o.RetryBase = 500 * time.Millisecond
	}
	if o.RetryMaxDelay <= 0 {
		o.RetryMaxDelay = 15 * time.Second
	}
	if o.RetryJitter <= 0 {
		o.RetryJitter = 0.2
	}
	if o.Overlap != OverlapAllow && o.Overlap != OverlapSkipIfRunning {
		o.Overlap = OverlapSkipIfRunning
	}
	return o
}

// RunState tracks whether a task is already in-flight.
// In Phase 2 we treat "SkipIfRunning" as "skip if running OR already queued",
// which prevents queue blow-ups when a schedule triggers faster than execution.
type RunState struct {
	mu       sync.Mutex
	inflight int
}

func (s *RunState) tryAcquire() bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight > 0 {
		return false
	}
	s.inflight++
	return true
}

func (s *RunState) release() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.inflight > 0 {
		s.inflight--
	}
	s.mu.Unlock()
}

type HistoryItem struct {
	ID       string
	Name     string
	Started  time.Time
	Duration time.Duration
	Error    string
}

// TaskEvent is emitted on the event bus for task lifecycle events.
type TaskEvent struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Started  time.Time     `json:"started"`
	Duration time.Duration `json:"duration"`
	Attempts int           `json:"attempts"`
	Error    string        `json:"error,omitempty"`
}

// Task is a unit of work executed by the engine.
//
// ConcurrencyKey is reserved for future "concurrency groups". In Phase 2,
// SkipIfRunning uses State (if provided) to gate overlap.
type Task struct {
	ID             string
	Name           string
	Timeout        time.Duration
	Run            func(ctx context.Context) error
	Opt            TaskOptions
	ConcurrencyKey string
	State          *RunState
}

// Snapshot is a lightweight view for diagnostics.
type Snapshot struct {
	Enabled  bool
	Workers  int
	QueueLen int
	QueueCap int
	Dropped  uint64

	// Diagnostics for operators.
	DefaultTimeout time.Duration
	RetryMax       int
	History        []HistoryItem
}

// DefaultTaskOptions returns the effective task options when a task does not
// provide overrides.
//
// This is used by diagnostics (e.g., scheduler snapshot) to surface the active
// retry policy defaults.
func DefaultTaskOptions(cfg Config) TaskOptions {
	return (TaskOptions{}).withDefaults(cfg)
}
