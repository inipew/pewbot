package engine

import (
	"context"
	"sync"
	"time"
)

// Config controls the task execution engine.
//
// In this repo, the scheduler is trigger-only; execution settings belong here.
// The app layer maps config.task_engine (or legacy scheduler fields) into this struct.
type Config struct {
	Enabled   bool
	Workers   int
	QueueSize int

	// DefaultTimeout is used when Task.Timeout is 0.
	DefaultTimeout time.Duration

	// MaxQueueDelay drops tasks that have been queued longer than this duration.
	// 0 disables stale-queue dropping.
	MaxQueueDelay time.Duration

	HistorySize int
	RetryMax    int

	// Circuit breaker (consecutive-failure based).
	//
	// If CircuitTripFailures < 0, the circuit breaker is disabled.
	// If CircuitTripFailures == 0, a default is applied.
	CircuitTripFailures int
	CircuitBaseDelay    time.Duration
	CircuitMaxDelay     time.Duration
	CircuitResetAfter   time.Duration
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

	// ConcurrencyLimit limits concurrent executions within ConcurrencyKey.
	// 0 disables concurrency-group limiting.
	ConcurrencyLimit int

	// CircuitTripFailures overrides the engine circuit breaker threshold for this task.
	// If < 0, circuit breaker is disabled for this task.
	// If 0, engine default is used.
	CircuitTripFailures int
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
	if o.ConcurrencyLimit < 0 {
		o.ConcurrencyLimit = 0
	}
	// CircuitTripFailures is handled at runtime; just keep the value as-is.
	return o
}

// RunState tracks whether a task is already in-flight.
// In this engine we treat "SkipIfRunning" as "skip if running OR already queued",
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
	ID         string
	Name       string
	Started    time.Time
	QueueDelay time.Duration
	Duration   time.Duration
	Error      string
}

// TaskEvent is emitted on the event bus for task lifecycle events.
type TaskEvent struct {
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	Started    time.Time     `json:"started"`
	QueueDelay time.Duration `json:"queue_delay"`
	Duration   time.Duration `json:"duration"`
	Attempts   int           `json:"attempts"`
	Error      string        `json:"error,omitempty"`
}

// Task is a unit of work executed by the engine.
//
// ConcurrencyKey is reserved for future "concurrency groups".
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

	// Adaptive concurrency (soft limit).
	ActiveMax        int
	ActiveLimit      int
	InFlight         int
	WaitingForPermit int

	Dropped          uint64
	DroppedQueueFull uint64
	DroppedStale     uint64

	// Diagnostics for operators.
	DefaultTimeout time.Duration
	MaxQueueDelay  time.Duration
	RetryMax       int

	// Circuit breaker diagnostics.
	CircuitTotal int
	CircuitOpen  int

	History []HistoryItem
}

// DefaultTaskOptions returns the effective task options when a task does not
// provide overrides.
//
// This is used by diagnostics (e.g., scheduler snapshot) to surface the active
// retry policy defaults.
func DefaultTaskOptions(cfg Config) TaskOptions {
	return (TaskOptions{}).withDefaults(cfg)
}
