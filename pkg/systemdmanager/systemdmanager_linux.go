//go:build linux

package systemdmanager

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
)

//
// Public types
//

// ServiceStatus represents the current state of a service.
type ServiceStatus struct {
	Name          string
	Active        string // active, inactive, failed, etc.
	SubState      string // running, dead, etc.
	LoadState     string // loaded, not-found, etc.
	Description   string
	Memory        uint64        // in bytes
	Uptime        time.Duration // since it became active
	Enabled       bool          // is enabled on boot
	ActiveSince   time.Time     // ActiveEnterTimestamp
	ActiveExit    time.Time     // ActiveExitTimestamp
	InactiveSince time.Time     // InactiveEnterTimestamp
	StateChange   time.Time     // StateChangeTimestamp
}

// OperationResult wraps result of a single service operation
type OperationResult struct {
	ServiceName string
	Success     bool
	Error       error
	Message     string // Human-readable message (plain)
}

// BatchResult wraps results of multiple service operations
type BatchResult struct {
	Results      []OperationResult
	SuccessCount int
	FailureCount int
	Total        int
}

// ServiceEvent represents a service state change
type ServiceEvent struct {
	ServiceName string
	OldState    string
	NewState    string
	Timestamp   time.Time
}

// ServiceWatcher monitors service state changes
type ServiceWatcher struct {
	events chan ServiceEvent
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// ServiceManager handles systemd service operations.
type ServiceManager struct {
	mu       sync.RWMutex
	conn     *dbus.Conn
	services []string

	// enabledCache memoizes IsEnabled results to avoid expensive D-Bus calls
	// on every status check (auto-recover can call status frequently).
	enabledCache map[string]enabledCacheEntry
	enabledTTL   time.Duration

	enabledMax   int
	enabledSweep uint64
	enabledOps   uint64
}

type enabledCacheEntry struct {
	enabled bool
	expires time.Time
}

const defaultEnabledCacheTTL = 5 * time.Minute
const defaultEnabledCacheMax = 512
const defaultEnabledCacheSweepEvery = 64

//
// Construction & lifecycle
//

// NewServiceManagerContext creates a new service manager instance using ctx for the initial D-Bus connection.
// If ctx is nil, context.Background() is used.
func NewServiceManagerContext(ctx context.Context, services []string) (*ServiceManager, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := dbus.NewSystemConnectionContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to systemd: %w", err)
	}

	return &ServiceManager{
		conn:         conn,
		services:     append([]string(nil), services...), // defensive copy
		enabledCache: map[string]enabledCacheEntry{},
		enabledTTL:   defaultEnabledCacheTTL,
		enabledMax:   defaultEnabledCacheMax,
		enabledSweep: defaultEnabledCacheSweepEvery,
	}, nil
}

// NewServiceManager creates a new service manager instance (legacy helper).
// It uses context.Background() for the connection.
func NewServiceManager(services []string) (*ServiceManager, error) {
	return NewServiceManagerContext(context.Background(), services)
}

// SetEnabledCacheTTL updates the IsEnabled cache TTL.
//
// Semantics:
//   - ttl == 0 : use default TTL
//   - ttl < 0  : disable caching (every call hits D-Bus)
func (sm *ServiceManager) SetEnabledCacheTTL(ttl time.Duration) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if ttl == 0 {
		ttl = defaultEnabledCacheTTL
	}
	sm.enabledTTL = ttl
	sm.enabledOps = 0
	if sm.enabledCache != nil {
		for k := range sm.enabledCache {
			delete(sm.enabledCache, k)
		}
	}
}

// ClearEnabledCache clears cached IsEnabled results.
func (sm *ServiceManager) ClearEnabledCache() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.enabledOps = 0
	if sm.enabledCache != nil {
		for k := range sm.enabledCache {
			delete(sm.enabledCache, k)
		}
	}
}

// SetEnabledCacheMax updates the maximum number of cached IsEnabled entries.
//
// Semantics:
//   - max <= 0 : use default max
func (sm *ServiceManager) SetEnabledCacheMax(max int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if max <= 0 {
		max = defaultEnabledCacheMax
	}
	sm.enabledMax = max
	if sm.enabledCache != nil {
		sm.pruneEnabledCacheLocked(time.Now())
	}
}

