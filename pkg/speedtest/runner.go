package speedtest

import (
	"context"
	"crypto/tls"
	"fmt"
	"math"
	"net"
	"net/http"
	"reflect"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	st "github.com/showwin/speedtest-go/speedtest"
)

// RunConfig controls how a speedtest run is executed.
type RunConfig struct {
	// Candidate servers to consider (sorted by distance, then pinged).
	ServerCount int
	// Number of lowest-latency servers to run a full download/upload test on.
	// Full tests are executed sequentially to reduce peak memory usage.
	FullTestServers int

	// UserConfig passed to speedtest-go.
	SavingMode     bool
	MaxConnections int

	// PostRunGC triggers a best-effort GC after a run.
	PostRunGC bool
	// PostRunFreeOSMemory calls debug.FreeOSMemory after the run (heavier, but helps RSS drop).
	PostRunFreeOSMemory bool

	// OperationTimeout is used for internal HTTP dial timeout heuristics.
	// It does NOT automatically wrap the provided context.
	OperationTimeout time.Duration

	// PingConcurrency caps how many ping tests run concurrently.
	PingConcurrency int

	// DisableHTTP2 prevents HTTP/2 for speedtest traffic (reduces persistent allocations and goroutines).
	DisableHTTP2 bool
	// DisableKeepAlives disables HTTP keep-alives for speedtest traffic (encourages connections to close promptly).
	DisableKeepAlives bool

	// PacketLossEnabled toggles packet loss probing (extra network work).
	PacketLossEnabled bool
	// PacketLossTimeout bounds packet loss probing.
	PacketLossTimeout time.Duration
}

// Runner executes speedtests.
type Runner struct {
	cfg     RunConfig
	spawner Spawner
}

// Option customizes a Runner.
type Option func(*Runner)

// WithSpawner makes the runner use the provided spawner for internal goroutines
// (e.g. ping concurrency), enabling ownership under a supervisor.
func WithSpawner(s Spawner) Option { return func(r *Runner) { r.spawner = s } }

// NewRunner constructs a Runner.
func NewRunner(cfg RunConfig, opts ...Option) *Runner {
	r := &Runner{cfg: cfg}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Run executes a single speedtest run.
func (r *Runner) Run(ctx context.Context) (*Result, error) {
	if ctx == nil {
		return nil, fmt.Errorf("nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cfg := r.cfg
	if cfg.ServerCount <= 0 {
		cfg.ServerCount = 5
	}
	if cfg.FullTestServers <= 0 {
		cfg.FullTestServers = 1
	}
	if cfg.FullTestServers > cfg.ServerCount {
		cfg.FullTestServers = cfg.ServerCount
	}
	if cfg.MaxConnections <= 0 {
		cfg.MaxConnections = 4
	}
	if cfg.PingConcurrency <= 0 {
		cfg.PingConcurrency = 4
	}
	if cfg.PacketLossTimeout <= 0 {
		cfg.PacketLossTimeout = 3 * time.Second
	}

	// Always cancel a derived context when we leave this function.
	// This helps ensure any library goroutines that honor context exit promptly.
	runCtx, cancelRun := context.WithCancel(ctx)
	ctx = runCtx

	start := time.Now()

	// Dedicated HTTP transport so we can isolate and aggressively clean up connections after a run.
	hc, tr := newHTTPClient(cfg)

	// IMPORTANT: avoid package-level speedtest helpers; speedtest-go can keep package-level state.
	stc := st.New(st.WithUserConfig(&st.UserConfig{
		SavingMode:     cfg.SavingMode,
		MaxConnections: cfg.MaxConnections,
	}))
	applyHTTPClient(stc, hc)
	stc.SetNThread(cfg.MaxConnections)

	defer func() {
		// 1) Cancel first: helps any in-flight library goroutines unwind.
		cancelRun()

		// 2) Best-effort library cleanup.
		stc.Snapshots().Clean()
		stc.Reset()

		// 3) Connection cleanup.
		if tr != nil {
			tr.CloseIdleConnections()
		}

		// 4) Optional memory reclamation.
		if cfg.PostRunFreeOSMemory {
			debug.FreeOSMemory()
		}
	}()

	user, err := stc.FetchUserInfoContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch user info: %w", err)
	}

	servers, err := stc.FetchServerListContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch server list: %w", err)
	}
	if a := servers.Available(); a != nil {
		servers = *a
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("no servers available")
	}

	// Cheap filter: distance.
	sort.Slice(servers, func(i, j int) bool { return servers[i].Distance < servers[j].Distance })
	candidateN := cfg.ServerCount
	if candidateN > len(servers) {
		candidateN = len(servers)
	}
	candidates := servers[:candidateN]

	pinged, err := r.pingCandidates(ctx, candidates, cfg.PingConcurrency)
	if err != nil {
		return nil, err
	}
	if len(pinged) == 0 {
		return nil, fmt.Errorf("all latency tests failed")
	}

	// Best first.
	sort.Slice(pinged, func(i, j int) bool { return pinged[i].Latency < pinged[j].Latency })
	fullN := cfg.FullTestServers
	if fullN > len(pinged) {
		fullN = len(pinged)
	}
	fullSet := pinged[:fullN]

	fullResults := make([]serverTestResult, 0, len(fullSet))
	for _, s := range fullSet {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if err := s.DownloadTestContext(ctx); err != nil {
			continue
		}
		dl := s.DLSpeed.Mbps()

		if err := s.UploadTestContext(ctx); err != nil {
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
		stc.Snapshots().Clean()
		stc.Reset()
	}
	if len(fullResults) == 0 {
		return nil, fmt.Errorf("full test failed for all servers")
	}

	avg := calculateAverage(fullResults)
	chosen := findBest(fullResults)
	if chosen == nil {
		chosen = &fullResults[0]
	}

	pl := 0.0
	if cfg.PacketLossEnabled {
		host := chosen.Server.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		plCtx, cancel := context.WithTimeout(ctx, cfg.PacketLossTimeout)
		pl = packetLoss(plCtx, host)
		cancel()
	}

	// Prefer jitter from chosen server; fallback to a rough estimate.
	jitterMs := float64(chosen.Server.Jitter.Milliseconds())
	if jitterMs <= 0 {
		jitterMs = math.Max(0.1, float64(avg.Ping.Milliseconds())*0.1)
	}

	dur := time.Since(start)
	res := &Result{
		Timestamp:      time.Now(),
		DownloadMbps:   avg.Download,
		UploadMbps:     avg.Upload,
		PingMs:         float64(avg.Ping.Milliseconds()),
		Jitter:         jitterMs,
		PacketLoss:     pl,
		ISP:            user.Isp,
		ServerName:     chosen.Server.Sponsor,
		ServerCountry:  chosen.Server.Country,
		Duration:       dur,
		CandidateCount: candidateN,
		FullTestCount:  len(fullResults),
	}

	return res, nil
}

type pingResult struct {
	Server *st.Server
	Err    error
}

func (r *Runner) pingCandidates(ctx context.Context, servers []*st.Server, maxConcurrent int) ([]*st.Server, error) {
	if maxConcurrent <= 0 {
		maxConcurrent = 4
	}

	sem := make(chan struct{}, maxConcurrent)
	out := make(chan pingResult, len(servers))
	var wg sync.WaitGroup

	launch := func(name string, fn func()) {
		if r.spawner != nil {
			r.spawner.Go(name, fn)
			return
		}
		go fn()
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

			err := s.PingTestContext(ctx, nil)
			out <- pingResult{Server: s, Err: err}
		})
	}

	wg.Wait()
	close(out)

	pinged := make([]*st.Server, 0, len(servers))
	for pr := range out {
		if pr.Err != nil {
			continue
		}
		if pr.Server == nil {
			continue
		}
		if pr.Server.Latency <= 0 {
			continue
		}
		pinged = append(pinged, pr.Server)
	}
	return pinged, nil
}

