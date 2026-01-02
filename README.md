# pewbot

Modular Telegram bot framework (single operator) dengan:
- Hot-reload JSON config
- Plugin system + per-plugin timeout (duration string)
- Scheduler (one-time/interval/daily/weekly/cron) + worker pool + retry + history/statistics
- Broadcaster (concurrent + rate limit + retry + job progress) + group recipients
- Notifier (multi-channel + priority routes + history)
- Smart logging (console/file/Telegram sink + rate limit + min-level)
- Middleware (panic recovery + request logging)

## Quickstart

1) Copy config example:
```bash
cp config.example.json config.json
```

2) Edit `config.json`:
- `telegram.token`: token bot Telegram
- `telegram.owner_user_ids`: daftar user id operator (boleh lebih dari satu)
- `telegram.group_log`: (opsional) chat id group/chan untuk sink log Telegram (contoh: `-1001234567890`)
- `logging.telegram.thread_id`: (opsional) thread id untuk topik (0 = tidak pakai topik)
- Semua timeout memakai **duration string** Go, contoh: `"10s"`, `"2m"`, `"500ms"`.

3) Run:
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
