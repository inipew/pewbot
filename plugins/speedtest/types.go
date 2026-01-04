package speedtest

import (
	"sync"
	"time"

	"pewbot/internal/pluginkit"

	"github.com/showwin/speedtest-go/speedtest"
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

	// PostRunGC enables a lightweight GC at the end of a speedtest run.
	// Prefer fixing retention (client lifetime / Reset) before enabling this.
	PostRunGC bool `json:"post_run_gc"`

	taskTimeout      time.Duration `json:"-"`
	operationTimeout time.Duration `json:"-"`
}

// SpeedtestResult represents test results
type SpeedtestResult struct {
	Timestamp     time.Time `json:"timestamp"`
	DownloadMbps  float64   `json:"download_mbps"`
	UploadMbps    float64   `json:"upload_mbps"`
	PingMs        float64   `json:"ping_ms"`
	Jitter        float64   `json:"jitter"`
	PacketLoss    float64   `json:"packet_loss"`
	ISP           string    `json:"isp"`
	ServerName    string    `json:"server_name"`
	ServerCountry string    `json:"server_country"`
}

// serverTestResult contains individual server test data
type serverTestResult struct {
	Server   *speedtest.Server
	Download float64
	Upload   float64
	Ping     time.Duration
}

// DailyStats contains 24-hour statistics
type DailyStats struct {
	Period        string    `json:"period"`
	TestCount     int       `json:"test_count"`
	AvgDownload   float64   `json:"avg_download_mbps"`
	AvgUpload     float64   `json:"avg_upload_mbps"`
	AvgPing       float64   `json:"avg_ping_ms"`
	MaxDownload   float64   `json:"max_download_mbps"`
	MinDownload   float64   `json:"min_download_mbps"`
	MaxUpload     float64   `json:"max_upload_mbps"`
	MinUpload     float64   `json:"min_upload_mbps"`
	MaxPing       float64   `json:"max_ping_ms"`
	MinPing       float64   `json:"min_ping_ms"`
	AvgPacketLoss float64   `json:"avg_packet_loss"`
	FirstTest     time.Time `json:"first_test"`
	LastTest      time.Time `json:"last_test"`
}

// Plugin implements speedtest functionality
type Plugin struct {
	pluginkit.EnhancedPluginBase
	cfg       Config
	historyMu sync.Mutex // serialize history_file access
	mu        sync.RWMutex
	autoTask  string // last scheduled short name

	// runGate ensures speedtest runs (manual command + scheduled task) never overlap.
	// It is a 1-token semaphore.
	runGateOnce sync.Once
	runGate     chan struct{}
}
