package systemd

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	logx "pewbot/pkg/logx"
	sm "pewbot/pkg/systemdmanager"
)

type autoParams struct {
	minDown        time.Duration
	restartTimeout time.Duration
	backoffBase    time.Duration
	backoffMax     time.Duration
	alertEvery     time.Duration
	alertStreak    int
	jobTimeout     time.Duration
}

func (p *Plugin) resetAutoState() {
	p.autoMu.Lock()
	p.recoverState = nil
	p.missingWarn = nil
	p.autoMu.Unlock()
}

func (p *Plugin) reconcileAutoRecover(ctx context.Context, cfg Config) {
	// Always clear any existing schedule/state on reconcile.
	if p.Schedule() != nil {
		p.autoMu.Lock()
		old := p.autoTask
		p.autoMu.Unlock()
		if old != "" {
			p.Schedule().Remove(old)
		}
	}
	p.resetAutoState()

	sc := cfg.Scheduler
	name := sc.NameOr("auto_recover")
	if !sc.Active() {
		p.autoMu.Lock()
		p.autoTask = ""
		p.autoMu.Unlock()
		p.Log.Debug("auto-recover disabled")
		return
	}

	mgr := p.mgrSnapshot()
	if mgr == nil {
		p.Log.Warn("auto-recover skipped (systemd manager not available)")
		return
	}
	if p.Schedule() == nil {
		p.Log.Warn("auto-recover skipped (scheduler not available)")
		return
	}

	ap := parseAutoParams(cfg.AutoRecover, len(cfg.AllowUnits))

	// Initialize state maps.
	p.autoMu.Lock()
	p.recoverState = map[string]*unitRecoverState{}
	p.missingWarn = map[string]bool{}
	p.autoMu.Unlock()

	// Operation timeout is applied to per-unit status checks.
	opTO := cfg.Timeouts.OperationOr(2 * time.Second)

	// Validate allowlist once at startup/reload.
	p.validateAllowlistOnce(ctx, mgr, cfg.AllowUnits, opTO)

	taskTO := cfg.Timeouts.TaskOr(ap.jobTimeout)
	if cfg.Timeouts.Task != "" && taskTO < ap.restartTimeout+5*time.Second {
		p.Log.Warn("task timeout is shorter than restart_timeout; restarts may be cut off",
			logx.Duration("task_timeout", taskTO),
			logx.Duration("restart_timeout", ap.restartTimeout),
		)
	}

	err := p.Schedule().Spec(name, sc.Schedule).
		Timeout(taskTO).
		SkipIfRunning().
		Do(func(jobCtx context.Context) error {
			// task ctx already includes scheduler timeout; keep restart operations bounded too.
			return p.autoRecoverTick(jobCtx, mgr, cfg.AllowUnits, ap, opTO)
		})
	if err != nil {
		p.Log.Error("auto-recover schedule failed", logx.Err(err))
		return
	}

	if err == nil {
		p.autoMu.Lock()
		p.autoTask = name
		p.autoMu.Unlock()
	}

	p.Log.Info("auto-recover scheduled",
		logx.String("task", name),
		logx.String("spec", sc.Schedule),
		logx.Duration("min_down", ap.minDown),
		logx.Duration("restart_timeout", ap.restartTimeout),
		logx.Duration("backoff_base", ap.backoffBase),
		logx.Duration("backoff_max", ap.backoffMax),
		logx.Int("alert_streak", ap.alertStreak),
		logx.Duration("alert_interval", ap.alertEvery),
		logx.Duration("task_timeout", taskTO),
		logx.Duration("operation_timeout", opTO),
	)
}

func parseAutoParams(c AutoRecoverOptions, unitCount int) autoParams {
	minDown := mustDur(c.MinDown, 3*time.Second)
	restartTimeout := mustDur(c.RestartTimeout, 15*time.Second)
	backoffBase := mustDur(c.BackoffBase, 5*time.Second)
	backoffMax := mustDur(c.BackoffMax, 5*time.Minute)
	alertEvery := mustDur(c.AlertInterval, 10*time.Minute)
	alertStreak := c.FailAlertStreak
	if alertStreak <= 0 {
		alertStreak = 3
	}
	// Allow enough time to check multiple units sequentially.
	if unitCount < 1 {
		unitCount = 1
	}
	jobTimeout := time.Duration(unitCount)*restartTimeout + 15*time.Second
	if jobTimeout < 30*time.Second {
		jobTimeout = 30 * time.Second
	}
	if jobTimeout > 5*time.Minute {
		jobTimeout = 5 * time.Minute
	}
	return autoParams{
		minDown:        minDown,
		restartTimeout: restartTimeout,
		backoffBase:    backoffBase,
		backoffMax:     backoffMax,
		alertEvery:     alertEvery,
		alertStreak:    alertStreak,
		jobTimeout:     jobTimeout,
	}
}

func (p *Plugin) validateAllowlistOnce(parent context.Context, mgr *sm.ServiceManager, units []string, opTO time.Duration) {
	if mgr == nil || len(units) == 0 {
		return
	}
	if opTO <= 0 {
		opTO = 2 * time.Second
	}

	ctx, cancel := context.WithTimeout(parent, opTO)
	defer cancel()

	for _, u := range units {
		st, err := mgr.GetStatusContext(ctx, u)
		if err != nil || st == nil {
			continue
		}
		if st.LoadState == "not-found" || st.SubState == "not-found" {
			p.autoMu.Lock()
			us := p.ensureUnitStateLocked(u)
			us.Missing = true
			p.missingWarn[u] = true
			p.autoMu.Unlock()

			p.Log.Warn("auto-recover skipping missing unit", logx.String("unit", u))
		}
	}
}

