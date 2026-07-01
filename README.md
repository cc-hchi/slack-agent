# Local Slack Agent

Personal local Slack work agent with a real Go/SQLite backend and a Vite
dashboard.

## Run Locally

Start both backend and frontend from the repo root:

```bash
make dev
```

Open `http://127.0.0.1:5173`.

The frontend dependencies are installed automatically the first time if
`frontend/node_modules` is missing. Press `Ctrl-C` to stop both services.

To restart an existing local dev session:

```bash
make restart
```

This stops current backend/frontend listeners on the configured dev ports, then
starts the same `make dev` flow. It refuses to stop a process outside this
project unless `SLACK_AGENT_RESTART_FORCE=true` is set.

You can also run each side separately:

```bash
make backend
```

In another shell:

```bash
make frontend-install
make frontend
```

## Configuration

Copy `.env.example` to `.env` and set real Slack credentials.

Slack sync and intake auto-advance run in the background by default. On startup
and after each Slack sync, pending intake items are analyzed, action-required
items create jobs, and queued jobs are run until they reach a draft,
blocked/failed review state, or a terminal no-reply state.

Useful automation knobs:

- `SLACK_AGENT_ANALYZER_CONCURRENCY=1` controls concurrent analyzer sessions.
- `SLACK_AGENT_WORKER_CONCURRENCY=2` controls concurrent worker sessions.
- `SLACK_AGENT_SYNC_INTERVAL_SECONDS=300` controls how often the backend starts
  the next background Slack sync after the previous sync finishes.
- `SLACK_AGENT_SEARCH_PAGE_SIZE=100` and `SLACK_AGENT_SEARCH_MAX_PAGES=20`
  control mention/user-participation search depth.
- `SLACK_AGENT_INCLUDE_USER_PARTICIPATED=true` opts into `from:@user`
  collection. By default intake only uses mentions and DMs.
- `SLACK_AGENT_DM_CHANNEL_LIMIT=0` scans all IM channels; set a positive value
  to cap it.
- `SLACK_AGENT_DM_HISTORY_PAGE_SIZE=100` and
  `SLACK_AGENT_DM_HISTORY_MAX_PAGES=10` control per-DM history depth.
- `SLACK_AGENT_REPLY_PAGE_SIZE=200` and `SLACK_AGENT_REPLY_MAX_PAGES=10`
  control thread reply depth.
- `SLACK_AGENT_SEARCH_LOOKBACK_DAYS=3` keeps an overlap window around sync
  cursors so slower background syncs do not miss late-arriving updates.
- Codex analyzer and worker sessions share a per-intake workspace. The
  analyzer runs the configured workspace bootstrap before starting; later worker
  sessions reuse that workspace and default to local-only execution:
  `SLACK_AGENT_CODEX_PERMISSION_PROFILE=:workspace` with
  `SLACK_AGENT_CODEX_APPROVAL_POLICY=never`. This allows local workspace
  file/command work and prevents unattended network or out-of-workspace approval
  prompts from hanging automation.

Slack sending is only performed through the reply review endpoint:
`POST /api/replies/{reply_id}/send`.

## Current Notes

- The backend uses local SQLite under `data/` by default.
- SQLite stores Slack trigger coordinates and summary fields, not full Slack
  message transcripts. Analyzer and worker sessions refresh Slack context live.
- Codex workers use `codex app-server` over stdio JSON-RPC.
- Each intake item gets a local workspace under `data/workspaces`.
- The default bootstrap command is `install-chi-skills`; health checks show
  a missing-config state if it is not on `PATH`.
