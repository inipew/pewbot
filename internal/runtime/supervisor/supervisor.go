package supervisor

import (
	"context"
	"errors"
	"fmt"
	logx "pewbot/pkg/logx"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Supervisor manages goroutines tied to a shared context.
// - Named goroutines (for logging/debug)
// - Panic recovery
// - Optional cancel-on-first-error
// - Graceful stop with timeout-aware waiting
type Supervisor struct {
	ctx    context.Context
	cancel context.CancelFunc

	// Counters are best-effort operational metrics.
	// - started: total goroutines ever started via this supervisor
	// - active: goroutines currently running under this supervisor
	started uint64
	active  int64

	log         logx.Logger
	cancelOnErr bool
	errOnce     sync.Once
	firstErr    atomic.Value // stores error
	doneOnce    sync.Once
	doneCh      chan struct{}
	wg          sync.WaitGroup

	mu    sync.Mutex
	stats map[string]*gorStats
}

type SupervisorOption func(*Supervisor)

// SupervisorCounters exposes best-effort goroutine counters.
// These are operational signals only (not a synchronization primitive).
type SupervisorCounters struct {
	Active  int64  `json:"active"`
	Started uint64 `json:"started"`
}

// GoroutineStats is an aggregated, best-effort view of goroutines started via Go/GoRestart.
//
// Notes:
//   - Stats are keyed by goroutine name, so multiple concurrent goroutines with the same name
//     are aggregated.
//   - Intended for observability/debugging only.
type GoroutineStats struct {
	Name         string        `json:"name"`
	Active       int64         `json:"active"`
	Started      uint64        `json:"started"`
	Panics       uint64        `json:"panics"`
	Restarts     uint64        `json:"restarts"`
	LastStartAt  time.Time     `json:"last_start_at"`
	LastStopAt   time.Time     `json:"last_stop_at"`
	LastErrAt    time.Time     `json:"last_err_at"`
	LastErr      string        `json:"last_err,omitempty"`
	LastPanicAt  time.Time     `json:"last_panic_at"`
	LastPanic    string        `json:"last_panic,omitempty"`
	LastRuntime  time.Duration `json:"last_runtime"`
	TotalRuntime time.Duration `json:"total_runtime"`
}

// SupervisorSnapshot is a point-in-time snapshot of a supervisor.
type SupervisorSnapshot struct {
	Counters   SupervisorCounters `json:"counters"`
	FirstError string             `json:"first_error,omitempty"`
	Goroutines []GoroutineStats   `json:"goroutines"`
}

// Internal aggregated stats per name.
type gorStats struct {
	name         string
	active       int64
	started      uint64
	panics       uint64
	restarts     uint64
	lastStartAt  time.Time
	lastStopAt   time.Time
	lastErrAt    time.Time
	lastErr      string
	lastPanicAt  time.Time
	lastPanic    string
	lastRuntime  time.Duration
	totalRuntime time.Duration
}

func WithLogger(log logx.Logger) SupervisorOption {
	return func(s *Supervisor) { s.log = log }
}

// If enabled, the first non-nil error from any goroutine will cancel the supervisor context.
func WithCancelOnError(enabled bool) SupervisorOption {
	return func(s *Supervisor) { s.cancelOnErr = enabled }
}

func NewSupervisor(parent context.Context, opts ...SupervisorOption) *Supervisor {
	ctx, cancel := context.WithCancel(parent)
	s := &Supervisor{
		ctx:    ctx,
		cancel: cancel,
		doneCh: make(chan struct{}),
		stats:  map[string]*gorStats{},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

func (s *Supervisor) Context() context.Context { return s.ctx }

// Cancel cancels the supervisor context without waiting for goroutines to exit.
func (s *Supervisor) Cancel() { s.cancel() }

func (s *Supervisor) Err() error {
	v := s.firstErr.Load()
	if v == nil {
		return nil
	}
	if err, ok := v.(error); ok {
		return err
	}
	return nil
}

// Counters returns best-effort goroutine counters for this supervisor.
func (s *Supervisor) Counters() SupervisorCounters {
	if s == nil {
		return SupervisorCounters{}
	}
	return SupervisorCounters{
		Active:  atomic.LoadInt64(&s.active),
		Started: atomic.LoadUint64(&s.started),
	}
}

// Snapshot returns a point-in-time snapshot of the supervisor.
//
// This is intended for observability/debug output, not for synchronization.
func (s *Supervisor) Snapshot() SupervisorSnapshot {
	if s == nil {
		return SupervisorSnapshot{}
	}
	snap := SupervisorSnapshot{Counters: s.Counters()}
	if err := s.Err(); err != nil {
		snap.FirstError = err.Error()
	}

	s.mu.Lock()
	gs := make([]GoroutineStats, 0, len(s.stats))
	for _, st := range s.stats {
		if st == nil {
			continue
		}
		gs = append(gs, GoroutineStats{
			Name:         st.name,
			Active:       st.active,
			Started:      st.started,
			Panics:       st.panics,
			Restarts:     st.restarts,
			LastStartAt:  st.lastStartAt,
			LastStopAt:   st.lastStopAt,
			LastErrAt:    st.lastErrAt,
			LastErr:      st.lastErr,
			LastPanicAt:  st.lastPanicAt,
			LastPanic:    st.lastPanic,
			LastRuntime:  st.lastRuntime,
			TotalRuntime: st.totalRuntime,
		})
	}
	s.mu.Unlock()

	sort.Slice(gs, func(i, j int) bool {
		// Active first, then most recently started, then name.
		if gs[i].Active != gs[j].Active {
			return gs[i].Active > gs[j].Active
		}
		if !gs[i].LastStartAt.Equal(gs[j].LastStartAt) {
			return gs[i].LastStartAt.After(gs[j].LastStartAt)
		}
		return gs[i].Name < gs[j].Name
	})

	snap.Goroutines = gs
	return snap
}

func (s *Supervisor) noteStart(name string, isRestart bool) time.Time {
	now := time.Now()
	if s == nil {
		return now
	}
	s.mu.Lock()
	if s.stats == nil {
		s.stats = map[string]*gorStats{}
	}
	st := s.stats[name]
	if st == nil {
		st = &gorStats{name: name}
		s.stats[name] = st
	}
	st.started++
	if isRestart {
		st.restarts++
	}
	st.active++
	st.lastStartAt = now
	s.mu.Unlock()
	return now
}

func (s *Supervisor) noteStop(name string, startedAt time.Time, err error) {
	now := time.Now()
	if s == nil {
		return
	}
	dur := now.Sub(startedAt)
	s.mu.Lock()
	if s.stats == nil {
		s.stats = map[string]*gorStats{}
	}
	st := s.stats[name]
	if st == nil {
		st = &gorStats{name: name}
		s.stats[name] = st
	}
	if st.active > 0 {
		st.active--
	}
	st.lastStopAt = now
	st.lastRuntime = dur
	st.totalRuntime += dur
	if err != nil {
		st.lastErr = err.Error()
		st.lastErrAt = now
	}
	s.mu.Unlock()
}

func (s *Supervisor) notePanic(name string, startedAt time.Time, p any) {
	now := time.Now()
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stats == nil {
		s.stats = map[string]*gorStats{}
	}
	st := s.stats[name]
	if st == nil {
		st = &gorStats{name: name}
		s.stats[name] = st
	}
	st.panics++
	st.lastPanicAt = now
	st.lastPanic = fmt.Sprint(p)
	s.mu.Unlock()
}

func (s *Supervisor) Go(name string, fn func(ctx context.Context) error) {
	if fn == nil {
		return
	}
	atomic.AddUint64(&s.started, 1)
	atomic.AddInt64(&s.active, 1)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer atomic.AddInt64(&s.active, -1)

		startedAt := s.noteStart(name, false)

		// Panic-safe wrapper
		defer func() {
			if r := recover(); r != nil {
				s.notePanic(name, startedAt, r)
				err := fmt.Errorf("panic in %s: %v", name, r)
				if !s.log.IsZero() {
					s.log.Error("goroutine panicked", logx.String("name", name), logx.Any("panic", r), logx.String("stack", string(debug.Stack())))
				}
				s.noteStop(name, startedAt, err)
				s.setErr(err)
				if s.cancelOnErr {
					s.cancel()
				}
			}
		}()

		if !s.log.IsZero() {
			s.log.Debug("goroutine started", logx.String("name", name))
		}
		err := fn(s.ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			err2 := fmt.Errorf("%s: %w", name, err)
			s.noteStop(name, startedAt, err2)
			s.setErr(err2)
			if s.cancelOnErr {
				s.cancel()
			}
		} else {
			s.noteStop(name, startedAt, nil)
		}
		if !s.log.IsZero() {
			s.log.Debug("goroutine stopped", logx.String("name", name))
		}
	}()
}

func (s *Supervisor) Go0(name string, fn func(ctx context.Context)) {
	if fn == nil {
		return
	}
	s.Go(name, func(ctx context.Context) error {
		fn(ctx)
		return nil
	})
}

// GoRestart0 is a convenience wrapper around GoRestart for functions that
// don't naturally return an error.
//
// The function will be restarted on panic, and on non-nil errors returned by
// the wrapper you provide via opts (e.g., when you explicitly return an error
// to indicate an unexpected exit).
func (s *Supervisor) GoRestart0(name string, fn func(ctx context.Context), opts ...RestartOption) {
	if fn == nil {
		return
	}
	s.GoRestart(name, func(ctx context.Context) error {
		fn(ctx)
		return nil
	}, opts...)
}

// RestartOption configures GoRestart.
type RestartOption func(*restartCfg)

type restartCfg struct {
	minBackoff      time.Duration
	maxBackoff      time.Duration
	maxRestarts     int // <=0 means unlimited
	stopOnCleanExit bool
	fatalOnFinalErr bool
	publishFirstErr bool
}

// WithRestartBackoff configures the exponential backoff window used between restarts.
func WithRestartBackoff(min, max time.Duration) RestartOption {
	return func(c *restartCfg) {
		if min > 0 {
			c.minBackoff = min
		}
		if max > 0 {
			c.maxBackoff = max
		}
	}
}

// WithMaxRestarts limits the number of restarts (errors/panics) before giving up.
//
// Note: the initial run is not counted as a restart.
func WithMaxRestarts(n int) RestartOption { return func(c *restartCfg) { c.maxRestarts = n } }

// WithFatalOnFinalError makes GoRestart set supervisor Err and optionally cancel the supervisor
// if it gives up after exhausting restarts.
func WithFatalOnFinalError(enabled bool) RestartOption {
	return func(c *restartCfg) { c.fatalOnFinalErr = enabled }
}

// WithPublishFirstError makes GoRestart set supervisor Err on the first observed error/panic.
// This is useful when you want failures to surface in /health while still auto-restarting.
func WithPublishFirstError(enabled bool) RestartOption {
	return func(c *restartCfg) { c.publishFirstErr = enabled }
}

// WithStopOnCleanExit makes GoRestart stop (not restart) if fn returns nil.
// Default is true.
func WithStopOnCleanExit(enabled bool) RestartOption {
	return func(c *restartCfg) { c.stopOnCleanExit = enabled }
}

// GoRestart runs fn and restarts it on error/panic using exponential backoff until ctx is canceled.
//
// This is intended for long-running loops (pollers, watchers, consumers) where transient failures
// should self-heal without bringing down the whole process.
func (s *Supervisor) GoRestart(name string, fn func(ctx context.Context) error, opts ...RestartOption) {
	if fn == nil {
		return
	}
	cfg := restartCfg{
		minBackoff:      250 * time.Millisecond,
		maxBackoff:      30 * time.Second,
		maxRestarts:     0,
		stopOnCleanExit: true,
	}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.minBackoff <= 0 {
		cfg.minBackoff = 250 * time.Millisecond
	}
	if cfg.maxBackoff < cfg.minBackoff {
		cfg.maxBackoff = cfg.minBackoff
	}

	// One supervisor goroutine hosts the restart loop.
	// Use a distinct internal name to avoid double-counting stats for the logical task name.
	wrapName := name + ".restart"
	s.Go0(wrapName, func(ctx context.Context) {
		backoff := cfg.minBackoff
		restarts := 0
		for {
			if ctx.Err() != nil {
				return
			}

			startedAt := s.noteStart(name, restarts > 0)

			// Run fn with panic capture.
			err, pan, stack := func() (err error, pan any, stack string) {
				defer func() {
					if r := recover(); r != nil {
						pan = r
						stack = string(debug.Stack())
					}
				}()
				err = fn(ctx)
				return
			}()

			if pan != nil {
				s.notePanic(name, startedAt, pan)
				if !s.log.IsZero() {
					s.log.Error("goroutine panicked (restart)", logx.String("name", name), logx.Any("panic", pan), logx.String("stack", stack))
				}
				err = fmt.Errorf("panic: %v", pan)
			}

			// If cancellation is requested (shutdown/drain), treat the run as a clean stop.
			//
			// This avoids false-positive "exited" errors when a restart-loop function
			// returns because its dependencies were stopped during shutdown.
			if ctx.Err() != nil {
				s.noteStop(name, startedAt, nil)
				return
			}

			// Context cancellation is a clean stop.
			if errors.Is(err, context.Canceled) {
				s.noteStop(name, startedAt, nil)
				return
			}
			if err == nil {
				if cfg.stopOnCleanExit {
					s.noteStop(name, startedAt, nil)
					return
				}
				// Treat clean exits as restarts when configured.
				err = errors.New("exited")
			}

			err2 := fmt.Errorf("%s: %w", name, err)
			s.noteStop(name, startedAt, err2)
			if cfg.publishFirstErr {
				s.setErr(err2)
			}

			restarts++
			// If the loop ran for a while before failing, reset backoff so rare failures
			// don't cause long restart delays.
			if time.Since(startedAt) >= 30*time.Second {
				backoff = cfg.minBackoff
			}
			if cfg.maxRestarts > 0 && restarts > cfg.maxRestarts {
				if !s.log.IsZero() {
					s.log.Error("goroutine gave up after restarts", logx.String("name", name), logx.Int("restarts", restarts), logx.Any("err", err))
				}
				if cfg.fatalOnFinalErr {
					s.setErr(err2)
					if s.cancelOnErr {
						s.cancel()
					}
				}
				return
			}

			// Jittered exponential backoff.
			wait := backoff
			if wait < cfg.minBackoff {
				wait = cfg.minBackoff
			}
			if wait > cfg.maxBackoff {
				wait = cfg.maxBackoff
			}
			// 20% jitter.
			j := time.Duration(int64(wait) / 5)
			if j > 0 {
				wait += time.Duration(time.Now().UnixNano() % int64(j+1))
			}
			if !s.log.IsZero() {
				s.log.Warn("goroutine restarting", logx.String("name", name), logx.Duration("backoff", wait), logx.Any("err", err))
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			// Increase backoff.
			backoff *= 2
			if backoff > cfg.maxBackoff {
				backoff = cfg.maxBackoff
			}
		}
	})
}

func (s *Supervisor) Stop(ctx context.Context) error {
	s.cancel()
	return s.Wait(ctx)
}

func (s *Supervisor) Wait(ctx context.Context) error {
	s.doneOnce.Do(func() {
		go func() {
			s.wg.Wait()
			close(s.doneCh)
		}()
	})

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.doneCh:
		return s.Err()
	}
}

func (s *Supervisor) setErr(err error) {
	if err == nil {
		return
	}
	s.errOnce.Do(func() { s.firstErr.Store(err) })
}
