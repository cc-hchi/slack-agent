# Dashboard Design Brief

## Purpose

The dashboard is a local operations console for the personal Slack agent. It is
not a marketing site. The first screen should immediately show what needs the
user's attention.

## Target User

One power user managing personal Slack obligations while allowing Codex workers
to advance local work in the background.

## Primary Screens

### Work Queue

Shows Slack-derived work grouped by state:

- Needs review.
- Working.
- Blocked.
- New analysis.
- No action.
- Failed.

The default sort should put reply drafts and blockers first.

### Intake Detail

Shows:

- Slack intake summary.
- Trigger summary and Slack permalink.
- Live Slack context refresh status when analyzer or worker has fetched it.
- Why the item was captured.
- Analyzer decision and confidence.
- Linked job if one exists.

### Job Detail

Shows:

- Current job state.
- Analyzer plan.
- Worker timeline.
- Workspace path.
- Bootstrap output.
- Codex thread id.
- Artifacts and changed files.
- Block reason and next requested user confirmation.

### Reply Review

Shows:

- Slack destination.
- Draft reply.
- Editable reply text.
- Evidence from the worker.
- Approve/send, reject, regenerate, and mark not needed actions.

Approve/send must be visually distinct because it is the only irreversible
external action.

### Health And Settings

Shows:

- Slack auth status.
- Codex app-server status.
- Bootstrap command status.
- Workspace root.
- Sync interval.
- Worker concurrency.
- Last collector run.

## Interaction Level

Full interactivity:

- Filters, tabs, and search work.
- Queue state updates from the backend.
- Details load real local records.
- Reply draft editing persists.
- Approve sends to Slack through the backend.
- Retry/re-run actions call real backend endpoints.

## Visual Direction Requirements

No existing design system is available. The UI should feel like a serious local
operator console:

- Dense but readable.
- Clear state badges.
- Calm color system with high contrast.
- Minimal decorative surfaces.
- No landing-page hero.
- No nested cards.
- Use icon buttons where appropriate.
- Make blocked and review states easy to scan.

## First Visual Exploration Target

Generate three desktop dashboard concepts at 1440 x 1024:

1. Queue-first command center.
2. Split-pane thread and job triage.
3. Timeline-first operations console.
