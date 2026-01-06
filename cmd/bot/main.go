package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	botapp "pewbot/internal/app"
	"pewbot/internal/plugin/builtin/echo"
	"pewbot/internal/plugin/builtin/speedtest"
	"pewbot/internal/plugin/builtin/system"
	"pewbot/internal/plugin/builtin/systemd"
)

func main() {
	configFlag := flag.String("config", "", "path to config (json/yaml) (optional)")
	flag.Parse()

	cfgPath := ""
	if *configFlag != "" {
		cfgPath = *configFlag
	} else if env := os.Getenv("PEWBOT_CONFIG"); env != "" {
		cfgPath = env
	} else {
		candidates := []string{
			"config.local.yaml",
			"config.local.yml",
			"config.local.json",
			"config.yaml",
			"config.yml",
			"config.json",
			"configs/config.local.yaml",
			"configs/config.local.yml",
			"configs/config.local.json",
			"configs/config.yaml",
			"configs/config.yml",
			"configs/config.json",
			"configs/config.example.yaml",
			"configs/config.example.yml",
			"configs/config.example.json",
		}
		for _, p := range candidates {
			if _, err := os.Stat(p); err == nil {
				cfgPath = p
				break
			}
		}
		if cfgPath == "" {
			cfgPath = "configs/config.example.yaml"
		}
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	reasonCh := make(chan botapp.StopReason, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sig := <-sigCh
		reason := botapp.StopUnknown
		switch sig {
		case os.Interrupt:
			reason = botapp.StopSIGINT
		case syscall.SIGTERM:
			reason = botapp.StopSIGTERM
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

	bot, err := botapp.NewApp(cfgPath)
	if err != nil {
		fmt.Println("fatal:", err)
		os.Exit(1)
	}

	// Register plugins
	bot.Plugins().Register(
		echo.New(),
		system.New(),
		speedtest.New(),
	)
	if runtime.GOOS == "linux" {
		bot.Plugins().Register(systemd.New())
	}

	if err := bot.Start(ctx); err != nil {
		fmt.Println("fatal start:", err)
		os.Exit(1)
	}

	reason := botapp.StopUnknown
	select {
	case <-ctx.Done():
		select {
		case reason = <-reasonCh:
		default:
		}
	case <-bot.Done():
		if bot.Err() != nil {
			reason = botapp.StopFatalError
		}
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = bot.Stop(stopCtx, reason)
}
