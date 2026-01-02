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
	ServerCount int                           `json:"server_count"`

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

// SpeedtestHistory stores all historical results
type SpeedtestHistory struct {
	Results []SpeedtestResult `json:"results"`
	mu      sync.RWMutex
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
	cfg      Config
	history  *SpeedtestHistory
	mu       sync.RWMutex
	autoTask string // last scheduled short name
}