func (sm *ServiceManager) pruneEnabledCacheLocked(now time.Time) {
	if sm.enabledCache == nil {
		return
	}
	max := sm.enabledMax
	if max <= 0 {
		max = defaultEnabledCacheMax
	}
	// 1) Drop expired entries.
	for k, ent := range sm.enabledCache {
		if !ent.expires.IsZero() && now.After(ent.expires) {
			delete(sm.enabledCache, k)
		}
	}
	if max <= 0 || len(sm.enabledCache) <= max {
		return
	}
	// 2) Still too large: drop the oldest (earliest expiry) entries.
	type kv struct {
		k string
		e time.Time
	}
	items := make([]kv, 0, len(sm.enabledCache))
	for k, ent := range sm.enabledCache {
		items = append(items, kv{k: k, e: ent.expires})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].e.Before(items[j].e) })
	excess := len(sm.enabledCache) - max
	for i := 0; i < excess && i < len(items); i++ {
		delete(sm.enabledCache, items[i].k)
	}
}

// Close closes the systemd connection.
func (sm *ServiceManager) Close() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.conn != nil {
		sm.conn.Close()
		sm.conn = nil
	}
	sm.enabledCache = nil
	return nil
}

//
// Service list management
//

// GetManagedServices returns a copy of the managed services list
func (sm *ServiceManager) GetManagedServices() []string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	result := make([]string, len(sm.services))
	copy(result, sm.services)
	return result
}

// AddManagedService adds a service to the managed list
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

// RemoveManagedService removes a service from the managed list
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

// IsManaged checks if a service is in the managed list
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

//
// Internal helpers
//

// exists checks if a service unit exists.
func (sm *ServiceManager) exists(ctx context.Context, serviceName string) bool {
	unitName := serviceName + ".service"

	props, err := sm.conn.GetUnitPropertiesContext(ctx, unitName)
	if err != nil {
		return false
	}

	loadState, _ := getStringProperty(props, "LoadState")
	return loadState != "not-found"
}

func parseTimestamp(props map[string]interface{}, key string) time.Time {
	if ts, ok := props[key].(uint64); ok && ts > 0 {
		// systemd timestamps are in microseconds since the Unix epoch
		return time.Unix(int64(ts/1_000_000), 0)
	}
	return time.Time{}
}

func getStringProperty(props map[string]interface{}, key string) (string, bool) {
	if val, ok := props[key].(string); ok {
		return val, true
	}
	return "", false
}

//
// Basic operations with OperationResult
//

func (sm *ServiceManager) StartWithResult(ctx context.Context, serviceName string) OperationResult {
	err := sm.StartContext(ctx, serviceName)
	return OperationResult{
		ServiceName: serviceName,
		Success:     err == nil,
		Error:       err,
		Message:     formatOperationMessage("start", serviceName, err),
	}
}

func (sm *ServiceManager) StopWithResult(ctx context.Context, serviceName string) OperationResult {
	err := sm.StopContext(ctx, serviceName)
	return OperationResult{
		ServiceName: serviceName,
		Success:     err == nil,
		Error:       err,
		Message:     formatOperationMessage("stop", serviceName, err),
	}
}

func (sm *ServiceManager) RestartWithResult(ctx context.Context, serviceName string) OperationResult {
	err := sm.RestartContext(ctx, serviceName)
	return OperationResult{
		ServiceName: serviceName,
		Success:     err == nil,
		Error:       err,
		Message:     formatOperationMessage("restart", serviceName, err),
	}
}

//
// Basic operations
//

func (sm *ServiceManager) StartContext(ctx context.Context, serviceName string) error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.conn == nil {
		return fmt.Errorf("systemd connection is closed")
	}

	_, err := sm.conn.StartUnitContext(ctx, serviceName+".service", "replace", nil)
	if err != nil {
		return fmt.Errorf("failed to start %s: %w", serviceName, err)
	}
	return nil
}

func (sm *ServiceManager) StopContext(ctx context.Context, serviceName string) error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.conn == nil {
		return fmt.Errorf("systemd connection is closed")
	}

	_, err := sm.conn.StopUnitContext(ctx, serviceName+".service", "replace", nil)
	if err != nil {
		return fmt.Errorf("failed to stop %s: %w", serviceName, err)
	}
	return nil
}

func (sm *ServiceManager) RestartContext(ctx context.Context, serviceName string) error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.conn == nil {
		return fmt.Errorf("systemd connection is closed")
	}

	_, err := sm.conn.RestartUnitContext(ctx, serviceName+".service", "replace", nil)
	if err != nil {
		return fmt.Errorf("failed to restart %s: %w", serviceName, err)
	}
	return nil
}

