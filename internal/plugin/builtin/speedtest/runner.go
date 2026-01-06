package speedtest

import (
	"context"
	"errors"
	"fmt"
	"time"

	speedpkg "pewbot/pkg/speedtest"
)

var ErrAlreadyRunning = errors.New("speedtest already running")

// runSpeedtest executes a speedtest run with concurrency + goroutine ownership
// under the plugin supervisor.
func (p *Plugin) runSpeedtest(ctx context.Context, source string) (*speedpkg.Result, string, error) {
	// Global run gate: prevent overlap between manual command and scheduled task.
	p.runGateOnce.Do(func() {
		p.runGate = make(chan struct{}, 1)
		p.runGate <- struct{}{}
	})
	select {
	case <-p.runGate:
		// acquired
	default:
		p.PublishEvent("speedtest.skipped_running", map[string]any{"source": source})
		return nil, "", ErrAlreadyRunning
	}
	defer func() { p.runGate <- struct{}{} }()

	startGate := time.Now()
	p.PublishEvent("speedtest.run.started", map[string]any{"source": source})
	defer func() {
		p.PublishEvent("speedtest.run.finished", map[string]any{"source": source, "held_ms": time.Since(startGate).Milliseconds()})
	}()

	cfg := p.getConfig()
	start := time.Now()

	// Bind this run to the plugin lifecycle so any internal goroutines exit on plugin stop.
	//
	// - Base context: plugin runtime context (canceled on disable/stop)
	// - Also cancel when the *caller* ctx ends (command/task timeout)
	// - And enforce an operation timeout
	callerCtx := ctx
	base := callerCtx
	if pctx := p.Context(); pctx != nil {
		base = pctx
	}
	opCtx, cancel := context.WithTimeout(base, cfg.operationTimeout)
	stopCallerCancel := context.AfterFunc(callerCtx, cancel)
	defer stopCallerCancel()
	defer cancel()
	ctx = opCtx

	spawner := speedpkg.SpawnerFunc(func(name string, fn func()) {
		if p.Runner != nil {
			p.Runner.Go0(name, func(_ context.Context) { fn() })
			return
		}
		fn()
	})

	runner := speedpkg.NewRunner(speedpkg.RunConfig{
		ServerCount:         cfg.ServerCount,
		FullTestServers:     cfg.FullTestServers,
		SavingMode:          cfg.SavingMode,
		MaxConnections:      cfg.MaxConnections,
		PostRunGC:           cfg.PostRunGC,
		PostRunFreeOSMemory: cfg.PostRunFreeOSMemory,
		OperationTimeout:    cfg.operationTimeout,
		PingConcurrency:     cfg.PingConcurrency,
		DisableHTTP2:        cfg.DisableHTTP2,
		DisableKeepAlives:   cfg.DisableKeepAlives,
		PacketLossEnabled:   cfg.PacketLossEnabled,
		PacketLossTimeout:   cfg.packetLossTO,
	}, speedpkg.WithSpawner(spawner))

	res, err := runner.Run(ctx)
	if err != nil {
		return nil, "", err
	}
	res.Duration = time.Since(start)

	msg := p.formatResult(res)
	return res, msg, nil
}

// formatResult is kept plugin-side because it's bot UX.
func (p *Plugin) formatResult(res *speedpkg.Result) string {
	if res == nil {
		return ""
	}
	serverCount := res.FullTestCount
	if serverCount <= 0 {
		serverCount = 1
	}
	return fmt.Sprintf(
		"🚀 Speedtest Results\n"+
			"━━━━━━━━━━━━━━━━━━━━\n"+
			"⬇️  Download: %.2f Mbps\n"+
			"⬆️  Upload: %.2f Mbps\n"+
			"📡 Ping: %.2f ms\n"+
			"📊 Jitter: %.2f ms\n"+
			"📦 Packet Loss: %.2f%%\n"+
			"🌐 ISP: %s\n"+
			"🖥️  Server: %s (%s)\n"+
			"⏱️  Duration: %.1fs | Servers: %d\n"+
			"🕐 Time: %s",
		res.DownloadMbps,
		res.UploadMbps,
		res.PingMs,
		res.Jitter,
		res.PacketLoss,
		res.ISP,
		res.ServerName,
		res.ServerCountry,
		res.Duration.Seconds(),
		serverCount,
		res.Timestamp.Format("2006-01-02 15:04:05"),
	)
}
