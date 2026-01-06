# pewbot

Modular Telegram bot framework (single operator) dengan:
- Hot-reload JSON/YAML config
- Plugin system + per-plugin timeout (duration string)
- Scheduler (one-time/interval/daily/weekly/cron) + worker pool + retry + history/statistics
- Notifier (multi-channel + priority routes + history)
- Smart logging (console/file/Telegram sink + rate limit + min-level)
- Middleware (panic recovery + request logging)
- Optional pprof HTTP server (diagnostics; configurable + safe-by-default)

## Quickstart

1) Copy config example (YAML direkomendasikan; JSON juga didukung):
```bash
cp configs/config.example.yaml config.yaml
# atau:
# cp configs/config.example.json config.json
```

2) Edit `config.yaml` (atau `config.json`):
- `telegram.token`: token bot Telegram
- `telegram.owner_user_ids`: daftar user id operator (boleh lebih dari satu)
- `telegram.group_log`: (opsional) chat id group/chan untuk sink log Telegram (contoh: `-1001234567890`)
- `logging.telegram.thread_id`: (opsional) thread id untuk topik (0 = tidak pakai topik)
- Semua timeout memakai **duration string** Go, contoh: `"10s"`, `"2m"`, `"500ms"`.

3) Run:
```bash
go mod tidy
go run ./cmd/bot -config ./config.yaml
```

## pprof (optional)

Aktifkan di config:

- `pprof.enabled`: true/false
- `pprof.addr`: default `127.0.0.1:6060`
- `pprof.prefix`: default `/debug/pprof/`
- `pprof.token`: (opsional) Bearer token. Jika bind non-loopback, token wajib kecuali `allow_insecure=true`.

Contoh akses:
- `http://127.0.0.1:6060/debug/pprof/`
- dengan token: kirim `Authorization: Bearer <token>` atau `?token=<token>`

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
