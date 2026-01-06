package router

import "sync"

// SupervisorRegistry is a small thread-safe registry for subsystem supervisors.
//
// Motivation: Services are shared across many goroutines (command handlers,
// plugin runtime, config hot-reload). A plain map would race.
type SupervisorRegistry struct {
	mu sync.RWMutex
	m  map[string]*Supervisor
}

func NewSupervisorRegistry() *SupervisorRegistry {
	return &SupervisorRegistry{m: map[string]*Supervisor{}}
}

// Set registers (or replaces) a supervisor under name. If sup is nil, it deletes.
func (r *SupervisorRegistry) Set(name string, sup *Supervisor) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if sup == nil {
		delete(r.m, name)
		return
	}
	r.m[name] = sup
}

func (r *SupervisorRegistry) Delete(name string) {
	r.Set(name, nil)
}

// Snapshot returns a copy of the current registry.
func (r *SupervisorRegistry) Snapshot() map[string]*Supervisor {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]*Supervisor, len(r.m))
	for k, v := range r.m {
		out[k] = v
	}
	return out
}
