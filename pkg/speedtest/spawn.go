package speedtest

// Spawner allows callers (e.g. the bot supervisor) to own goroutines created by
// the speedtest runner. When nil, the runner falls back to plain `go`.
//
// This package deliberately does not depend on the bot's internal supervisor
// implementation.
type Spawner interface {
	Go(name string, fn func())
}

// SpawnerFunc adapts a function to Spawner.
type SpawnerFunc func(name string, fn func())

func (f SpawnerFunc) Go(name string, fn func()) { f(name, fn) }
