package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"pewbot/internal/kit"
	"pewbot/internal/storage"
)

// Capability names.
//
// This is an *operational* guardrail only (plugins are in-process).
// Capabilities are enforced by wrapping selected ports passed via PluginDeps.
const (
	CapNotifySend     = "notify.send"
	CapSchedulerRead  = "scheduler.read"
	CapSchedulerWrite = "scheduler.write"
	CapStorageRead    = "storage.read"
	CapStorageWrite   = "storage.write"
	CapStorageAudit   = "storage.audit"
)

var ErrCapabilityDenied = errors.New("capability denied")

// capRef is a mutable capability set shared by wrappers.
// It enables hot-reload of allowlists without re-initializing plugins.
type capRef struct {
	mu       sync.RWMutex
	allowAll bool
	set      map[string]struct{}
}

func newCapRef(allow []string) *capRef {
	r := &capRef{}
	r.Update(allow)
	return r
}

func (r *capRef) Update(allow []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(allow) == 0 {
		r.allowAll = true
		r.set = nil
		return
	}
	r.allowAll = false
	m := make(map[string]struct{}, len(allow))
	for _, s := range allow {
		if s == "" {
			continue
		}
		m[s] = struct{}{}
	}
	r.set = m
}

func (r *capRef) Allows(cap string) bool {
	if r == nil {
		return true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.allowAll {
		return true
	}
	_, ok := r.set[cap]
	return ok
}

func (r *capRef) AllowsAny(caps ...string) bool {
	for _, c := range caps {
		if r.Allows(c) {
			return true
		}
	}
	return false
}

func deny(cap string) error {
	return fmt.Errorf("%w: %s", ErrCapabilityDenied, cap)
}

// --- Wrapped ports ---

type capNotifier struct {
	inner NotifierPort
	caps  *capRef
}

func (n *capNotifier) Notify(ctx context.Context, nn kit.Notification) error {
	if n == nil || n.inner == nil {
		return errors.New("notifier not available")
	}
	if !n.caps.Allows(CapNotifySend) {
		return deny(CapNotifySend)
	}
	return n.inner.Notify(ctx, nn)
}

type capScheduler struct {
	inner SchedulerPort
	caps  *capRef
}

func (s *capScheduler) Enabled() bool {
	if s == nil || s.inner == nil {
		return false
	}
	// Allow read operations if plugin has read or write.
	if !s.caps.AllowsAny(CapSchedulerRead, CapSchedulerWrite) {
		return false
	}
	return s.inner.Enabled()
}

func (s *capScheduler) Snapshot() Snapshot {
	if s == nil || s.inner == nil {
		return Snapshot{}
	}
	if !s.caps.AllowsAny(CapSchedulerRead, CapSchedulerWrite) {
		return Snapshot{}
	}
	return s.inner.Snapshot()
}

func (s *capScheduler) AddCron(name, spec string, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	if !s.caps.Allows(CapSchedulerWrite) {
		return "", deny(CapSchedulerWrite)
	}
	return s.inner.AddCron(name, spec, timeout, job)
}

func (s *capScheduler) AddCronOpt(name, spec string, timeout time.Duration, opt TaskOptions, job func(ctx context.Context) error) (string, error) {
	if !s.caps.Allows(CapSchedulerWrite) {
		return "", deny(CapSchedulerWrite)
	}
	return s.inner.AddCronOpt(name, spec, timeout, opt, job)
}

func (s *capScheduler) AddInterval(name string, every time.Duration, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	if !s.caps.Allows(CapSchedulerWrite) {
		return "", deny(CapSchedulerWrite)
	}
	return s.inner.AddInterval(name, every, timeout, job)
}

func (s *capScheduler) AddIntervalOpt(name string, every time.Duration, timeout time.Duration, opt TaskOptions, job func(ctx context.Context) error) (string, error) {
	if !s.caps.Allows(CapSchedulerWrite) {
		return "", deny(CapSchedulerWrite)
	}
	return s.inner.AddIntervalOpt(name, every, timeout, opt, job)
}

func (s *capScheduler) AddOnce(name string, at time.Time, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	if !s.caps.Allows(CapSchedulerWrite) {
		return "", deny(CapSchedulerWrite)
	}
	return s.inner.AddOnce(name, at, timeout, job)
}

func (s *capScheduler) AddDaily(name, atHHMM string, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	if !s.caps.Allows(CapSchedulerWrite) {
		return "", deny(CapSchedulerWrite)
	}
	return s.inner.AddDaily(name, atHHMM, timeout, job)
}

func (s *capScheduler) AddWeekly(name string, weekday time.Weekday, atHHMM string, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	if !s.caps.Allows(CapSchedulerWrite) {
		return "", deny(CapSchedulerWrite)
	}
	return s.inner.AddWeekly(name, weekday, atHHMM, timeout, job)
}

func (s *capScheduler) Remove(name string) bool {
	if !s.caps.Allows(CapSchedulerWrite) {
		return false
	}
	return s.inner.Remove(name)
}

type capStore struct {
	inner storage.Store
	caps  *capRef
}

func (st *capStore) AppendAudit(ctx context.Context, e storage.AuditEntry) error {
	if st == nil || st.inner == nil {
		return storage.ErrDisabled
	}
	if !st.caps.AllowsAny(CapStorageAudit, CapStorageWrite) {
		return deny(CapStorageAudit)
	}
	return st.inner.AppendAudit(ctx, e)
}

func (st *capStore) PutDedup(ctx context.Context, key string, until time.Time) error {
	if st == nil || st.inner == nil {
		return storage.ErrDisabled
	}
	if !st.caps.Allows(CapStorageWrite) {
		return deny(CapStorageWrite)
	}
	return st.inner.PutDedup(ctx, key, until)
}

func (st *capStore) GetDedup(ctx context.Context, key string) (time.Time, bool, error) {
	if st == nil || st.inner == nil {
		return time.Time{}, false, storage.ErrDisabled
	}
	if !st.caps.AllowsAny(CapStorageRead, CapStorageWrite) {
		return time.Time{}, false, deny(CapStorageRead)
	}
	return st.inner.GetDedup(ctx, key)
}

func (st *capStore) Close() error {
	// plugins should not close the shared store; treat as no-op.
	return nil
}

func wrapServicesForPlugin(s *Services, caps *capRef) *Services {
	if s == nil {
		return nil
	}
	out := &Services{}
	// PluginsPort is read-only and is not capability-gated (operational surface).
	out.Plugins = s.Plugins
	// AppSupervisor is also operational/read-only.
	out.AppSupervisor = s.AppSupervisor
	if s.Scheduler != nil {
		out.Scheduler = &capScheduler{inner: s.Scheduler, caps: caps}
	}
	if s.Notifier != nil {
		out.Notifier = &capNotifier{inner: s.Notifier, caps: caps}
	}
	return out
}

func wrapStoreForPlugin(st storage.Store, caps *capRef) storage.Store {
	if st == nil {
		return nil
	}
	return &capStore{inner: st, caps: caps}
}
