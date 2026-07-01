# Slack Agent Product Brief

## Goal

Build a personal local Slack work agent that watches Slack triggers involving the
user, extracts actionable work, tries to move that work forward with Codex, and
prepares Slack reply drafts for human review.

This is a personal workstation tool, not a team SaaS product. Optimize for fast
local operation, traceability, and low-friction review.

## Confirmed Scope

### Slack sources

The collector ingests only these Slack surfaces at first:

- Threads where the user has posted.
- Threads where the user was mentioned.
- Direct messages sent to the user.

### Execution policy

Workers may take local actions without restriction, including reading and
writing local files, running commands, using the network, creating independent
workspaces, and advancing tasks with Codex.

The only hard human gate is sending Slack messages. Slack replies are generated
as drafts and must wait for user review before being posted.

### Storage

All Slack content snapshots, analysis results, jobs, worker events, artifacts,
blockers, and reply drafts are stored locally.

### Backend

The backend must be a real working service, not a mock. It should:

- Poll or sync Slack into local records.
- Analyze each candidate thread.
- Create jobs for threads that need action.
- Start Codex analyzer and worker sessions.
- Track worker progress and terminal state.
- Surface reply drafts for review.
- Post approved Slack replies.

### Dashboard

The dashboard must be a real operational console over the local backend. It
should show intake status, job state, worker attempts, blockers, artifacts, and
Slack reply drafts.

## Non-Goals For The First Version

- Multi-user permissioning.
- Cloud deployment.
- Automatically sending Slack messages without review.
- Processing every Slack channel or every unread message.
- Team-wide assignment workflows.
- A mobile-first experience.

## Product Principles

- Keep the first screen useful as a work queue, not a landing page.
- Make every automated decision inspectable.
- Prefer explicit state over hidden background work.
- Show what was tried before asking the user for help.
- Make blocked items actionable: every block needs a reason and a proposed next
  confirmation.
- Treat Slack reply review as the primary safety rail.

## Primary Workflow

1. Collector discovers a Slack trigger from a mention or DM.
2. Analyzer creates a dedicated intake workspace, runs
   `install-chi-skills`, and opens a dedicated Codex session for that Slack
   context.
3. Analyzer decides whether the intake needs user action.
4. If no action is needed, the system records the reason and archives the item.
5. If action is needed, the system creates a job linked to the analyzer
   workspace.
6. Worker reuses the intake workspace and opens a dedicated Codex session
   to advance the job.
7. Worker records progress, artifacts, errors, and final outcome.
8. If a Slack reply is useful, worker creates a draft.
9. User reviews, edits, approves, or rejects the draft from the dashboard.
10. Backend sends the approved reply to Slack.

## Current Local Probe Results

- `codex app-server` stdio JSON-RPC initialization works locally.
- The local daemon control socket was not available during probing, so the first
  implementation should spawn app-server subprocesses directly instead of
  depending on daemon proxy.
- `install-chi-skills` was found at `/Users/chihuang/.local/bin/install-chi-skills`;
  the implementation should keep the bootstrap command configurable in case the
  path changes.
  implementation should make the bootstrap command configurable and expose a
  startup health check for it.
