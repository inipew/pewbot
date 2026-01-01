package systemd

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"pewbot/internal/kit"
	sch "pewbot/internal/services/scheduler"
	sm "pewbot/pkg/systemdmanager"
)

// ---- auto-recover loop ----

func (p *Plugin) stopAutoRecover() {
	// Stop scheduled auto-recover job if any
	s := p.deps.Services.Scheduler
	if s != nil {
		_ = s.Remove("systemd:auto-recover")
	}

	// reset runtime state
	p.autoMu.Lock()
	atomic.StoreInt32(&p.autoRecoverRunning, 0)
	p.recoverState = nil
	p.missingWarned = nil
	p.autoMu.Unlock()
}

func (p *Plugin) startAutoRecover(parent context.Context, cfg Config) {
	if !cfg.AutoRecover.Enabled || p.mgr == nil {
		return
	}

	s := p.deps.Services.Scheduler
	if s == nil || !s.Enabled() {
		p.log.Warn("auto-recover skipped (scheduler disabled)")
		return
	}

	interval := mustDur(cfg.AutoRecover.Interval, 30*time.Second)
	minDown := mustDur(cfg.AutoRecover.MinDown, 3*time.Second)
	backoffBase := mustDur(cfg.AutoRecover.BackoffBase, 5*time.Second)
	backoffMax := mustDur(cfg.AutoRecover.BackoffMax, 5*time.Minute)
	alertEvery := mustDur(cfg.AutoRecover.AlertInterval, 10*time.Minute)
	restartTimeout := mustDur(cfg.AutoRecover.RestartTimeout, 15*time.Second)
	alertStreak := cfg.AutoRecover.FailAlertStreak
	if alertStreak <= 0 {
		alertStreak = 3
	}

	// ensure a clean schedule on start/reload
	_ = s.Remove("systemd:auto-recover")

	// reset state for new schedule
	p.autoMu.Lock()
	p.recoverState = map[string]*unitRecoverState{}
	p.missingWarned = map[string]bool{}
	p.autoMu.Unlock()

	// validate allowlist once: warn about missing units and skip them in ticks
	p.validateAllowlistOnce(parent)

	_, err := s.AddIntervalOpt(
		"systemd:auto-recover",
		interval,
		0,
		sch.TaskOptions{Overlap: sch.OverlapSkipIfRunning},
		func(ctx context.Context) error {
			select {
			case <-parent.Done():
				return nil
			default:
			}
			// extra guard (cheap)
			if !atomic.CompareAndSwapInt32(&p.autoRecoverRunning, 0, 1) {
				return nil
			}
			defer atomic.StoreInt32(&p.autoRecoverRunning, 0)
			return p.autoRecoverTick(ctx, minDown, restartTimeout, backoffBase, backoffMax, alertStreak, alertEvery)
		},
	)
	if err != nil {
		p.log.Error("auto-recover schedule failed", slog.String("err", err.Error()))
		return
	}

	p.log.Info("auto-recover scheduled",
		slog.Duration("interval", interval),
		slog.Duration("min_down", minDown),
		slog.Duration("backoff_base", backoffBase),
		slog.Duration("backoff_max", backoffMax),
		slog.Int("alert_streak", alertStreak),
	)
}

