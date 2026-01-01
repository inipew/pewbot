package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"pewbot/internal/core"
	"pewbot/plugins/echo"
	"pewbot/plugins/speedtest"
	"pewbot/plugins/system"
	"pewbot/plugins/systemd"
)

func main() {
	var cfgPath string
	flag.StringVar(&cfgPath, "config", "./config.json", "path to config json")
	flag.Parse()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	reasonCh := make(chan core.StopReason, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sig := <-sigCh
		reason := core.StopUnknown
		switch sig {
		case os.Interrupt:
			reason = core.StopSIGINT
		case syscall.SIGTERM:
			reason = core.StopSIGTERM
		}
		select { // non-blocking
		case reasonCh <- reason:
		default:
		}
		cancel()

		// second signal => hard exit
		<-sigCh
		fmt.Println("forced exit")
		os.Exit(1)
	}()
	defer signal.Stop(sigCh)

	app, err := core.NewApp(cfgPath)
	if err != nil {
		fmt.Println("fatal:", err)
		os.Exit(1)
	}

	// Register plugins (tambah plugin cukup New() + Register)
	app.Plugins().Register(
		echo.New(),
		system.New(),
		systemd.New(),
		speedtest.New(),
	)

	if err := app.Start(ctx); err != nil {
		fmt.Println("fatal start:", err)
		os.Exit(1)
	}

	reason := core.StopUnknown
	select {
	case <-ctx.Done():
		select {
		case reason = <-reasonCh:
		default:
		}
	case <-app.Done():
		if app.Err() != nil {
			reason = core.StopFatalError
		}
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = app.Stop(stopCtx, reason)
}
