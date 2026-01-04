package speedtest

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// HealthLoopEnabled opts this plugin into the core-managed periodic health loop.
//
// Speedtest health can be non-trivial in practice (network + allocations), so we
// want it to be supervised (owned + joinable) under the plugin supervisor.
func (p *Plugin) HealthLoopEnabled() bool { return true }

// Health implements core.HealthChecker.
//
// This is intentionally lightweight:
//   - It does not perform a full speedtest.
//   - It validates internal readiness and configuration invariants.
//
// If you want a "real" probe (server list / ping), do it in a dedicated
// scheduled task, not in Health().
func (p *Plugin) Health(ctx context.Context) (string, error) {
	if p == nil {
		return "nil", errors.New("plugin is nil")
	}
	select {
	case <-ctx.Done():
		return "canceled", ctx.Err()
	default:
	}

	// Gate state: if token isn't available, a run is in progress.
	p.runGateOnce.Do(func() {
		p.runGate = make(chan struct{}, 1)
		p.runGate <- struct{}{}
	})
	running := false
	select {
	case <-p.runGate:
		// token acquired -> not running
		p.runGate <- struct{}{}
	default:
		running = true
	}
	if running {
		return "running", nil
	}

	// If auto scheduler is enabled, ensure we have a task registered.
	cfg := p.getConfig()
	if cfg.Scheduler.Enabled && cfg.Scheduler.Schedule != "" {
		p.mu.RLock()
		auto := p.autoTask
		p.mu.RUnlock()
		if auto == "" {
			return "schedule_missing", fmt.Errorf("auto schedule enabled but task is not registered")
		}
	}

	// If history file is configured, ensure the directory is writable (best-effort).
	if cfg.HistoryFile != "" {
		// We only stat; we don't create files here.
		if _, err := os.Stat(cfg.HistoryFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "history_unreadable", fmt.Errorf("history file stat: %w", err)
		}
	}

	return "ok", nil
}
