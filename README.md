# pewbot

Modular Telegram bot framework (single operator) dengan:
- Hot-reload JSON config
- Plugin system + per-plugin timeout
- Scheduler (one-time/interval/daily/weekly/cron) + worker pool + retry + history/statistics
- Broadcaster (concurrent + rate limit + retry + job progress) + group recipients
- Notifier (multi-channel + priority routes + history)
- Smart logging (console/file/Telegram sink + rate limit + min-level)
- Middleware (panic recovery + request logging)

## Quickstart

1) Copy config example:
```bash
cp config.example.json config.json
# isi telegram.token dan logging.telegram.chat_id
```

2) Run:
```bash
go mod tidy
go run ./cmd/bot -config ./config.json
```

## Core commands
- `/help`
- `/plugins`
- `/health`

## Plugin examples
- `plugins/system`: `/ping`, `/sysinfo`, `/notify_test`
- `plugins/echo`: `/echo hello`

## Add plugin baru
Buat folder `plugins/<name>/` dan implement `core.Plugin`.

Lihat `plugins/echo` sebagai template.