func (p *Plugin) ensureUnitStateLocked(unit string) *unitRecoverState {
	if p.recoverState == nil {
		p.recoverState = map[string]*unitRecoverState{}
	}
	us, ok := p.recoverState[unit]
	if !ok {
		us = &unitRecoverState{}
		p.recoverState[unit] = us
	}
	return us
}

func (p *Plugin) pickDownSince(st *sm.ServiceStatus) time.Time {
	if st == nil {
		return time.Time{}
	}
	if !st.InactiveSince.IsZero() {
		return st.InactiveSince
	}
	if !st.ActiveExit.IsZero() {
		return st.ActiveExit
	}
	if !st.StateChange.IsZero() {
		return st.StateChange
	}
	return time.Time{}
}

func (p *Plugin) autoRecoverTick(
	ctx context.Context,
	mgr *sm.ServiceManager,
	units []string,
	ap autoParams,
	opTO time.Duration,
) error {
	if mgr == nil || len(units) == 0 {
		return nil
	}

	now := time.Now()
	rng := rand.New(rand.NewSource(now.UnixNano()))

	for _, u := range units {
		// state snapshot / init
		p.autoMu.Lock()
		us := p.ensureUnitStateLocked(u)
		missingWarned := p.missingWarn[u]
		p.autoMu.Unlock()

		if us.Missing {
			continue
		}

		stCtx := ctx
		var cancel context.CancelFunc
		if opTO > 0 {
			stCtx, cancel = context.WithTimeout(ctx, opTO)
		}
		st, err := mgr.GetStatusContext(stCtx, u)
		if cancel != nil {
			cancel()
		}
		if err != nil || st == nil {
			continue
		}

		if st.LoadState == "not-found" || st.SubState == "not-found" {
			p.autoMu.Lock()
			us.Missing = true
			if !missingWarned {
				p.missingWarn[u] = true
			}
			p.autoMu.Unlock()
			if !missingWarned {
				p.Log.Warn("auto-recover skipping missing unit", logx.String("unit", u))
			}
			continue
		}

		if st.Active == "active" {
			// clear state when healthy again
			p.autoMu.Lock()
			us.FailStreak = 0
			us.NextTry = time.Time{}
			us.LastErr = ""
			us.LastAlert = time.Time{}
			p.autoMu.Unlock()
			continue
		}

		ds := p.pickDownSince(st)
		if ds.IsZero() {
			ds = now
		}
		downFor := now.Sub(ds)
		if downFor < ap.minDown {
			continue
		}

		p.autoMu.Lock()
		nextTry := us.NextTry
		p.autoMu.Unlock()
		if !nextTry.IsZero() && now.Before(nextTry) {
			continue
		}

		// attempt restart with timeout
		opCtx, cancel := context.WithTimeout(ctx, ap.restartTimeout)
		rerr := mgr.RestartContext(opCtx, u)
		cancel()

		if rerr == nil {
			p.Log.Info("auto-recover restart ok",
				logx.String("unit", u),
				logx.String("state", st.Active),
				logx.Duration("down_for", downFor),
			)
			p.autoMu.Lock()
			us.FailStreak = 0
			us.NextTry = time.Time{}
			us.LastErr = ""
			us.LastAlert = time.Time{}
			p.autoMu.Unlock()
			continue
		}

		// failure: compute exponential backoff with jitter
		p.autoMu.Lock()
		us.FailStreak++
		us.LastErr = rerr.Error()

		backoff := ap.backoffBase
		if us.FailStreak > 1 {
			shift := us.FailStreak - 1
			if shift > 30 {
				shift = 30
			}
			backoff = ap.backoffBase * time.Duration(1<<shift)
		}
		if backoff > ap.backoffMax {
			backoff = ap.backoffMax
		}

		jitter := 0.7 + rng.Float64()*0.6 // 0.7..1.3
		next := now.Add(time.Duration(float64(backoff) * jitter))
		us.NextTry = next

		shouldAlert := ap.alertStreak > 0 && us.FailStreak >= ap.alertStreak && (us.LastAlert.IsZero() || now.Sub(us.LastAlert) >= ap.alertEvery)
		if shouldAlert {
			us.LastAlert = now
		}
		failStreak := us.FailStreak
		p.autoMu.Unlock()

		p.Log.Warn("auto-recover restart failed",
			logx.String("unit", u),
			logx.String("state", st.Active),
			logx.Int("streak", failStreak),
			logx.Duration("down_for", downFor),
			logx.Duration("backoff", backoff),
			logx.Time("next_try", next),
			logx.Err(rerr),
		)

		if shouldAlert {
			p.notifyAutoRecoverFailure(ctx, u, st, rerr, failStreak, downFor, next)
		}
	}

	return nil
}

func (p *Plugin) notifyAutoRecoverFailure(ctx context.Context, unit string, st *sm.ServiceStatus, err error, streak int, downFor time.Duration, next time.Time) {
	state := ""
	if st != nil {
		state = st.Active + "/" + st.SubState
	}
	msg := fmt.Sprintf(
		"systemd auto-recover: restart failed\n- unit=%s\n- state=%s\n- streak=%d\n- down_for=%s\n- next_try=%s\n- err=%v",
		unit,
		state,
		streak,
		downFor,
		next.Format(time.RFC3339),
		err,
	)
	_ = p.Notify().Error(msg)
}

func mustDur(s string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
