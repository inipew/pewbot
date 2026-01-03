package speedtest

import "time"

// getConfig returns a snapshot of the current config with safe defaults applied.
//
// Defaults are usually set in OnConfigChange, but it's possible for commands or
// scheduled tasks to run before a config is loaded (or after a failed reload).
// Keeping this helper makes the run logic defensive.
func (p *Plugin) getConfig() Config {
	p.mu.RLock()
	c := p.cfg
	p.mu.RUnlock()

	// Safe defaults
	if c.ServerCount <= 0 {
		c.ServerCount = 5
	}
	if c.FullTestServers <= 0 {
		c.FullTestServers = 1
	}
	if c.FullTestServers > c.ServerCount {
		c.FullTestServers = c.ServerCount
	}
	if c.MaxConnections <= 0 {
		c.MaxConnections = 4
	}
	if c.operationTimeout <= 0 {
		c.operationTimeout = 30 * time.Second
	}
	if c.taskTimeout <= 0 {
		c.taskTimeout = 60 * time.Second
	}
	return c
}
