-- pewbot storage schema (minimal)

CREATE TABLE IF NOT EXISTS audit (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  at TEXT NOT NULL,
  actor_id INTEGER NOT NULL,
  actor_username TEXT,
  chat_id INTEGER NOT NULL,
  thread_id INTEGER NOT NULL,
  plugin TEXT NOT NULL,
  action TEXT NOT NULL,
  target TEXT NOT NULL,
  ok INTEGER NOT NULL,
  fail INTEGER NOT NULL,
  err TEXT,
  took_ms INTEGER NOT NULL,
  meta TEXT
);

CREATE INDEX IF NOT EXISTS idx_audit_at ON audit(at);

CREATE TABLE IF NOT EXISTS dedup (
  key TEXT PRIMARY KEY,
  until INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_dedup_until ON dedup(until);
