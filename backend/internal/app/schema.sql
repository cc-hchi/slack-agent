CREATE TABLE IF NOT EXISTS metadata (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS intake_items (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  slack_team_id TEXT NOT NULL,
  channel_id TEXT NOT NULL,
  channel_name TEXT,
  trigger_ts TEXT NOT NULL,
  thread_ts TEXT NOT NULL,
  source_type TEXT NOT NULL,
  status TEXT NOT NULL,
  resolution TEXT,
  title TEXT,
  permalink TEXT,
  trigger_user_id TEXT,
  trigger_text TEXT,
  latest_slack_message_ts TEXT,
  latest_user_id TEXT,
  latest_user_name TEXT,
  latest_text TEXT,
  last_analyzed_slack_ts TEXT,
  last_synced_at TEXT,
  raw_json TEXT,
  UNIQUE(slack_team_id, channel_id, trigger_ts)
);

CREATE INDEX IF NOT EXISTS idx_intake_items_status_latest_ts
  ON intake_items(status, latest_slack_message_ts);

CREATE INDEX IF NOT EXISTS idx_intake_items_latest_ts
  ON intake_items(latest_slack_message_ts);

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
  intake_item_id INTEGER NOT NULL REFERENCES intake_items(id) ON DELETE CASCADE,
  codex_thread_id TEXT,
  codex_session_id TEXT,
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

CREATE INDEX IF NOT EXISTS idx_analysis_runs_intake_latest
  ON analysis_runs(intake_item_id, id DESC);

CREATE INDEX IF NOT EXISTS idx_analysis_runs_codex_session_id
  ON analysis_runs(codex_session_id);

CREATE TABLE IF NOT EXISTS jobs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  intake_item_id INTEGER NOT NULL REFERENCES intake_items(id) ON DELETE CASCADE,
  analysis_run_id INTEGER REFERENCES analysis_runs(id) ON DELETE SET NULL,
  title TEXT NOT NULL,
  status TEXT NOT NULL,
  urgency TEXT,
  task_type TEXT,
  workspace_path TEXT,
  bootstrap_status TEXT,
  codex_thread_id TEXT,
  codex_session_id TEXT,
  current_block_reason TEXT,
  next_user_action TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_jobs_intake_item_id
  ON jobs(intake_item_id);

CREATE INDEX IF NOT EXISTS idx_jobs_codex_session_id
  ON jobs(codex_session_id);

CREATE TABLE IF NOT EXISTS job_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  event_type TEXT NOT NULL,
  message TEXT,
  payload_json TEXT,
  created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_job_events_created_at
  ON job_events(created_at DESC);

CREATE TABLE IF NOT EXISTS worker_sessions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  codex_thread_id TEXT,
  codex_session_id TEXT,
  status TEXT NOT NULL,
  started_at TEXT,
  completed_at TEXT,
  error TEXT
);

CREATE INDEX IF NOT EXISTS idx_worker_sessions_codex_session_id
  ON worker_sessions(codex_session_id);

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
  intake_item_id INTEGER NOT NULL REFERENCES intake_items(id) ON DELETE CASCADE,
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
