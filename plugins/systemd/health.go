package systemd

import (
	"context"
	"errors"
	"fmt"
)

// HealthLoopEnabled opts this plugin into the core-managed periodic health loop.
func (p *Plugin) HealthLoopEnabled() bool { return true }

// Health implements core.HealthChecker.
//
// This performs a lightweight D-Bus probe against one allowed unit to ensure the
// systemd connection is alive. It does not attempt any control operations.
func (p *Plugin) Health(ctx context.Context) (string, error) {
	if p == nil {
		return "nil", errors.New("plugin is nil")
	}
	select {
	case <-ctx.Done():
		return "canceled", ctx.Err()
	default:
	}

	cfg := p.cfgSnapshot()
	if len(cfg.AllowUnits) == 0 {
		// Not configured to manage any units.
		return "no_allow_units", nil
	}

	p.mu.RLock()
	mgr := p.mgr
	p.mu.RUnlock()
	if mgr == nil {
		return "no_manager", errors.New("systemd manager is not initialized")
	}

	unit := cfg.AllowUnits[0]
	st, err := mgr.GetStatusLiteContext(ctx, unit)
	if err != nil {
		return "dbus_error", err
	}
	if st != nil && st.Active == "failed" {
		return "failed", fmt.Errorf("unit %s is failed", unit)
	}
	return "ok", nil
}
