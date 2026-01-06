package speedtest

import "time"

// Result is a single speedtest measurement.
//
// IMPORTANT: JSON tags are kept stable because results are persisted to the
// history file (NDJSON). Changing tags can break existing history.
type Result struct {
	Timestamp     time.Time `json:"timestamp"`
	DownloadMbps  float64   `json:"download_mbps"`
	UploadMbps    float64   `json:"upload_mbps"`
	PingMs        float64   `json:"ping_ms"`
	Jitter        float64   `json:"jitter"`
	PacketLoss    float64   `json:"packet_loss"`
	ISP           string    `json:"isp"`
	ServerName    string    `json:"server_name"`
	ServerCountry string    `json:"server_country"`

	// Non-persisted fields (useful for formatting / debugging).
	Duration       time.Duration `json:"-"`
	CandidateCount int           `json:"-"`
	FullTestCount  int           `json:"-"`
}

// DailyStats is a simple rolling statistics structure.
// This mirrors what the speedtest plugin shows in /speedtest stats.
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
