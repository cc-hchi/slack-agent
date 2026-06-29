CREATE TABLE IF NOT EXISTS metadata (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS slack_threads (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  slack_team_id TEXT NOT NULL,
  channel_id TEXT NOT NULL,
  channel_name TEXT,
  thread_ts TEXT NOT NULL,
  root_message_ts TEXT,
  source_type TEXT NOT NULL,
  status TEXT NOT NULL,
  title TEXT,
  permalink TEXT,
  last_slack_activity_at TEXT,
  latest_slack_message_ts TEXT,
  last_analyzed_slack_ts TEXT,
  last_synced_at TEXT,
  raw_json TEXT,
  UNIQUE(slack_team_id, channel_id, thread_ts)
);

CREATE INDEX IF NOT EXISTS idx_slack_threads_status_activity
  ON slack_threads(status, last_slack_activity_at);

CREATE TABLE IF NOT EXISTS slack_messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  slack_thread_id INTEGER NOT NULL REFERENCES slack_threads(id) ON DELETE CASCADE,
  slack_message_ts TEXT NOT NULL,
  user_id TEXT,
  user_name TEXT,
  text TEXT,
  is_user_message INTEGER NOT NULL DEFAULT 0,
  mentions_user INTEGER NOT NULL DEFAULT 0,
  raw_json TEXT,
  UNIQUE(slack_thread_id, slack_message_ts)
);

CREATE INDEX IF NOT EXISTS idx_slack_messages_thread_ts
  ON slack_messages(slack_thread_id, slack_message_ts);

CREATE TABLE IF NOT EXISTS slack_users (
  user_id TEXT PRIMARY KEY,
  display_name TEXT,
  real_name TEXT,
  team_id TEXT,
  is_bot INTEGER NOT NULL DEFAULT 0,
  deleted INTEGER NOT NULL DEFAULT 0,
  raw_json TEXT,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS analysis_runs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  slack_thread_id INTEGER NOT NULL REFERENCES slack_threads(id) ON DELETE CASCADE,
  codex_thread_id TEXT,
  status TEXT NOT NULL,
  action_required INTEGER,
  confidence REAL,
  summary TEXT,
  rationale TEXT,
  structured_result_json TEXT,
  started_at TEXT,
  completed_at TEXT,
  error TEXT
);

CREATE TABLE IF NOT EXISTS jobs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  slack_thread_id INTEGER NOT NULL REFERENCES slack_threads(id) ON DELETE CASCADE,
  analysis_run_id INTEGER REFERENCES analysis_runs(id) ON DELETE SET NULL,
  title TEXT NOT NULL,
  status TEXT NOT NULL,
  urgency TEXT,
  task_type TEXT,
  workspace_path TEXT,
  bootstrap_status TEXT,
  codex_thread_id TEXT,
  current_block_reason TEXT,
  next_user_action TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_jobs_slack_thread_id
  ON jobs(slack_thread_id);

CREATE TABLE IF NOT EXISTS job_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  event_type TEXT NOT NULL,
  message TEXT,
  payload_json TEXT,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS worker_sessions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  codex_thread_id TEXT,
  status TEXT NOT NULL,
  started_at TEXT,
  completed_at TEXT,
  error TEXT
);

CREATE TABLE IF NOT EXISTS worker_artifacts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  artifact_type TEXT NOT NULL,
  path TEXT,
  summary TEXT,
  payload_json TEXT,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS reply_drafts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id INTEGER REFERENCES jobs(id) ON DELETE SET NULL,
  slack_thread_id INTEGER NOT NULL REFERENCES slack_threads(id) ON DELETE CASCADE,
  status TEXT NOT NULL,
  draft_text TEXT NOT NULL,
  edited_text TEXT,
  rationale TEXT,
  slack_channel_id TEXT NOT NULL,
  slack_thread_ts TEXT NOT NULL,
  slack_message_ts TEXT,
  sent_permalink TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS health_checks (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  service TEXT NOT NULL,
  status TEXT NOT NULL,
  detail TEXT,
  checked_at TEXT NOT NULL
);