//
// Batch operations
//

func (sm *ServiceManager) BatchStart(ctx context.Context, serviceNames []string) BatchResult {
	return sm.batchOperation(ctx, serviceNames, sm.StartWithResult)
}
func (sm *ServiceManager) BatchStop(ctx context.Context, serviceNames []string) BatchResult {
	return sm.batchOperation(ctx, serviceNames, sm.StopWithResult)
}
func (sm *ServiceManager) BatchRestart(ctx context.Context, serviceNames []string) BatchResult {
	return sm.batchOperation(ctx, serviceNames, sm.RestartWithResult)
}

func (sm *ServiceManager) batchOperation(
	ctx context.Context,
	serviceNames []string,
	opFunc func(context.Context, string) OperationResult,
) BatchResult {
	results := make([]OperationResult, 0, len(serviceNames))

	for _, svc := range serviceNames {
		// Keep per-unit timeout to avoid hanging on a single D-Bus call.
		svcCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		result := opFunc(svcCtx, svc)
		cancel()
		results = append(results, result)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].ServiceName < results[j].ServiceName
	})

	successCount := 0
	for _, r := range results {
		if r.Success {
			successCount++
		}
	}

	return BatchResult{
		Results:      results,
		SuccessCount: successCount,
		FailureCount: len(results) - successCount,
		Total:        len(results),
	}
}

//
// Enable/disable & status
//

func (sm *ServiceManager) EnableContext(ctx context.Context, serviceName string) error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.conn == nil {
		return fmt.Errorf("systemd connection is closed")
	}

	_, _, err := sm.conn.EnableUnitFilesContext(ctx, []string{serviceName + ".service"}, false, true)
	if err != nil {
		return fmt.Errorf("failed to enable %s: %w", serviceName, err)
	}

	if err := sm.conn.ReloadContext(ctx); err != nil {
		return fmt.Errorf("enabled %s but failed to reload systemd daemon: %w", serviceName, err)
	}
	return nil
}

func (sm *ServiceManager) DisableContext(ctx context.Context, serviceName string) error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.conn == nil {
		return fmt.Errorf("systemd connection is closed")
	}

	_, err := sm.conn.DisableUnitFilesContext(ctx, []string{serviceName + ".service"}, false)
	if err != nil {
		return fmt.Errorf("failed to disable %s: %w", serviceName, err)
	}

	if err := sm.conn.ReloadContext(ctx); err != nil {
		return fmt.Errorf("disabled %s but failed to reload systemd daemon: %w", serviceName, err)
	}
	return nil
}

func (sm *ServiceManager) IsEnabled(ctx context.Context, serviceName string) bool {
	// Snapshot connection & cached value without holding the lock across D-Bus calls.
	now := time.Now()

	sm.mu.RLock()
	conn := sm.conn
	ttl := sm.enabledTTL
	// ttl == 0 => default TTL; ttl < 0 => disable caching entirely.
	if ttl == 0 {
		ttl = defaultEnabledCacheTTL
	}
	if conn == nil {
		sm.mu.RUnlock()
		return false
	}
	if ttl >= 0 {
		if ent, ok := sm.enabledCache[serviceName]; ok && now.Before(ent.expires) {
			en := ent.enabled
			sm.mu.RUnlock()
			return en
		}
	}
	sm.mu.RUnlock()

	unitName := serviceName + ".service"
	states, err := conn.ListUnitFilesByPatternsContext(ctx, nil, []string{unitName})
	if err != nil {
		// Don't cache failures; context timeouts can be transient.
		return false
	}

	enabled := false
	for _, state := range states {
		if state.Path == unitName || strings.HasSuffix(state.Path, "/"+unitName) {
			enabled = state.Type == "enabled"
			break
		}
	}

	if ttl >= 0 {
		sm.mu.Lock()
		if sm.enabledCache == nil {
			sm.enabledCache = map[string]enabledCacheEntry{}
		}
		if sm.enabledMax <= 0 {
			sm.enabledMax = defaultEnabledCacheMax
		}
		if sm.enabledSweep == 0 {
			sm.enabledSweep = defaultEnabledCacheSweepEvery
		}
		sm.enabledCache[serviceName] = enabledCacheEntry{enabled: enabled, expires: now.Add(ttl)}
		sm.enabledOps++
		if len(sm.enabledCache) > sm.enabledMax || (sm.enabledSweep > 0 && sm.enabledOps%sm.enabledSweep == 0) {
			sm.pruneEnabledCacheLocked(now)
		}
		sm.mu.Unlock()
	}

	return enabled
}

