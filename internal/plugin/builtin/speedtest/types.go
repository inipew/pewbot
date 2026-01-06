package speedtest

import (
	"sync"
	"time"

	pluginkit "pewbot/internal/plugin/kit"
	speedpkg "pewbot/pkg/speedtest"
)

// Config defines plugin configuration
type Config struct {
	Prefix      string                        `json:"prefix"`
	Scheduler   pluginkit.SchedulerTaskConfig `json:"scheduler"`
	Timeouts    pluginkit.TimeoutsConfig      `json:"timeouts,omitempty"`
	HistoryFile string                        `json:"history_file"`

	// ServerCount is the number of candidate servers to consider.
	// We will run latency tests on these candidates, then run the full
	// download/upload test on FullTestServers of the best candidates.
	ServerCount int `json:"server_count"`

	// FullTestServers controls how many of the lowest-latency servers
	// will be used for the full download/upload test (sequentially).
	// Keeping this small (1-2) dramatically reduces peak memory usage.
	FullTestServers int `json:"full_test_servers"`

	// SavingMode and MaxConnections are passed into speedtest-go UserConfig.
	// These help reduce peak allocations during download/upload tests.
	SavingMode     bool `json:"saving_mode"`
	MaxConnections int  `json:"max_connections"`

	// PingConcurrency caps how many latency tests run concurrently.
	PingConcurrency int `json:"ping_concurrency"`

	// DisableHTTP2 prevents HTTP/2 for speedtest traffic (reduces persistent allocations and goroutines).
	DisableHTTP2 bool `json:"disable_http2"`
	// DisableKeepAlives disables HTTP keep-alives for speedtest traffic (encourages prompt connection close).
	DisableKeepAlives bool `json:"disable_keepalives"`

	// PacketLossEnabled toggles packet loss probing (extra network work).
	PacketLossEnabled bool `json:"packet_loss_enabled"`
	// PacketLossTimeoutSeconds bounds packet loss probing (0 => default).
	PacketLossTimeoutSeconds int `json:"packet_loss_timeout_seconds"`

	// PostRunGC enables a GC at the end of a speedtest run.
	PostRunGC bool `json:"post_run_gc"`
	// PostRunFreeOSMemory calls debug.FreeOSMemory after the run (heavier, but helps RSS drop).
	PostRunFreeOSMemory bool `json:"post_run_free_os_memory"`

	taskTimeout      time.Duration `json:"-"`
	operationTimeout time.Duration `json:"-"`
	packetLossTO     time.Duration `json:"-"`
}

// Plugin implements speedtest functionality
type Plugin struct {
	pluginkit.EnhancedPluginBase
	cfg      Config
	mu       sync.RWMutex
	autoTask string // last scheduled short name

	storeMu sync.Mutex
	store   *speedpkg.HistoryStore

	// runGate ensures speedtest runs (manual command + scheduled task) never overlap.
	// It is a 1-token semaphore.
	runGateOnce sync.Once
	runGate     chan struct{}
}
