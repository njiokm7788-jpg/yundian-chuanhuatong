CREATE TABLE IF NOT EXISTS users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT UNIQUE NOT NULL,
  pass_hash TEXT NOT NULL,
  salt TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'user',
  plan TEXT NOT NULL DEFAULT 'trial',
  class_quota INTEGER NOT NULL DEFAULT 1,
  expire_at INTEGER,
  status TEXT NOT NULL DEFAULT 'active',
  created_at INTEGER NOT NULL,
  last_login INTEGER
);

CREATE TABLE IF NOT EXISTS sessions (
  token TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  expire_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS classes (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  name TEXT NOT NULL,
  note TEXT DEFAULT '',
  pair_code TEXT UNIQUE NOT NULL,
  device_id TEXT,
  device_name TEXT,
  device_ver TEXT,
  last_seen INTEGER DEFAULT 0,
  sort INTEGER DEFAULT 0,
  display_seconds INTEGER DEFAULT 20,
  font_size INTEGER DEFAULT 0,
  theme TEXT DEFAULT 'blue',
  repeat INTEGER DEFAULT 1,
  voice_rate INTEGER DEFAULT 10,
  show_clock INTEGER DEFAULT 1,
  beep INTEGER DEFAULT 1,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_classes_user ON classes(user_id);

CREATE TABLE IF NOT EXISTS device_tokens (
  token TEXT PRIMARY KEY,
  class_id INTEGER NOT NULL,
  user_id INTEGER NOT NULL DEFAULT 0,
  name TEXT DEFAULT '',
  device_id TEXT,
  created_at INTEGER NOT NULL,
  last_seen INTEGER
);

CREATE TABLE IF NOT EXISTS messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  class_id INTEGER NOT NULL,
  text TEXT NOT NULL,
  audio_key TEXT,
  voice INTEGER DEFAULT 1,
  repeat INTEGER DEFAULT 1,
  kind TEXT DEFAULT 'manual',
  status TEXT DEFAULT 'sent',
  ack_at INTEGER,
  played_at INTEGER,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_msg_class ON messages(class_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_msg_user ON messages(user_id, id DESC);

CREATE TABLE IF NOT EXISTS templates (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  title TEXT NOT NULL,
  text TEXT NOT NULL,
  sort INTEGER DEFAULT 0,
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS schedules (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  class_id INTEGER NOT NULL,
  text TEXT NOT NULL,
  time_hhmm TEXT NOT NULL,
  weekdays TEXT NOT NULL DEFAULT '1,2,3,4,5',
  mode TEXT NOT NULL DEFAULT 'daily',
  once_at INTEGER,
  enabled INTEGER DEFAULT 1,
  next_at INTEGER NOT NULL,
  last_run INTEGER,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sched_next ON schedules(enabled, next_at);

CREATE TABLE IF NOT EXISTS codes (
  code TEXT PRIMARY KEY,
  plan TEXT NOT NULL DEFAULT 'basic',
  class_bonus INTEGER NOT NULL DEFAULT 0,
  days INTEGER NOT NULL DEFAULT 0,
  batch TEXT,
  used_by INTEGER,
  used_at INTEGER,
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER,
  action TEXT NOT NULL,
  detail TEXT,
  ip TEXT,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_logs_time ON logs(created_at DESC);
