package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
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

	log         *slog.Logger
	cancelOnErr bool
	errOnce     sync.Once
	firstErr    atomic.Value // stores error
	doneOnce    sync.Once
	doneCh      chan struct{}
	wg          sync.WaitGroup
}

type SupervisorOption func(*Supervisor)

// SupervisorCounters exposes best-effort goroutine counters.
// These are operational signals only (not a synchronization primitive).
type SupervisorCounters struct {
	Active  int64  `json:"active"`
	Started uint64 `json:"started"`
}

func WithLogger(log *slog.Logger) SupervisorOption {
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

		// Panic-safe wrapper
		defer func() {
			if r := recover(); r != nil {
				err := fmt.Errorf("panic in %s: %v", name, r)
				if s.log != nil {
					s.log.Error("goroutine panicked", slog.String("name", name), slog.Any("panic", r), slog.String("stack", string(debug.Stack())))
				}
				s.setErr(err)
				if s.cancelOnErr {
					s.cancel()
				}
			}
		}()

		if s.log != nil {
			s.log.Debug("goroutine started", slog.String("name", name))
		}
		err := fn(s.ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			s.setErr(fmt.Errorf("%s: %w", name, err))
			if s.cancelOnErr {
				s.cancel()
			}
		}
		if s.log != nil {
			s.log.Debug("goroutine stopped", slog.String("name", name))
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