func (p *Plugin) validateAllowlistOnce(parent context.Context) {
	mgr := p.mgr
	if mgr == nil {
		return
	}

	p.mu.RLock()
	units := append([]string(nil), p.cfg.AllowUnits...)
	p.mu.RUnlock()

	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
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
			p.missingWarned[u] = true
			p.autoMu.Unlock()

			p.log.Warn("auto-recover skipping missing unit", slog.String("unit", u))
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
	// Prefer systemd timestamps over "first seen down".
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
	minDown time.Duration,
	restartTimeout time.Duration,
	backoffBase time.Duration,
	backoffMax time.Duration,
	alertStreak int,
	alertEvery time.Duration,
) error {
	mgr := p.mgr
	if mgr == nil {
		return nil
	}

	p.mu.RLock()
	units := append([]string(nil), p.cfg.AllowUnits...)
	p.mu.RUnlock()

	now := time.Now()
	rng := rand.New(rand.NewSource(now.UnixNano()))

	for _, u := range units {
		// state snapshot / init
		p.autoMu.Lock()
		us := p.ensureUnitStateLocked(u)
		missingWarned := p.missingWarned[u]
		p.autoMu.Unlock()

		if us.Missing {
			continue
		}

		st, err := mgr.GetStatusContext(ctx, u)
		if err != nil || st == nil {
			continue
		}

		if st.LoadState == "not-found" || st.SubState == "not-found" {
			p.autoMu.Lock()
			us.Missing = true
			if !missingWarned {
				p.missingWarned[u] = true
			}
			p.autoMu.Unlock()
			if !missingWarned {
				p.log.Warn("auto-recover skipping missing unit", slog.String("unit", u))
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
			// fallback to wall clock if systemd didn't give timestamps
			ds = now
		}
		downFor := now.Sub(ds)
		if downFor < minDown {
			continue
		}

		p.autoMu.Lock()
		nextTry := us.NextTry
		p.autoMu.Unlock()
		if !nextTry.IsZero() && now.Before(nextTry) {
			continue
		}

		// attempt restart with timeout
		opCtx, cancel := context.WithTimeout(ctx, restartTimeout)
		rerr := mgr.RestartContext(opCtx, u)
		cancel()

		if rerr == nil {
			p.log.Info("auto-recover restart ok",
				slog.String("unit", u),
				slog.String("state", st.Active),
				slog.Duration("down_for", downFor),
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

		backoff := backoffBase
		if us.FailStreak > 1 {
			shift := us.FailStreak - 1
			if shift > 30 {
				shift = 30
			}
			backoff = backoffBase * time.Duration(1<<shift)
		}
		if backoff > backoffMax {
			backoff = backoffMax
		}

		jitter := 0.7 + rng.Float64()*0.6 // 0.7..1.3
		next := now.Add(time.Duration(float64(backoff) * jitter))
		us.NextTry = next

		shouldAlert := alertStreak > 0 && us.FailStreak >= alertStreak && (us.LastAlert.IsZero() || now.Sub(us.LastAlert) >= alertEvery)
		if shouldAlert {
			us.LastAlert = now
		}
		failStreak := us.FailStreak
		p.autoMu.Unlock()

		p.log.Warn("auto-recover restart failed",
			slog.String("unit", u),
			slog.String("state", st.Active),
			slog.Int("streak", failStreak),
			slog.Duration("down_for", downFor),
			slog.Duration("backoff", backoff),
			slog.Time("next_try", next),
			slog.String("err", rerr.Error()),
		)

		if shouldAlert {
			p.notifyAutoRecoverFailure(ctx, u, st, rerr, failStreak, downFor, next)
		}
	}

	return nil
}

func (p *Plugin) notifyAutoRecoverFailure(ctx context.Context, unit string, st *sm.ServiceStatus, err error, streak int, downFor time.Duration, next time.Time) {
	n := p.deps.Services.Notifier
	if n == nil {
		return
	}

	target := kit.ChatTarget{}
	// Prefer group_log (if configured), else first owner.
	group := strings.TrimSpace(p.deps.Config.Get().Telegram.GroupLog)
	if group != "" {
		if id, perr := strconv.ParseInt(group, 10, 64); perr == nil {
			target.ChatID = id
			// reuse logging thread if set
			target.ThreadID = p.deps.Config.Get().Logging.Telegram.ThreadID
		}
	}
	if target.ChatID == 0 && len(p.deps.OwnerUserID) > 0 {
		target.ChatID = p.deps.OwnerUserID[0]
	}

	if target.ChatID == 0 {
		return
	}

	state := ""
	if st != nil {
		state = st.Active + "/" + st.SubState
	}
	msg := fmt.Sprintf("systemd auto-recover: restart failed\n- unit=%s\n- state=%s\n- streak=%d\n- down_for=%s\n- next_try=%s\n- err=%v",
		unit, state, streak, downFor, next.Format(time.RFC3339), err,
	)
	_ = n.Notify(ctx, kit.Notification{
		Priority: 9,
		Target:   target,
		Text:     msg,
		Options:  &kit.SendOptions{DisablePreview: true},
	})
}
func mustDur(s string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