func isNoSuchUnitErr(err error) bool {
	if err == nil {
		return false
	}
	es := err.Error()
	// systemd returns org.freedesktop.systemd1.NoSuchUnit for missing units.
	if strings.Contains(es, "NoSuchUnit") {
		return true
	}
	// Fallback: be defensive for different backends.
	return strings.Contains(es, "not-found")
}

// GetStatusContext is a cheap status lookup intended for high-frequency checks (e.g., auto-recover).
//
// It uses ListUnitsByPatterns (lightweight) for the core state, and only falls back to the
// expensive property map when the unit is not active (to fetch down-since timestamps).
//
// Note: this intentionally does NOT populate Enabled/Memory/Uptime.
func (sm *ServiceManager) GetStatusContext(ctx context.Context, serviceName string) (*ServiceStatus, error) {
	sm.mu.RLock()
	conn := sm.conn
	sm.mu.RUnlock()
	if conn == nil {
		return nil, fmt.Errorf("systemd connection is closed")
	}

	unitName := serviceName + ".service"

	// Fast path: core state without pulling the full unit property map.
	units, err := conn.ListUnitsByPatternsContext(ctx, nil, []string{unitName})
	if err == nil && len(units) > 0 {
		// Prefer exact match if patterns returned multiple.
		u := units[0]
		for _, x := range units {
			if x.Name == unitName {
				u = x
				break
			}
		}
		st := &ServiceStatus{
			Name:        serviceName,
			Active:      u.ActiveState,
			SubState:    u.SubState,
			LoadState:   u.LoadState,
			Description: u.Description,
		}
		if st.LoadState == "not-found" || st.SubState == "not-found" {
			st.Active = "unknown"
			st.SubState = "not-found"
			st.LoadState = "not-found"
			return st, nil
		}
		// Healthy unit: no need for timestamps.
		if st.Active == "active" {
			return st, nil
		}
		// Down / transitioning: fetch a few timestamps used to compute "down since".
		props, perr := conn.GetUnitPropertiesContext(ctx, unitName)
		if perr == nil {
			st.ActiveSince = parseTimestamp(props, "ActiveEnterTimestamp")
			st.ActiveExit = parseTimestamp(props, "ActiveExitTimestamp")
			st.InactiveSince = parseTimestamp(props, "InactiveEnterTimestamp")
			st.StateChange = parseTimestamp(props, "StateChangeTimestamp")
		}
		return st, nil
	}

	// Fallback: property query (handles missing units or older backends).
	props, err := conn.GetUnitPropertiesContext(ctx, unitName)
	if err != nil {
		if isNoSuchUnitErr(err) {
			return &ServiceStatus{
				Name:      serviceName,
				Active:    "unknown",
				SubState:  "not-found",
				LoadState: "not-found",
			}, nil
		}
		return nil, fmt.Errorf("failed to get status for %s: %w", serviceName, err)
	}

	activeState, _ := getStringProperty(props, "ActiveState")
	subState, _ := getStringProperty(props, "SubState")
	loadState, _ := getStringProperty(props, "LoadState")
	description, _ := getStringProperty(props, "Description")

	if loadState == "not-found" {
		return &ServiceStatus{
			Name:      serviceName,
			Active:    "unknown",
			SubState:  "not-found",
			LoadState: "not-found",
		}, nil
	}

	return &ServiceStatus{
		Name:          serviceName,
		Active:        activeState,
		SubState:      subState,
		LoadState:     loadState,
		Description:   description,
		ActiveSince:   parseTimestamp(props, "ActiveEnterTimestamp"),
		ActiveExit:    parseTimestamp(props, "ActiveExitTimestamp"),
		InactiveSince: parseTimestamp(props, "InactiveEnterTimestamp"),
		StateChange:   parseTimestamp(props, "StateChangeTimestamp"),
	}, nil
}

