package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// Config controls the scheduler service.
type Config struct {
	Enabled        bool
	Workers        int
	DefaultTimeout time.Duration
	HistorySize    int
	Timezone       string // IANA TZ, e.g. "Asia/Jakarta"
	RetryMax       int    // max retries per task (default 3)
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
	// defaults from scheduler config
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
	// default overlap: skip (safer)
	if o.Overlap != OverlapAllow && o.Overlap != OverlapSkipIfRunning {
		o.Overlap = OverlapSkipIfRunning
	}
	return o
}

type runState struct {
	mu      sync.Mutex
	running bool
}

type HistoryItem struct {
	ID       string
	Name     string
	Started  time.Time
	Duration time.Duration
	Error    string
}

type task struct {
	id      string
	name    string
	timeout time.Duration
	run     func(ctx context.Context) error
	opt     TaskOptions
	state   *runState
}

type scheduleDef struct {
	id      string
	name    string
	spec    string // cron spec or @every
	timeout time.Duration
	job     func(ctx context.Context) error
	entryID cron.EntryID
	opt     TaskOptions
	state   *runState
}

type Service struct {
	mu sync.Mutex

	log *slog.Logger
	cfg Config
	loc *time.Location

	parser cron.Parser
	c      *cron.Cron
	defs   []scheduleDef

	queue  chan task
	stopCh chan struct{}
	// stopDone is non-nil while a Stop() is in progress; it is closed when workers fully exit.
	stopDone chan struct{}

	// one-time timers (timers are runtime; onceAt/onceTimeout are persistent definitions)
	tmu         sync.Mutex
	timers      map[string]*time.Timer
	onceAt      map[string]time.Time
	onceTimeout map[string]time.Duration
	onceJob     map[string]func(ctx context.Context) error
	onceVer     map[string]uint64

	hmu       sync.Mutex
	history   []HistoryItem
	runCtx    context.Context
	runCancel context.CancelFunc
	workerWG  sync.WaitGroup
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
	Enabled   bool
	Timezone  string
	Workers   int
	QueueLen  int
	Schedules []ScheduleInfo
	History   []HistoryItem
}
