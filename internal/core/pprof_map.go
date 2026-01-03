package core

import (
	"fmt"
	"net"
	"strings"
	"time"

	svc "pewbot/internal/services/pprof"
)

// mapPprofConfig validates and converts the JSON config into the service config.
// It never starts the server.
func mapPprofConfig(cfg *Config) (svc.Config, error) {
	var out svc.Config
	if cfg == nil {
		return out, nil
	}
	pc := cfg.Pprof

	out.Enabled = pc.Enabled
	out.AllowInsecure = pc.AllowInsecure
	out.Token = strings.TrimSpace(pc.Token)
	out.Addr = strings.TrimSpace(pc.Addr)
	out.Prefix = strings.TrimSpace(pc.Prefix)

	if out.Addr == "" {
		out.Addr = "127.0.0.1:6060"
	}
	if out.Prefix == "" {
		out.Prefix = "/debug/pprof/"
	}

	// durations
	readTO, err := parseDurationOrDefault("pprof.read_timeout", pc.ReadTimeout, 5*time.Second)
	if err != nil {
		return out, err
	}
	writeTO, err := parseDurationField("pprof.write_timeout", pc.WriteTimeout)
	if err != nil {
		return out, err
	}
	idleTO, err := parseDurationOrDefault("pprof.idle_timeout", pc.IdleTimeout, 120*time.Second)
	if err != nil {
		return out, err
	}
	out.ReadTimeout = readTO
	out.WriteTimeout = writeTO // default 0 (disabled)
	out.IdleTimeout = idleTO

	// runtime profiling rates
	if pc.MutexProfileFraction < 0 {
		return out, fmt.Errorf("pprof.mutex_profile_fraction must be >= 0")
	}
	if pc.BlockProfileRate < 0 {
		return out, fmt.Errorf("pprof.block_profile_rate must be >= 0")
	}
	if pc.MemProfileRate < 0 {
		return out, fmt.Errorf("pprof.mem_profile_rate must be >= 0")
	}
	out.MutexProfileFraction = pc.MutexProfileFraction
	out.BlockProfileRate = pc.BlockProfileRate
	out.MemProfileRate = pc.MemProfileRate

	// Validate addr format if enabled.
	if out.Enabled {
		if _, _, err := net.SplitHostPort(out.Addr); err != nil {
			return out, fmt.Errorf("pprof.addr: invalid %q (expected host:port): %w", out.Addr, err)
		}
		// Security: refuse public bind without explicit opt-in.
		if !out.AllowInsecure && out.Token == "" && !isLoopbackAddr(out.Addr) {
			return out, fmt.Errorf("pprof: binding to non-loopback addr requires token or allow_insecure=true")
		}
	}

	return out, nil
}

func isLoopbackAddr(addr string) bool {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	h = strings.TrimSpace(h)
	if h == "" {
		return false
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