func (sm *ServiceManager) GetStatusFullContext(ctx context.Context, serviceName string) (*ServiceStatus, error) {
	sm.mu.RLock()
	conn := sm.conn
	sm.mu.RUnlock()

	if conn == nil {
		return nil, fmt.Errorf("systemd connection is closed")
	}

	unitName := serviceName + ".service"
	props, err := conn.GetUnitPropertiesContext(ctx, unitName)
	if err != nil {
		if isNoSuchUnitErr(err) {
			return &ServiceStatus{
				Name:      serviceName,
				Active:    "unknown",
				SubState:  "not-found",
				LoadState: "not-found",
			}, nil
		}
		return nil, fmt.Errorf("failed to get status for %s: %w", serviceName, err)
	}

	activeState, _ := getStringProperty(props, "ActiveState")
	subState, _ := getStringProperty(props, "SubState")
	loadState, _ := getStringProperty(props, "LoadState")
	description, _ := getStringProperty(props, "Description")

	if loadState == "not-found" {
		return &ServiceStatus{
			Name:      serviceName,
			Active:    "unknown",
			SubState:  "not-found",
			LoadState: "not-found",
		}, nil
	}

	status := &ServiceStatus{
		Name:          serviceName,
		Active:        activeState,
		SubState:      subState,
		LoadState:     loadState,
		Description:   description,
		Enabled:       sm.IsEnabled(ctx, serviceName),
		ActiveSince:   parseTimestamp(props, "ActiveEnterTimestamp"),
		ActiveExit:    parseTimestamp(props, "ActiveExitTimestamp"),
		InactiveSince: parseTimestamp(props, "InactiveEnterTimestamp"),
		StateChange:   parseTimestamp(props, "StateChangeTimestamp"),
	}

	if mem, ok := props["MemoryCurrent"].(uint64); ok && mem > 0 {
		status.Memory = mem
	} else if mem, ok := props["MemoryUsage"].(uint64); ok && mem > 0 {
		status.Memory = mem
	} else if mainPID, ok := props["MainPID"].(uint32); ok && mainPID > 0 {
		if procMem := getMemoryFromProc(mainPID); procMem > 0 {
			status.Memory = procMem
		}
	} else {
		status.Memory = getMemoryFromSystemctl(serviceName)
	}

	if ts, ok := props["ActiveEnterTimestamp"].(uint64); ok && ts > 0 {
		startTime := time.Unix(int64(ts/1_000_000), 0)
		status.Uptime = time.Since(startTime)
	}

	return status, nil
}

// GetStatusLiteContext is a cheaper variant of GetStatusFullContext.
// It only fetches core state + timestamps, skipping Enabled/Memory/Uptime.
// Useful for high-frequency checks (e.g., auto-recover tick) to reduce load and
// avoid retaining large allocations from repeated D-Bus calls.
func (sm *ServiceManager) GetStatusLiteContext(ctx context.Context, serviceName string) (*ServiceStatus, error) {
	sm.mu.RLock()
	conn := sm.conn
	sm.mu.RUnlock()

	if conn == nil {
		return nil, fmt.Errorf("systemd connection is closed")
	}

	unitName := serviceName + ".service"
	props, err := conn.GetUnitPropertiesContext(ctx, unitName)
	if err != nil {
		if isNoSuchUnitErr(err) {
			return &ServiceStatus{
				Name:      serviceName,
				Active:    "unknown",
				SubState:  "not-found",
				LoadState: "not-found",
			}, nil
		}
		return nil, fmt.Errorf("failed to get status for %s: %w", serviceName, err)
	}

	activeState, _ := getStringProperty(props, "ActiveState")
	subState, _ := getStringProperty(props, "SubState")
	loadState, _ := getStringProperty(props, "LoadState")
	description, _ := getStringProperty(props, "Description")

	if loadState == "not-found" {
		return &ServiceStatus{
			Name:      serviceName,
			Active:    "unknown",
			SubState:  "not-found",
			LoadState: "not-found",
		}, nil
	}

	return &ServiceStatus{
		Name:          serviceName,
		Active:        activeState,
		SubState:      subState,
		LoadState:     loadState,
		Description:   description,
		ActiveSince:   parseTimestamp(props, "ActiveEnterTimestamp"),
		ActiveExit:    parseTimestamp(props, "ActiveExitTimestamp"),
		InactiveSince: parseTimestamp(props, "InactiveEnterTimestamp"),
		StateChange:   parseTimestamp(props, "StateChangeTimestamp"),
	}, nil
}

