package speedtest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/showwin/speedtest-go/speedtest"
)

type idleConnCloser interface {
	CloseIdleConnections()
}

type pingResult struct {
	Server *speedtest.Server
	Err    error
}

var ErrAlreadyRunning = errors.New("speedtest already running")

func (p *Plugin) runSpeedtest(ctx context.Context, source string) (*SpeedtestResult, string, error) {
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

	// Use a dedicated HTTP transport for speedtest, then close idle connections
	// when the run finishes. This reduces the chance of lingering net/http
	// persistConn goroutines across runs.
	hc, tr := newSpeedtestHTTPClient(cfg)
	defer func() {
		if tr != nil {
			tr.CloseIdleConnections()
		}
		// Also close idles on the default transport (some library paths may still
		// fall back to it).
		closeIdleConnections(http.DefaultTransport)
	}()

	// IMPORTANT:
	// Don't use package-level speedtest.Fetch* helpers. speedtest-go keeps a
	// package-level default client (with a DataManager) that can retain large
	// snapshots/chunks across runs.
	st := speedtest.New(speedtest.WithUserConfig(&speedtest.UserConfig{
		SavingMode:     cfg.SavingMode,
		MaxConnections: cfg.MaxConnections,
	}))
	// Best-effort: if the library exposes a way to set a custom client, use it.
	applyHTTPClient(st, hc)
	// Be extra defensive: some speedtest-go paths use the manager thread count.
	if cfg.MaxConnections > 0 {
		st.SetNThread(cfg.MaxConnections)
	}
	defer func() {
		// Try to aggressively drop snapshots/chunks before returning.
		st.Snapshots().Clean()
		st.Reset()
		if cfg.PostRunGC {
			runtime.GC()
		}
	}()

	user, err := st.FetchUserInfoContext(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("fetch user info: %w", err)
	}

	servers, err := st.FetchServerListContext(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("fetch server list: %w", err)
	}
	if a := servers.Available(); a != nil {
		servers = *a
	}
	if len(servers) == 0 {
		return nil, "", fmt.Errorf("no servers available")
	}

	// Take the closest N candidates by distance first (cheap), then ping those.
	sort.Slice(servers, func(i, j int) bool {
		return servers[i].Distance < servers[j].Distance
	})

	candidateN := cfg.ServerCount
	if candidateN <= 0 {
		candidateN = 5
	}
	if candidateN > len(servers) {
		candidateN = len(servers)
	}
	candidates := servers[:candidateN]

	// Ping candidates (low memory) with small concurrency.
	pinged, err := p.pingCandidates(ctx, candidates, 4)
	if err != nil {
		return nil, "", err
	}
	if len(pinged) == 0 {
		return nil, "", fmt.Errorf("all latency tests failed")
	}

	// Sort by latency (best first).
	sort.Slice(pinged, func(i, j int) bool {
		return pinged[i].Latency < pinged[j].Latency
	})
	fullN := cfg.FullTestServers
	if fullN <= 0 {
		fullN = 1
	}
	if fullN > len(pinged) {
		fullN = len(pinged)
	}
	fullSet := pinged[:fullN]

	// Run full download/upload test sequentially on the best N.
	// This avoids large concurrent allocations and reduces peak memory usage.
	fullResults := make([]serverTestResult, 0, len(fullSet))
	for _, s := range fullSet {
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		default:
		}

		p.Log.Debug("speedtest: full test server",
			slog.String("name", s.Sponsor),
			slog.String("country", s.Country),
			slog.Float64("distance_km", s.Distance),
			slog.Int64("ping_ms", s.Latency.Milliseconds()),
		)

		// Download + upload using context-aware calls.
		if err := s.DownloadTestContext(ctx); err != nil {
			p.Log.Warn("download test failed",
				slog.String("server", s.Name),
				slog.String("host", s.Host),
				slog.Any("err", err),
			)
			continue
		}
		dl := s.DLSpeed.Mbps()

		if err := s.UploadTestContext(ctx); err != nil {
			p.Log.Warn("upload test failed",
				slog.String("server", s.Name),
				slog.String("host", s.Host),
				slog.Any("err", err),
			)
			continue
		}
		ul := s.ULSpeed.Mbps()

		fullResults = append(fullResults, serverTestResult{
			Server:   s,
			Download: dl,
			Upload:   ul,
			Ping:     s.Latency,
		})

		// Drop per-test snapshots/chunks early.
		st.Snapshots().Clean()
		st.Reset()
	}

	if len(fullResults) == 0 {
		return nil, "", fmt.Errorf("full test failed for all servers")
	}

	avg := p.calculateAverage(fullResults)
	chosen := p.findBest(fullResults)
	if chosen == nil {
		chosen = &fullResults[0]
	}

	packetLoss := p.packetLoss(ctx, chosen.Server.Host)

	// Prefer jitter from the chosen server if available; fallback to a rough estimate.
	jitterMs := float64(chosen.Server.Jitter.Milliseconds())
	if jitterMs <= 0 {
		jitterMs = math.Max(0.1, float64(avg.Ping.Milliseconds())*0.1)
	}

	res := &SpeedtestResult{
		Timestamp:     time.Now(),
		DownloadMbps:  avg.Download,
		UploadMbps:    avg.Upload,
		PingMs:        float64(avg.Ping.Milliseconds()),
		Jitter:        jitterMs,
		PacketLoss:    packetLoss,
		ISP:           user.Isp,
		ServerName:    chosen.Server.Sponsor,
		ServerCountry: chosen.Server.Country,
	}

	dur := time.Since(start)
	msg := p.formatResult(res, len(fullResults), dur)

	p.Log.Info("Speedtest completed",
		slog.Float64("download_mbps", res.DownloadMbps),
		slog.Float64("upload_mbps", res.UploadMbps),
		slog.Float64("ping_ms", res.PingMs),
		slog.Float64("packet_loss", res.PacketLoss),
		slog.Int("candidates", candidateN),
		slog.Int("full_test_servers", len(fullResults)),
		slog.Float64("duration_sec", dur.Seconds()),
	)

	return res, msg, nil
}

