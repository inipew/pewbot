package broadcast

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"pewbot/internal/kit"
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
	ID        string
	Name      string
	Total     int
	Done      int
	Failed    int
	Failures  []kit.ChatTarget
	StartedAt time.Time
	DoneAt    time.Time
	Running   bool
}

type Service struct {
	mu sync.Mutex

	cfg     Config
	adapter kit.Adapter
	log     *slog.Logger

	limiter *rate.Limiter
	queue   chan job
	stopCh  chan struct{}
	// stopDone is non-nil while a Stop() is in progress; it is closed when workers fully exit.
	stopDone chan struct{}

	statusMu  sync.RWMutex
	status    map[string]*JobStatus
	runCtx    context.Context
	runCancel context.CancelFunc
	workerWG  sync.WaitGroup
}