func (sm *ServiceManager) GetAllStatusContext(ctx context.Context) ([]ServiceStatus, error) {
	sm.mu.RLock()
	services := make([]string, len(sm.services))
	copy(services, sm.services)
	sm.mu.RUnlock()

	if len(services) == 0 {
		return []ServiceStatus{}, nil
	}

	statuses := make([]ServiceStatus, 0, len(services))
	for _, svc := range services {
		// Bound each status probe; this is used by chat commands.
		svcCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		st, err := sm.GetStatusFullContext(svcCtx, svc)
		cancel()

		if err != nil || st == nil {
			statuses = append(statuses, ServiceStatus{Name: svc, Active: "unknown", SubState: "error", LoadState: "not-found"})
			continue
		}
		statuses = append(statuses, *st)
	}

	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	return statuses, nil
}

func (sm *ServiceManager) GetFailedServices(ctx context.Context) ([]string, error) {
	statuses, err := sm.GetAllStatusContext(ctx)
	if err != nil {
		return nil, err
	}
	failed := make([]string, 0)
	for _, st := range statuses {
		if st.Active == "failed" {
			failed = append(failed, st.Name)
		}
	}
	return failed, nil
}

func (sm *ServiceManager) GetInactiveServices(ctx context.Context) ([]string, error) {
	statuses, err := sm.GetAllStatusContext(ctx)
	if err != nil {
		return nil, err
	}
	inactive := make([]string, 0)
	for _, st := range statuses {
		if st.Active == "inactive" {
			inactive = append(inactive, st.Name)
		}
	}
	return inactive, nil
}

//
// Watcher (poll-based, simple)
//

func (sm *ServiceManager) WatchServices(ctx context.Context, services []string) (*ServiceWatcher, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.conn == nil {
		return nil, fmt.Errorf("systemd connection is closed")
	}

	watchCtx, cancel := context.WithCancel(ctx)

	watcher := &ServiceWatcher{
		events: make(chan ServiceEvent, 100),
		cancel: cancel,
	}

	watcher.wg.Add(1)
	go func() {
		defer watcher.wg.Done()
		defer close(watcher.events)

		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		prevStates := make(map[string]string)
		for _, svc := range services {
			status, err := sm.GetStatusContext(watchCtx, svc)
			if err == nil {
				prevStates[svc] = status.Active
			}
		}

		for {
			select {
			case <-watchCtx.Done():
				return
			case <-ticker.C:
				for _, svc := range services {
					status, err := sm.GetStatusContext(watchCtx, svc)
					if err != nil {
						continue
					}
					if prev, ok := prevStates[svc]; ok && prev != status.Active {
						event := ServiceEvent{
							ServiceName: svc,
							OldState:    prev,
							NewState:    status.Active,
							Timestamp:   time.Now(),
						}
						select {
						case watcher.events <- event:
						case <-watchCtx.Done():
							return
						}
						prevStates[svc] = status.Active
					} else if !ok {
						prevStates[svc] = status.Active
					}
				}
			}
		}
	}()

	return watcher, nil
}

func (sw *ServiceWatcher) Events() <-chan ServiceEvent { return sw.events }

func (sw *ServiceWatcher) Stop() {
	if sw.cancel != nil {
		sw.cancel()
	}
	sw.wg.Wait()
}

//
// Memory helper functions
//

func getMemoryFromSystemctl(serviceName string) uint64 {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "systemctl", "show", serviceName+".service", "--property=MemoryCurrent")
	output, err := cmd.Output()
	if err != nil {
		return 0
	}

	line := strings.TrimSpace(string(output))
	parts := strings.Split(line, "=")
	if len(parts) != 2 {
		return 0
	}

	mem, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return 0
	}

	return mem
}

// getMemoryFromProc reads memory usage from /proc/[pid]/status (VmRSS).
func getMemoryFromProc(pid uint32) uint64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}

	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
					return kb * 1024 // kB → bytes
				}
			}
		}
	}
	return 0
}

//
// Formatting helpers (generic, non-UI)
//

func formatOperationMessage(action, serviceName string, err error) string {
	if err != nil {
		return fmt.Sprintf("%s %s: error: %v", action, serviceName, err)
	}
	return fmt.Sprintf("%s %s: ok", action, serviceName)
}

func FormatActionResult(serviceName, action string, err error) string {
	return formatOperationMessage(action, serviceName, err)
}
