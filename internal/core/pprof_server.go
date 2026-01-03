package core

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"runtime"
	"sync"
	"time"
)

// PprofConfig controls the optional pprof HTTP server.
type PprofConfig struct {
	Enabled              bool   `json:"enabled"`
	Address              string `json:"address"`
	BlockProfileRate     int    `json:"block_profile_rate"`
	MutexProfileFraction int    `json:"mutex_profile_fraction"`
}

func (c PprofConfig) withDefaults() PprofConfig {
	if c.Address == "" {
		c.Address = "127.0.0.1:6060"
	}
	return c
}

// pprofServer manages lifecycle for the debug HTTP listener.
type pprofServer struct {
	mu   sync.Mutex
	log  *slog.Logger
	srv  *http.Server
	ln   net.Listener
	addr string
}

func newPprofServer(log *slog.Logger) *pprofServer {
	if log == nil {
		log = slog.Default()
	}
	return &pprofServer{log: log.With(slog.String("comp", "pprof"))}
}

// Apply starts/stops the pprof server according to cfg and updates profile rates.
func (p *pprofServer) Apply(ctx context.Context, cfg PprofConfig) {
	cfg = cfg.withDefaults()

	// Update global profiling knobs even if server is disabled.
	runtime.SetBlockProfileRate(cfg.BlockProfileRate)
	runtime.SetMutexProfileFraction(cfg.MutexProfileFraction)

	p.mu.Lock()
	defer p.mu.Unlock()

	if !cfg.Enabled {
		p.stopLocked(ctx)
		return
	}

	if p.srv != nil && p.addr == cfg.Address {
		return
	}

	p.stopLocked(ctx)
	p.startLocked(ctx, cfg)
}

func (p *pprofServer) startLocked(ctx context.Context, cfg PprofConfig) {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	srv := &http.Server{Addr: cfg.Address, Handler: mux}
	ln, err := net.Listen("tcp", cfg.Address)
	if err != nil {
		p.log.Warn("pprof listen failed", slog.String("addr", cfg.Address), slog.String("err", err.Error()))
		return
	}

	p.srv = srv
	p.ln = ln
	p.addr = ln.Addr().String()

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.log.Warn("pprof server error", slog.String("addr", p.addr), slog.String("err", err.Error()))
		}
	}()
	p.log.Info("pprof enabled", slog.String("addr", p.addr))
}

// Stop gracefully shuts down the pprof server.
func (p *pprofServer) Stop(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked(ctx)
}

func (p *pprofServer) stopLocked(ctx context.Context) {
	if p.srv == nil {
		return
	}
	srv := p.srv
	ln := p.ln
	p.srv = nil
	p.ln = nil
	addr := p.addr
	p.addr = ""

	shutdownCtx := ctx
	if shutdownCtx == nil {
		var cancel context.CancelFunc
		shutdownCtx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
	}

	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		p.log.Warn("pprof shutdown error", slog.String("addr", addr), slog.String("err", err.Error()))
	}
	if ln != nil {
		_ = ln.Close()
	}
	p.log.Info("pprof disabled", slog.String("addr", addr))
}

// Addr reports the actual listen address if running.
func (p *pprofServer) Addr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.addr
}
