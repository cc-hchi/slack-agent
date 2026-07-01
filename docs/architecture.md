# Slack Agent Architecture

## System Shape

The app is a local backend plus a local dashboard.

First implementation:

- Backend: Go `net/http`.
- Database: local SQLite with WAL enabled.
- Slack integration: Slack Web API using real workspace credentials.
- Worker runtime: Codex app-server over stdio JSON-RPC.
- Dashboard: React/Vite app served by the backend or a local dev server.
- Job workspaces: one folder per job under a configurable workspace root.

SQLite is enough for a personal local tool and keeps setup simple. The schema
should avoid opaque JSON-only records for core state, but can keep raw Slack and
Codex event payloads for auditability.

## Components

### Slack Collector

Responsibilities:

- Discover relevant Slack trigger messages from mentions and DMs.
- Fetch enough Slack metadata to identify the trigger message, thread context, latest
  activity, and display summary.
- De-duplicate by Slack team, channel, and trigger timestamp.
- Store normalized intake metadata locally, not full Slack message transcripts.
- Mark new or changed intake items for analysis.

First version should support both a manual "sync now" action and a background
interval.

Slack source rules:

- Direct messages to the user always qualify.
- Mentions of the user qualify.
- Threads where the user has posted qualify.

### Intake Analyzer

Responsibilities:

- Create a dedicated intake workspace and run workspace bootstrap.
- Start a dedicated Codex thread for one Slack trigger.
- Ask Codex whether the Slack context requires user action.
- Have Codex fetch the latest Slack messages live before deciding; the local DB
  only provides thread coordinates and summary fields.
- Require structured output:
  - action required or not.
  - confidence.
  - summary.
  - proposed job title.
  - task type.
  - urgency.
  - required context.
  - suggested Slack reply if no worker job is needed.
- Create a job when action is needed.
- Record "no action" decisions with rationale.

Analyzer sessions should be isolated from worker sessions, but they share the
same Slack-thread workspace so worker execution can reuse analyzer context and
artifacts. They answer "should we act?" and produce a job plan, but do not do
the work.

### Job Orchestrator

Responsibilities:

- Own job state transitions.
- Reuse the Slack-thread workspace created by the analyzer.
- Create and bootstrap a fallback workspace for legacy or manually-created jobs.
- Start worker sessions.
- Retry failed or blocked jobs only when explicitly requested.
- Enforce the Slack send gate.

Workspace initialization:

1. Use the job's persisted `workspace_path` when present.
2. Otherwise derive the workspace from the intake item id.
3. If no bootstrapped workspace exists, run the configurable bootstrap command.
   Default:
   `install-chi-skills`
4. Continue only if bootstrap succeeds, unless the user manually overrides.

### Codex Worker

Responsibilities:

- Start a dedicated Codex app-server thread in the job workspace.
- Run with full local execution permission.
- Attempt to complete or materially advance the task.
- Persist event stream, final result, changed files, commands, artifacts, and
  blockers.
- Produce Slack reply draft when useful.

Codex app-server protocol:

- Spawn `codex app-server` with stdio transport.
- Send `initialize`.
- Send `initialized`.
- Send `thread/start` with the job workspace cwd.
- Send `turn/start` with the worker prompt and Slack intake coordinates.
- Stream `item/*` and `turn/completed` notifications.

Do not depend on the local app-server daemon control socket in the first
version; direct child-process app-server sessions are simpler and already
verified.

### Reply Review

Responsibilities:

- Store reply drafts separately from worker results.
- Show source Slack context and worker evidence next to the draft.
- Allow edit, approve, reject, regenerate, and mark not needed.
- Post to Slack only after explicit approval.
- Store Slack post response and permalink after successful send.

The posting action is the only hard human approval gate in the confirmed
product scope.

### Dashboard API

Responsibilities:

- Expose state for the dashboard.
- Start sync, analysis, and worker runs.
- Approve or reject reply drafts.
- Stream worker logs/events.
- Show health checks for Slack, Codex, database, and bootstrap command.

## State Machine

Intake item states:

- `pending`
- `resolved`

`intake_items.resolution` records why a resolved item left intake, for example
`no_action`, `job_created`, `reply_sent`, `reply_ignored`, or
`completed_no_reply`. Analyzer queued/running state is runtime-only and is kept
in memory so pending items retry naturally after restart.

Job states:

- `queued`
- `draft_ready`
- `blocked`
- `completed_no_reply`
- `failed`
- `cancelled`

Active worker/working state is runtime-only and is derived from the backend
worker set, not persisted in `jobs.status`.

Reply states:

- `draft`
- `edited`
- `approved`
- `sent`
- `rejected`
- `not_needed`
- `send_failed`

## Data Model

Core tables:

- `intake_items`
- `slack_users`
- `analysis_runs`
- `jobs`
- `job_events`
- `worker_sessions`
- `worker_artifacts`
- `reply_drafts`
- `settings`
- `health_checks`

### intake_items

- id
- slack_team_id
- channel_id
- channel_name
- trigger_ts
- thread_ts
- source_type: `user_participated`, `mention`, `dm`
- status
- resolution
- trigger_user_id
- trigger_text
- latest_slack_message_ts
- latest_user_id
- latest_user_name
- latest_text
- last_analyzed_slack_ts
- last_synced_at
- permalink
- raw_json

### slack_users

- user_id
- display_name
- real_name
- team_id
- is_bot
- deleted
- raw_json
- updated_at

### analysis_runs

- id
- intake_item_id
- codex_thread_id
- codex_session_id
- status
- action_required
- confidence
- summary
- rationale
- structured_result_json
- started_at
- completed_at
- error

### jobs

- id
- intake_item_id
- analysis_run_id
- title
- status
- urgency
- task_type
- workspace_path
- bootstrap_status
- codex_thread_id
- current_block_reason
- next_user_action
- created_at
- updated_at

### job_events

- id
- job_id
- event_type
- message
- payload_json
- created_at

### worker_artifacts

- id
- job_id
- artifact_type
- path
- summary
- payload_json
- created_at

### reply_drafts

- id
- job_id
- intake_item_id
- status
- draft_text
- edited_text
- rationale
- slack_channel_id
- slack_thread_ts
- slack_message_ts
- sent_permalink
- created_at
- updated_at

## Worker Prompt Contract

Each worker prompt should include:

- Slack intake trigger and context coordinates.
- Analyzer result.
- Hard policy: do not send Slack messages.
- Allowed actions: local files, commands, network, repos, PRs, docs, tests.
- Required final structured result:
  - outcome: `completed`, `advanced`, `blocked`, or `failed`.
  - summary.
  - actions_taken.
  - evidence.
  - changed_files.
  - blocker_reason.
  - next_user_action.
  - slack_reply_draft.

The backend should parse this result and update job/reply state.

## Health Checks

Required startup checks:

- SQLite writable.
- Slack token present and can call `auth.test`.
- Slack user id resolved.
- Codex binary available.
- Codex app-server initialize succeeds.
- Bootstrap command exists, or is explicitly marked disabled.
- Workspace root exists and is writable.

## Open Implementation Decisions

- Exact Slack OAuth scopes and token source.
- Exact model and reasoning settings for analyzer versus worker.
- Whether to allow multiple concurrent worker jobs by default.
- Whether worker-created Codex threads should be persisted and resumable from
  the Codex desktop app, or treated as app-internal records only.
