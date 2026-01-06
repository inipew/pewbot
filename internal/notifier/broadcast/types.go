package broadcast

import (
	"context"
	logx "pewbot/pkg/logx"
	"sync"
	"time"

	"golang.org/x/time/rate"

	kit "pewbot/internal/transport"
)

type Config struct {
	Enabled    bool
	Workers    int
	RatePerSec int
	RetryMax   int
}

type job struct {
	id      string
	name    string
	targets []kit.ChatTarget
	text    string
	opt     *kit.SendOptions
}

type JobStatus struct {
	ID       string
	Name     string
	Total    int
	Done     int
	Failed   int
	Failures []kit.ChatTarget
	// CreatedAt is when the status entry was created (i.e., when NewJob() was called).
	// Useful for pruning old entries even when a job never starts.
	CreatedAt time.Time
	StartedAt time.Time
	DoneAt    time.Time
	Running   bool
}

type Service struct {
	mu sync.Mutex

	cfg     Config
	adapter kit.Adapter
	log     logx.Logger

	limiter *rate.Limiter
	queue   chan job
	stopCh  chan struct{}
	// stopDone is non-nil while a Stop() is in progress; it is closed when workers fully exit.
	stopDone chan struct{}

	statusMu sync.RWMutex
	status   map[string]*JobStatus
	// statusMax/statusTTL bound in-memory status retention to prevent map growth.
	statusMax int
	statusTTL time.Duration
	runCtx    context.Context
	runCancel context.CancelFunc
	workerWG  sync.WaitGroup
}