func (p *Plugin) pingCandidates(ctx context.Context, servers []*speedtest.Server, maxConcurrent int) ([]*speedtest.Server, error) {
	if maxConcurrent <= 0 {
		maxConcurrent = 4
	}

	sem := make(chan struct{}, maxConcurrent)
	out := make(chan pingResult, len(servers))
	var wg sync.WaitGroup

	launch := func(name string, fn func()) {
		// Prefer the plugin supervisor so goroutines are owned (panic-safe + stop-bounded).
		if p.Runner != nil {
			p.Runner.Go0(name, func(_ context.Context) { fn() })
			return
		}
		// Fallback: no supervisor (shouldn't happen in normal runtime). Run inline.
		fn()
	}

	for i, s := range servers {
		s := s
		idx := i
		wg.Add(1)
		launch(fmt.Sprintf("speedtest.ping.%d", idx), func() {
			defer wg.Done()

			select {
			case <-ctx.Done():
				out <- pingResult{Server: s, Err: ctx.Err()}
				return
			case sem <- struct{}{}:
			}
			defer func() { <-sem }()

			// PingTestContext sets s.Latency / s.Jitter.
			err := s.PingTestContext(ctx, nil)
			out <- pingResult{Server: s, Err: err}
		})
	}

	wg.Wait()
	close(out)

	pinged := make([]*speedtest.Server, 0, len(servers))
	for r := range out {
		if r.Err != nil {
			continue
		}
		if r.Server == nil {
			continue
		}
		if r.Server.Latency <= 0 {
			continue
		}
		pinged = append(pinged, r.Server)
	}

	return pinged, nil
}

func (p *Plugin) packetLoss(ctx context.Context, host string) float64 {
	if host == "" {
		return 0
	}
	pla := speedtest.NewPacketLossAnalyzer(nil)
	pl, err := pla.RunMultiWithContext(ctx, []string{host})
	if err != nil || pl == nil {
		return 0
	}
	// LossPercent is already in 0..100.
	return pl.LossPercent()
}

func newSpeedtestHTTPClient(cfg Config) (*http.Client, *http.Transport) {
	// Keep the dial timeout reasonably short; the overall operation is already
	// bounded by cfg.operationTimeout.
	dialTimeout := 10 * time.Second
	if cfg.operationTimeout > 0 {
		// Cap dial timeout to half of the operation timeout so we fail fast.
		capTo := cfg.operationTimeout / 2
		if capTo < dialTimeout {
			dialTimeout = capTo
		}
		if dialTimeout < 2*time.Second {
			dialTimeout = 2 * time.Second
		}
	}

	perHost := cfg.MaxConnections
	if perHost <= 0 {
		perHost = 4
	}
	if perHost < 2 {
		perHost = 2
	}

	d := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           d.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   perHost,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	hc := &http.Client{Transport: tr}
	return hc, tr
}

func closeIdleConnections(rt http.RoundTripper) {
	if rt == nil {
		return
	}
	if c, ok := rt.(idleConnCloser); ok {
		c.CloseIdleConnections()
	}
}

// applyHTTPClient tries to configure a custom http.Client on the speedtest
// instance, if the library exposes any compatible setter.
//
// This is best-effort and intentionally reflection-free so it stays stable
// across speedtest-go versions.
func applyHTTPClient(st any, hc *http.Client) {
	if st == nil || hc == nil {
		return
	}
	// Common method names across Go libs.
	if s, ok := st.(interface{ SetHTTPClient(*http.Client) }); ok {
		s.SetHTTPClient(hc)
		return
	}
	if s, ok := st.(interface{ SetHttpClient(*http.Client) }); ok {
		s.SetHttpClient(hc)
		return
	}
	if s, ok := st.(interface{ SetClient(*http.Client) }); ok {
		s.SetClient(hc)
		return
	}
}

// calculateAverage computes average metrics across successful results.
func (p *Plugin) calculateAverage(results []serverTestResult) serverTestResult {
	if len(results) == 0 {
		return serverTestResult{}
	}

	var totalDL, totalUL float64
	var totalPing time.Duration

	for _, r := range results {
		totalDL += r.Download
		totalUL += r.Upload
		totalPing += r.Ping
	}

	count := len(results)
	return serverTestResult{
		Download: totalDL / float64(count),
		Upload:   totalUL / float64(count),
		Ping:     totalPing / time.Duration(count),
	}
}

// findBest identifies the best server result.
// Prioritize lower ping, then higher download speed.
func (p *Plugin) findBest(results []serverTestResult) *serverTestResult {
	if len(results) == 0 {
		return nil
	}

	best := &results[0]
	for i := 1; i < len(results); i++ {
		if results[i].Ping < best.Ping ||
			(results[i].Ping == best.Ping && results[i].Download > best.Download) {
			best = &results[i]
		}
	}
	return best
}
