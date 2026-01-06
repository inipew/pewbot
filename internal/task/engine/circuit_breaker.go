package engine

import (
	"strings"
	"sync"
	"time"
)

// circuitState tracks consecutive failures for a single task key.
//
// It implements a simple consecutive-failure circuit breaker with cooldown:
//   - On success: resets failures and closes the circuit.
//   - On failure: increments failures and, once failures >= trip,
//     opens the circuit for an exponentially increasing cooldown.
type circuitState struct {
	fails       int
	openUntil   time.Time
	lastFailure time.Time
}

type circuitStore struct {
	mu sync.Mutex
	m  map[string]*circuitState
}

func (s *circuitStore) get(key string) *circuitState {
	if s == nil {
		return nil
	}
	k := strings.TrimSpace(key)
	if k == "" {
		return nil
	}

	s.mu.Lock()
	if s.m == nil {
		s.m = make(map[string]*circuitState)
	}
	st := s.m[k]
	if st == nil {
		st = &circuitState{}
		s.m[k] = st
	}
	s.mu.Unlock()
	return st
}

// circuitCfg holds effective settings after applying defaults.
type circuitCfg struct {
	trip       int
	baseDelay  time.Duration
	maxDelay   time.Duration
	resetAfter time.Duration
	enabled    bool
}

func effectiveCircuitCfg(engineCfg Config, opt TaskOptions) circuitCfg {
	trip := engineCfg.CircuitTripFailures
	if trip == 0 {
		trip = 5
	}
	if trip < 0 {
		return circuitCfg{enabled: false}
	}
	// Per-task override.
	if opt.CircuitTripFailures < 0 {
		return circuitCfg{enabled: false}
	}
	if opt.CircuitTripFailures > 0 {
		trip = opt.CircuitTripFailures
	}

	base := engineCfg.CircuitBaseDelay
	if base <= 0 {
		base = 5 * time.Second
	}
	maxD := engineCfg.CircuitMaxDelay
	if maxD <= 0 {
		maxD = 2 * time.Minute
	}
	reset := engineCfg.CircuitResetAfter
	if reset <= 0 {
		reset = 5 * time.Minute
	}

	return circuitCfg{trip: trip, baseDelay: base, maxDelay: maxD, resetAfter: reset, enabled: true}
}

func (s *Service) circuitIsOpen(now time.Time, taskKey string, cfg Config, opt TaskOptions) (bool, time.Time) {
	cc := effectiveCircuitCfg(cfg, opt)
	if !cc.enabled {
		return false, time.Time{}
	}
	st := s.circuits.get(taskKey)
	if st == nil {
		return false, time.Time{}
	}

	s.circuits.mu.Lock()
	defer s.circuits.mu.Unlock()

	// Opportunistic reset if last failure was long ago.
	if !st.lastFailure.IsZero() && cc.resetAfter > 0 && now.Sub(st.lastFailure) > cc.resetAfter {
		st.fails = 0
		st.openUntil = time.Time{}
	}
	if !st.openUntil.IsZero() && now.Before(st.openUntil) {
		return true, st.openUntil
	}
	return false, time.Time{}
}

func (s *Service) circuitRecordResult(now time.Time, taskKey string, cfg Config, opt TaskOptions, err error) {
	cc := effectiveCircuitCfg(cfg, opt)
	if !cc.enabled {
		return
	}
	st := s.circuits.get(taskKey)
	if st == nil {
		return
	}

	s.circuits.mu.Lock()
	defer s.circuits.mu.Unlock()

	// Opportunistic reset if last failure was long ago.
	if !st.lastFailure.IsZero() && cc.resetAfter > 0 && now.Sub(st.lastFailure) > cc.resetAfter {
		st.fails = 0
		st.openUntil = time.Time{}
	}

	if err == nil {
		st.fails = 0
		st.openUntil = time.Time{}
		st.lastFailure = time.Time{}
		return
	}

	st.fails++
	st.lastFailure = now

	if st.fails < cc.trip {
		return
	}

	// Exponential cooldown after tripping.
	pow := st.fails - cc.trip
	d := cc.baseDelay
	for i := 0; i < pow; i++ {
		d *= 2
		if d >= cc.maxDelay {
			d = cc.maxDelay
			break
		}
	}
	if d > cc.maxDelay {
		d = cc.maxDelay
	}
	st.openUntil = now.Add(d)
}

func (s *Service) circuitSnapshot(now time.Time, cfg Config) (total, open int) {
	cc := effectiveCircuitCfg(cfg, TaskOptions{})
	if !cc.enabled {
		return 0, 0
	}

	s.circuits.mu.Lock()
	defer s.circuits.mu.Unlock()
	if s.circuits.m == nil {
		return 0, 0
	}
	total = len(s.circuits.m)
	for _, st := range s.circuits.m {
		if st == nil {
			continue
		}
		if !st.openUntil.IsZero() && now.Before(st.openUntil) {
			open++
		}
	}
	return total, open
}
