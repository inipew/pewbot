package core

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"testing"
	"time"
)

func newDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func waitForHTTP(ctx context.Context, url string) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		reqCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, http.NoBody)
		if err != nil {
			cancel()
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil && resp != nil {
			_ = resp.Body.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func TestPprofServerApplyEnableDisable(t *testing.T) {
	log := newDiscardLogger()
	srv := newPprofServer(log)
	t.Cleanup(func() {
		srv.Stop(context.Background())
	})
	prevMutex := runtime.SetMutexProfileFraction(-1)
	t.Cleanup(func() {
		// Avoid leaking profiling knobs across tests.
		_ = runtime.SetMutexProfileFraction(prevMutex)
		runtime.SetBlockProfileRate(0)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cfg := PprofConfig{
		Enabled:              true,
		Address:              "127.0.0.1:0",
		BlockProfileRate:     1,
		MutexProfileFraction: 7,
	}
	srv.Apply(ctx, cfg)

	addr := srv.Addr()
	if addr == "" {
		t.Fatal("expected pprof server to expose address")
	}

	if err := waitForHTTP(ctx, "http://"+addr+"/debug/pprof/"); err != nil {
		t.Fatalf("pprof endpoint not reachable: %v", err)
	}

	if got := runtime.SetMutexProfileFraction(-1); got != cfg.MutexProfileFraction {
		t.Fatalf("mutex profile fraction = %d, want %d", got, cfg.MutexProfileFraction)
	}

	// Disable and ensure listener shuts down.
	srv.Apply(ctx, PprofConfig{Enabled: false})
	if addr := srv.Addr(); addr != "" {
		t.Fatalf("expected pprof server to stop, still at %s", addr)
	}
}