type serverTestResult struct {
	Server   *st.Server
	Download float64
	Upload   float64
	Ping     time.Duration
}

func calculateAverage(results []serverTestResult) serverTestResult {
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

// findBest prioritizes lower ping, then higher download speed.
func findBest(results []serverTestResult) *serverTestResult {
	if len(results) == 0 {
		return nil
	}
	best := &results[0]
	for i := 1; i < len(results); i++ {
		if results[i].Ping < best.Ping || (results[i].Ping == best.Ping && results[i].Download > best.Download) {
			best = &results[i]
		}
	}
	return best
}

func packetLoss(ctx context.Context, host string) float64 {
	if host == "" {
		return 0
	}
	pla := st.NewPacketLossAnalyzer(nil)
	pl, err := pla.RunMultiWithContext(ctx, []string{host})
	if err != nil || pl == nil {
		return 0
	}
	return pl.LossPercent()
}

type idleConnCloser interface{ CloseIdleConnections() }

func newHTTPClient(cfg RunConfig) (*http.Client, *http.Transport) {
	dialTimeout := 10 * time.Second
	if cfg.OperationTimeout > 0 {
		capTo := cfg.OperationTimeout / 2
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

	keepAlive := 30 * time.Second
	if cfg.DisableKeepAlives {
		// A negative KeepAlive means "disable" for net.Dialer.
		keepAlive = -1
	}

	d := &net.Dialer{Timeout: dialTimeout, KeepAlive: keepAlive}

	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           d.DialContext,
		MaxIdleConns:          0,
		MaxIdleConnsPerHost:   0,
		IdleConnTimeout:       2 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableKeepAlives:     cfg.DisableKeepAlives,
		ForceAttemptHTTP2:     !cfg.DisableHTTP2,
	}

	if cfg.DisableHTTP2 {
		// Force HTTP/1.1 only.
		tr.ForceAttemptHTTP2 = false
		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}

	// If keep-alives are enabled, allow a small idle pool during the run (we will still CloseIdleConnections on exit).
	if !cfg.DisableKeepAlives {
		tr.MaxIdleConns = 64
		tr.MaxIdleConnsPerHost = perHost
		tr.IdleConnTimeout = 10 * time.Second
	}

	return &http.Client{Transport: tr}, tr
}

// applyHTTPClient best-effort config of a custom http.Client on the speedtest instance.
func applyHTTPClient(stc any, hc *http.Client) {
	if stc == nil || hc == nil {
		return
	}

	// Try common setter methods first.
	if s, ok := stc.(interface{ SetHTTPClient(*http.Client) }); ok {
		s.SetHTTPClient(hc)
		return
	}
	if s, ok := stc.(interface{ SetHttpClient(*http.Client) }); ok {
		s.SetHttpClient(hc)
		return
	}
	if s, ok := stc.(interface{ SetClient(*http.Client) }); ok {
		s.SetClient(hc)
		return
	}

	// Fall back to reflection for common exported fields.
	v := reflect.ValueOf(stc)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return
	}
	e := v.Elem()
	if !e.IsValid() || e.Kind() != reflect.Struct {
		return
	}

	for _, name := range []string{"HTTPClient", "HttpClient", "Client"} {
		f := e.FieldByName(name)
		if !f.IsValid() || !f.CanSet() {
			continue
		}
		if f.Type().AssignableTo(reflect.TypeOf((*http.Client)(nil))) {
			f.Set(reflect.ValueOf(hc))
			return
		}
		if f.Type() == reflect.TypeOf(http.Client{}) {
			f.Set(reflect.ValueOf(*hc))
			return
		}
	}
}
