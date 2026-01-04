package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"pewbot/internal/eventbus"
	"pewbot/internal/services/taskengine"
)

// Config controls the scheduler (trigger) service.
//
// For backwards compatibility, this still contains execution-related fields
// (Workers/DefaultTimeout/HistorySize/RetryMax), but execution is handled by
// internal/services/taskengine.
type Config struct {
	Enabled        bool
	Workers        int
	DefaultTimeout time.Duration
	HistorySize    int
	Timezone       string // IANA TZ, e.g. "Asia/Jakarta"
	RetryMax       int    // max retries per task (default 3)
}

// Re-export execution types from taskengine.
type OverlapPolicy = taskengine.OverlapPolicy

type TaskOptions = taskengine.TaskOptions

type HistoryItem = taskengine.HistoryItem

type TaskEvent = taskengine.TaskEvent

const (
	OverlapAllow         = taskengine.OverlapAllow
	OverlapSkipIfRunning = taskengine.OverlapSkipIfRunning
)

type scheduleDef struct {
	id      string
	name    string
	spec    string // cron spec or @every
	timeout time.Duration
	job     func(ctx context.Context) error
	entryID cron.EntryID
	opt     TaskOptions
	state   *taskengine.RunState
}

type Service struct {
	mu sync.Mutex

	log *slog.Logger
	cfg Config
	loc *time.Location
	bus eventbus.Bus

	engine *taskengine.Service

	parser cron.Parser
	c      *cron.Cron
	defs   []scheduleDef

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

	// Executor diagnostics (taskengine).
	Workers        int
	QueueLen       int
	QueueCap       int
	Dropped        uint64
	DefaultTimeout time.Duration
	RetryMax       int
	RetryBase      time.Duration
	RetryMaxDelay  time.Duration
	RetryJitter    float64
	Schedules      []ScheduleInfo
	History        []HistoryItem
}
