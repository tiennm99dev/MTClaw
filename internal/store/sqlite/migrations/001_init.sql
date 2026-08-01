CREATE TABLE sessions (
  id                TEXT PRIMARY KEY,          -- ulid-ish, sortable
  channel           TEXT NOT NULL,             -- 'telegram' | 'cli' | 'cron'
  chat_id           TEXT NOT NULL,
  thread_id         TEXT NOT NULL DEFAULT '',  -- forum topic; '' when absent
  title             TEXT NOT NULL DEFAULT '',
  model             TEXT NOT NULL DEFAULT '',
  summary           TEXT NOT NULL DEFAULT '',  -- compaction target
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL,          -- unix millis
  updated_at        INTEGER NOT NULL,
  UNIQUE (channel, chat_id, thread_id)
);

CREATE TABLE messages (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id   TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  seq          INTEGER NOT NULL,               -- per-session monotonic
  role         TEXT NOT NULL,                  -- system|user|assistant|tool
  content      TEXT NOT NULL DEFAULT '',
  tool_calls   TEXT NOT NULL DEFAULT '',       -- JSON array, assistant rows only
  tool_call_id TEXT NOT NULL DEFAULT '',       -- tool rows only
  tool_name    TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  UNIQUE (session_id, seq)
);
CREATE INDEX idx_messages_session_seq ON messages(session_id, seq);

CREATE TABLE approvals (
  id          TEXT PRIMARY KEY,                -- nonce, also the callback_data payload
  session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  channel     TEXT NOT NULL,
  chat_id     TEXT NOT NULL,
  tool        TEXT NOT NULL,
  command     TEXT NOT NULL,
  reason      TEXT NOT NULL DEFAULT '',        -- classifier rationale in auto mode
  state       TEXT NOT NULL,                   -- pending|approved|denied|expired
  message_id  TEXT NOT NULL DEFAULT '',        -- telegram msg holding the buttons
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  decided_at  INTEGER,
  decided_by  TEXT NOT NULL DEFAULT ''         -- telegram user id
);
CREATE INDEX idx_approvals_state ON approvals(state, expires_at);

CREATE TABLE exec_audit (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id  TEXT NOT NULL,                   -- no FK: audit outlives sessions
  command     TEXT NOT NULL,
  cwd         TEXT NOT NULL DEFAULT '',
  decision    TEXT NOT NULL,                   -- denied_rule|allowed_rule|approved|denied_user|expired|auto_allowed
  rule        TEXT NOT NULL DEFAULT '',        -- the matching regex, when any
  exit_code   INTEGER,
  duration_ms INTEGER,
  truncated   INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL
);
CREATE INDEX idx_exec_audit_created ON exec_audit(created_at);

CREATE TABLE cron_runs (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  job_name    TEXT NOT NULL,
  session_id  TEXT NOT NULL DEFAULT '',
  status      TEXT NOT NULL,                   -- ok|error|skipped
  error       TEXT NOT NULL DEFAULT '',
  started_at  INTEGER NOT NULL,
  finished_at INTEGER
);
CREATE INDEX idx_cron_runs_job ON cron_runs(job_name, started_at);
