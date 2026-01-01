package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

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

	<-ctx.Done()
	_ = app.Stop(context.Background())
}
