package engine

import (
	"context"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"time"

	logx "pewbot/pkg/logx"
)

// initialPermitLimit returns a conservative starting concurrency limit.
// It will ramp up if queue pressure persists.
func initialPermitLimit(maxWorkers int) int32 {
	if maxWorkers <= 1 {
		return 1
	}
	// Conservative default: start at 2 (or 1 if only 2 workers).
	if maxWorkers == 2 {
		return 1
	}
	if maxWorkers >= 3 {
		return 2
	}
	return 1
}

func (s *Service) acquirePermit(ctx context.Context, stopCh <-chan struct{}) bool {
	ch := s.permits
	if ch == nil {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-stopCh:
		return false
	case <-ch:
		return true
	}
}

func (s *Service) releasePermit() {
	ch := s.permits
	if ch == nil {
		return
	}
	lim := atomic.LoadInt32(&s.permitLimit)
	if lim <= 0 {
		return
	}
	in := atomic.LoadInt32(&s.inFlight)
	avail := int32(len(ch))
	// Only return a token if it would not exceed the limit.
	if avail+in >= lim {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *Service) setPermitLimit(n int32) {
	max := atomic.LoadInt32(&s.permitMax)
	if max <= 0 {
		max = 1
	}
	if n < 1 {
		n = 1
	}
	if n > max {
		n = max
	}
	atomic.StoreInt32(&s.permitLimit, n)
	s.rebalancePermits()
}

func (s *Service) rebalancePermits() {
	ch := s.permits
	if ch == nil {
		return
	}
	lim := atomic.LoadInt32(&s.permitLimit)
	if lim <= 0 {
		return
	}
	in := atomic.LoadInt32(&s.inFlight)
	avail := int32(len(ch))

	// Drain extra tokens if limit decreased.
	for avail+in > lim {
		select {
		case <-ch:
			avail--
		default:
			return
		}
	}
	// Add tokens if limit increased.
	for avail+in < lim {
		select {
		case ch <- struct{}{}:
			avail++
		default:
			return
		}
	}
}

func (s *Service) autoscale(ctx context.Context, stopCh <-chan struct{}, queue <-chan queuedTask) {
	// Periodic controller: adjusts permitLimit based on queue pressure and runtime resource pressure.
	// Goals:
	//  - Scale up slowly when backlog persists.
	//  - Scale down quickly when memory/GC/goroutine pressure is high.
	//  - Scale down on sustained idle to stay conservative.
	const (
		tickEvery     = 2 * time.Second
		upCooldown    = 6 * time.Second
		downCooldown  = 3 * time.Second
		idleDownAfter = 3 // ticks
	)

	t := time.NewTicker(tickEvery)
	defer t.Stop()

	var lastChange time.Time
	idleTicks := 0

	var ms runtime.MemStats
	var lastPause uint64
	var lastGC uint32

	for {
		select {
		case <-ctx.Done():
			return
		case <-stopCh:
			return
		case <-t.C:
		}

		ql := 0
		qc := 0
		if queue != nil {
			ql = len(queue)
			qc = cap(queue)
		}

		lim := atomic.LoadInt32(&s.permitLimit)
		max := atomic.LoadInt32(&s.permitMax)
		if max <= 0 {
			max = 1
		}
		in := atomic.LoadInt32(&s.inFlight)
		waiting := atomic.LoadInt32(&s.waitingForPermit)

		runtime.ReadMemStats(&ms)
		gos := runtime.NumGoroutine()

		pauseDelta := ms.PauseTotalNs - lastPause
		gcDelta := ms.NumGC - lastGC
		lastPause = ms.PauseTotalNs
		lastGC = ms.NumGC

		// Memory limit (if configured via GOMEMLIMIT or debug.SetMemoryLimit).
		memLimit := debug.SetMemoryLimit(-1)
		memLimitSet := memLimit > 0 && memLimit < (1<<60) // filter out "no limit" sentinel values

		// Resource pressure signals.
		pressure := false
		reason := ""
		downBy := int32(0)

		// 1) Memory pressure.
		if memLimitSet {
			// Scale down when HeapInuse approaches the limit.
			// Use HeapInuse (rather than HeapAlloc) because it correlates better with actual heap footprint.
			h := int64(ms.HeapInuse)
			if h > (memLimit*85)/100 {
				pressure = true
				reason = "mem>85%"
				downBy = 2
			} else if h > (memLimit*75)/100 {
				pressure = true
				reason = "mem>75%"
				downBy = 1
			}
		} else {
			// Conservative hard thresholds if no explicit limit.
			if ms.HeapInuse > 1024<<20 { // > 1GiB
				pressure = true
				reason = "heap>1GiB"
				downBy = 2
			} else if ms.HeapInuse > 768<<20 { // > 768MiB
				pressure = true
				reason = "heap>768MiB"
				downBy = 1
			}
		}

		// 2) GC pressure (big pause deltas).
		if !pressure && gcDelta > 0 {
			if pauseDelta > uint64(250*time.Millisecond) {
				pressure = true
				reason = "gc_pause"
				downBy = 1
			}
		}

		// 3) Goroutine pressure.
		if !pressure {
			if gos > 3000 {
				pressure = true
				reason = "goroutines>3000"
				downBy = 2
			} else if gos > 1500 {
				pressure = true
				reason = "goroutines>1500"
				downBy = 1
			}
		}

		now := time.Now()
		target := lim

		if pressure {
			// Scale down quickly under pressure.
			if downBy <= 0 {
				downBy = 1
			}
			target = lim - downBy
			if target < 1 {
				target = 1
			}
			if target != lim && (lastChange.IsZero() || now.Sub(lastChange) >= downCooldown) {
				old := lim
				s.setPermitLimit(target)
				lastChange = now
				if !s.log.IsZero() {
					s.log.Debug("taskengine.active_limit", logx.Int("from", int(old)), logx.Int("to", int(target)), logx.String("reason", reason), logx.Int("queue", ql), logx.Int("queue_cap", qc), logx.Int("inflight", int(in)), logx.Int("waiting", int(waiting)), logx.Uint64("heap_inuse", ms.HeapInuse), logx.Int("goroutines", gos))
				}
			}
			continue
		}

		// No pressure: decide scale up/down based on backlog and idleness.
		backlog := int32(ql) + waiting
		if backlog == 0 && in == 0 {
			idleTicks++
		} else {
			idleTicks = 0
		}

		// Scale down on sustained idle.
		if idleTicks >= idleDownAfter && lim > 1 {
			if lastChange.IsZero() || now.Sub(lastChange) >= 10*time.Second {
				old := lim
				target = lim - 1
				s.setPermitLimit(target)
				lastChange = now
				idleTicks = 0
				if !s.log.IsZero() {
					s.log.Debug("taskengine.active_limit", logx.Int("from", int(old)), logx.Int("to", int(target)), logx.String("reason", "idle"), logx.Int("queue", ql), logx.Int("queue_cap", qc), logx.Int("inflight", int(in)), logx.Int("waiting", int(waiting)))
				}
			}
			continue
		}

		// Scale up when backlog is building.
		if backlog > 0 && lim < max {
			// queue pressure ratio (best-effort)
			ratio := 0.0
			if qc > 0 {
				ratio = float64(ql) / float64(qc)
			}

			bump := int32(0)
			// If backlog exceeds the current limit, or queue is filling up, increase.
			if backlog > lim {
				bump = 1
			}
			if ratio > 0.85 {
				bump = max32(bump, 2)
			} else if ratio > 0.60 {
				bump = max32(bump, 1)
			}

			if bump > 0 && (lastChange.IsZero() || now.Sub(lastChange) >= upCooldown) {
				old := lim
				target = lim + bump
				if target > max {
					target = max
				}
				s.setPermitLimit(target)
				lastChange = now
				if !s.log.IsZero() {
					s.log.Debug("taskengine.active_limit", logx.Int("from", int(old)), logx.Int("to", int(target)), logx.String("reason", "backlog"), logx.Int("queue", ql), logx.Int("queue_cap", qc), logx.Int("inflight", int(in)), logx.Int("waiting", int(waiting)), logx.Float64("q_ratio", ratio))
				}
			}
		}
	}
}

func max32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}
