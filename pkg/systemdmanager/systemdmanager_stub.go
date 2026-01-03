//go:build !linux

package systemdmanager

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

var ErrUnsupported = errors.New("systemdmanager: unsupported OS (linux only)")

type ServiceStatus struct {
	Name          string
	Active        string
	SubState      string
	LoadState     string
	Description   string
	Memory        uint64
	Uptime        time.Duration
	Enabled       bool
	ActiveSince   time.Time // ActiveEnterTimestamp
	ActiveExit    time.Time // ActiveExitTimestamp
	InactiveSince time.Time // InactiveEnterTimestamp
	StateChange   time.Time // StateChangeTimestamp
}

type OperationResult struct {
	ServiceName string
	Success     bool
	Error       error
	Message     string
}

type BatchResult struct {
	Results      []OperationResult
	SuccessCount int
	FailureCount int
	Total        int
}

type ServiceEvent struct {
	ServiceName string
	OldState    string
	NewState    string
	Timestamp   time.Time
}

type ServiceWatcher struct {
	events  chan ServiceEvent
	mu      sync.Mutex
	stopped bool
}

type ServiceManager struct {
	mu       sync.RWMutex
	services []string
}

func NewServiceManager(services []string) (*ServiceManager, error) {
	cp := append([]string(nil), services...)
	sort.Strings(cp)
	return &ServiceManager{services: cp}, nil
}

// SetEnabledCacheTTL updates the IsEnabled cache TTL (no-op on non-linux).
func (sm *ServiceManager) SetEnabledCacheTTL(ttl time.Duration) {}

// ClearEnabledCache clears cached IsEnabled results (no-op on non-linux).
func (sm *ServiceManager) ClearEnabledCache() {}

// SetEnabledCacheMax updates the IsEnabled cache max entries (no-op on non-linux).
func (sm *ServiceManager) SetEnabledCacheMax(max int) {}

func (sm *ServiceManager) Close() error { return nil }

func (sm *ServiceManager) GetManagedServices() []string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	cp := make([]string, len(sm.services))
	copy(cp, sm.services)
	return cp
}
func (sm *ServiceManager) AddManagedService(serviceName string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for _, s := range sm.services {
		if s == serviceName {
			return
		}
	}
	sm.services = append(sm.services, serviceName)
}
func (sm *ServiceManager) RemoveManagedService(serviceName string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for i, s := range sm.services {
		if s == serviceName {
			sm.services = append(sm.services[:i], sm.services[i+1:]...)
			return
		}
	}
}
func (sm *ServiceManager) IsManaged(serviceName string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	for _, s := range sm.services {
		if s == serviceName {
			return true
		}
	}
	return false
}

func (sm *ServiceManager) StartWithResult(ctx context.Context, serviceName string) OperationResult {
	return OperationResult{ServiceName: serviceName, Success: false, Error: ErrUnsupported, Message: "start " + serviceName + ": unsupported"}
}
func (sm *ServiceManager) StopWithResult(ctx context.Context, serviceName string) OperationResult {
	return OperationResult{ServiceName: serviceName, Success: false, Error: ErrUnsupported, Message: "stop " + serviceName + ": unsupported"}
}
func (sm *ServiceManager) RestartWithResult(ctx context.Context, serviceName string) OperationResult {
	return OperationResult{ServiceName: serviceName, Success: false, Error: ErrUnsupported, Message: "restart " + serviceName + ": unsupported"}
}
func (sm *ServiceManager) StartContext(ctx context.Context, serviceName string) error {
	return ErrUnsupported
}
func (sm *ServiceManager) StopContext(ctx context.Context, serviceName string) error {
	return ErrUnsupported
}
func (sm *ServiceManager) RestartContext(ctx context.Context, serviceName string) error {
	return ErrUnsupported
}

func (sm *ServiceManager) EnableContext(ctx context.Context, serviceName string) error {
	return ErrUnsupported
}
func (sm *ServiceManager) DisableContext(ctx context.Context, serviceName string) error {
	return ErrUnsupported
}
func (sm *ServiceManager) IsEnabled(ctx context.Context, serviceName string) bool { return false }

func (sm *ServiceManager) GetStatusContext(ctx context.Context, serviceName string) (*ServiceStatus, error) {
	return &ServiceStatus{Name: serviceName, Active: "unknown", SubState: "unsupported", LoadState: "unsupported"}, nil
}

func (sm *ServiceManager) GetStatusFullContext(ctx context.Context, serviceName string) (*ServiceStatus, error) {
	return sm.GetStatusContext(ctx, serviceName)
}

func (sm *ServiceManager) GetStatusLiteContext(ctx context.Context, serviceName string) (*ServiceStatus, error) {
	return sm.GetStatusContext(ctx, serviceName)
}
func (sm *ServiceManager) GetAllStatusContext(ctx context.Context) ([]ServiceStatus, error) {
	sm.mu.RLock()
	services := append([]string(nil), sm.services...)
	sm.mu.RUnlock()
	out := make([]ServiceStatus, 0, len(services))
	for _, s := range services {
		out = append(out, ServiceStatus{Name: s, Active: "unknown", SubState: "unsupported", LoadState: "unsupported"})
	}
	return out, nil
}
func (sm *ServiceManager) GetFailedServices(ctx context.Context) ([]string, error) {
	return []string{}, nil
}
func (sm *ServiceManager) GetInactiveServices(ctx context.Context) ([]string, error) {
	return []string{}, nil
}

func (sm *ServiceManager) BatchStart(ctx context.Context, serviceNames []string) BatchResult {
	return sm.batch(ctx, serviceNames, sm.StartWithResult)
}
func (sm *ServiceManager) BatchStop(ctx context.Context, serviceNames []string) BatchResult {
	return sm.batch(ctx, serviceNames, sm.StopWithResult)
}
func (sm *ServiceManager) BatchRestart(ctx context.Context, serviceNames []string) BatchResult {
	return sm.batch(ctx, serviceNames, sm.RestartWithResult)
}
func (sm *ServiceManager) batch(ctx context.Context, serviceNames []string, op func(context.Context, string) OperationResult) BatchResult {
	res := make([]OperationResult, 0, len(serviceNames))
	succ := 0
	for _, s := range serviceNames {
		r := op(ctx, s)
		if r.Success {
			succ++
		}
		res = append(res, r)
	}
	return BatchResult{Results: res, SuccessCount: succ, FailureCount: len(res) - succ, Total: len(res)}
}

func (sm *ServiceManager) WatchServices(ctx context.Context, services []string) (*ServiceWatcher, error) {
	ch := make(chan ServiceEvent)
	close(ch)
	return &ServiceWatcher{events: ch}, nil
}
func (sw *ServiceWatcher) Events() <-chan ServiceEvent { return sw.events }
func (sw *ServiceWatcher) Stop()                       {}
