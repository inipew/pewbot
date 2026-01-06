module pewbot

// NOTE: Keep this pinned to the locally available toolchain to avoid
// Go's auto toolchain download (which breaks in offline environments).
go 1.24.0

require (
	github.com/coreos/go-systemd/v22 v22.6.0
	github.com/fsnotify/fsnotify v1.9.0
	github.com/robfig/cron/v3 v3.0.1
	github.com/rs/zerolog v1.34.0
	github.com/showwin/speedtest-go v1.7.10
	go.yaml.in/yaml/v3 v3.0.4
	golang.org/x/time v0.14.0
	gopkg.in/telebot.v4 v4.0.0-beta.7
	modernc.org/sqlite v1.42.2
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/godbus/dbus/v5 v5.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v0.1.9 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/exp v0.0.0-20250620022241-b7579e27df2b // indirect
	golang.org/x/sys v0.39.0 // indirect
	modernc.org/libc v1.66.10 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
