package scheduler

import (
	"context"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"pewbot/internal/eventbus"
	"pewbot/internal/task/engine"
	logx "pewbot/pkg/logx"
)

// Config controls the scheduler (trigger) service.
//
// Deprecated legacy fields:
//   - Workers, DefaultTimeout, HistorySize, RetryMax
//
// These fields used to configure execution; execution is now handled by the task
// engine. The fields are still present so older configs don't break under
// DisallowUnknownFields.
type Config struct {
	Enabled        bool
	Workers        int
	DefaultTimeout time.Duration
	HistorySize    int
	Timezone       string // IANA TZ, e.g. "Asia/Jakarta"
	RetryMax       int    // max retries per task (default 3)
}

// Re-export execution types from engine.
type OverlapPolicy = engine.OverlapPolicy

type TaskOptions = engine.TaskOptions

type HistoryItem = engine.HistoryItem

type TaskEvent = engine.TaskEvent

const (
	OverlapAllow         = engine.OverlapAllow
	OverlapSkipIfRunning = engine.OverlapSkipIfRunning
)

type scheduleDef struct {
	id            string
	name          string
	spec          string // cron spec or @every
	timeout       time.Duration
	job           func(ctx context.Context) error
	entryID       cron.EntryID
	startupSpread time.Duration // initial random delay for @every schedules (startup spread)
	opt           TaskOptions
	state         *engine.RunState
}

type Service struct {
	mu sync.Mutex

	log logx.Logger
	cfg Config
	loc *time.Location
	bus eventbus.Bus

	engine *engine.Service

	parser cron.Parser
	c      *cron.Cron
	defs   []scheduleDef

	// Enqueue error throttling: key is schedule name.
	enqMu       sync.Mutex
	lastEnqWarn map[string]time.Time

	// one-time timers (timers are runtime; onceAt/onceTimeout are persistent definitions)
	tmu         sync.Mutex
	timers      map[string]*time.Timer
	onceAt      map[string]time.Time
	onceTimeout map[string]time.Duration
	onceJob     map[string]func(ctx context.Context) error
	onceVer     map[string]uint64
}

type ScheduleInfo struct {
	ID      string
	Name    string
	Spec    string
	Timeout time.Duration
	Next    time.Time
	Prev    time.Time
}

type Snapshot struct {
	Enabled  bool
	Timezone string

	// Executor diagnostics (task engine).
	Workers          int
	ActiveMax        int
	ActiveLimit      int
	InFlight         int
	WaitingForPermit int
	QueueLen         int
	QueueCap         int
	Dropped          uint64
	DroppedQueueFull uint64
	DroppedStale     uint64
	DefaultTimeout   time.Duration
	MaxQueueDelay    time.Duration
	RetryMax         int
	RetryBase        time.Duration
	RetryMaxDelay    time.Duration
	RetryJitter      float64
	Schedules        []ScheduleInfo
	History          []HistoryItem
}
