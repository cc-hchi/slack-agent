package app

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func testService(t *testing.T) (*Service, *Store) {
	t.Helper()
	cfg := Config{
		DatabasePath:            filepath.Join(t.TempDir(), "slack_agent.db"),
		WorkspaceRoot:           t.TempDir(),
		SlackReplyPageSize:      200,
		SlackReplyMaxPages:      10,
		SlackSearchLookbackDays: 14,
	}
	store, err := OpenStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	return &Service{cfg: cfg, store: store}, store
}

func TestUpsertThreadUsesRootLatestActivityAndRecollectsTerminalThread(t *testing.T) {
	service, store := testService(t)
	root := map[string]any{"ts": "1764168000.000100", "text": "Root task"}
	reply := map[string]any{"ts": "1764168060.000200", "thread_ts": "1764168000.000100", "text": "Reply"}
	if err := service.upsertThread("T1", "C1", "backend", "1764168000.000100", "mention", root, []map[string]any{root, reply}, "U-me"); err != nil {
		t.Fatal(err)
	}

	var title, rootTS, latestTS, activityAt string
	if err := store.db.QueryRow(`
		SELECT title, root_message_ts, latest_slack_message_ts, last_slack_activity_at
		FROM slack_threads
		WHERE slack_team_id='T1' AND channel_id='C1' AND thread_ts='1764168000.000100'`,
	).Scan(&title, &rootTS, &latestTS, &activityAt); err != nil {
		t.Fatal(err)
	}
	if title != "Root task" || rootTS != "1764168000.000100" || latestTS != "1764168060.000200" {
		t.Fatalf("unexpected thread metadata: title=%q root=%q latest=%q", title, rootTS, latestTS)
	}
	if activityAt != "2025-11-26T14:41:00Z" {
		t.Fatalf("expected Slack activity time, got %q", activityAt)
	}

	if _, err := store.db.Exec("UPDATE slack_threads SET status='no_action' WHERE slack_team_id='T1' AND channel_id='C1' AND thread_ts='1764168000.000100'"); err != nil {
		t.Fatal(err)
	}
	if err := service.upsertThread("T1", "C1", "backend", "1764168000.000100", "mention", root, []map[string]any{root, reply}, "U-me"); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := store.db.QueryRow("SELECT status FROM slack_threads WHERE slack_team_id='T1' AND channel_id='C1' AND thread_ts='1764168000.000100'").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "no_action" {
		t.Fatalf("same Slack activity should not recollect terminal thread, got %q", status)
	}

	newReply := map[string]any{"ts": "1764168120.000300", "thread_ts": "1764168000.000100", "text": "New reply"}
	if err := service.upsertThread("T1", "C1", "backend", "1764168000.000100", "mention", root, []map[string]any{root, reply, newReply}, "U-me"); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT status, latest_slack_message_ts FROM slack_threads WHERE slack_team_id='T1' AND channel_id='C1' AND thread_ts='1764168000.000100'").Scan(&status, &latestTS); err != nil {
		t.Fatal(err)
	}
	if status != "collected" || latestTS != "1764168120.000300" {
		t.Fatalf("new Slack activity should recollect thread, status=%q latest=%q", status, latestTS)
	}
}

func TestDashboardIntakeIncludesRecollectedThreadWithExistingJob(t *testing.T) {
	service, store := testService(t)
	root := map[string]any{"ts": "1764168000.000100", "text": "Root task"}
	reply := map[string]any{"ts": "1764168060.000200", "thread_ts": "1764168000.000100", "text": "Reply"}
	if err := service.upsertThread("T1", "C1", "backend", "1764168000.000100", "mention", root, []map[string]any{root, reply}, "U-me"); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM slack_threads").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("UPDATE slack_threads SET status='job_created' WHERE id=?", threadID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		"INSERT INTO jobs(slack_thread_id, title, status, created_at, updated_at) VALUES (?, 'Old job', 'completed_no_reply', ?, ?)",
		threadID,
		utcNow(),
		utcNow(),
	); err != nil {
		t.Fatal(err)
	}

	newReply := map[string]any{"ts": "1764168120.000300", "thread_ts": "1764168000.000100", "text": "New reply"}
	if err := service.upsertThread("T1", "C1", "backend", "1764168000.000100", "mention", root, []map[string]any{root, reply, newReply}, "U-me"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.DashboardSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Intake) != 1 {
		t.Fatalf("expected recollected thread with existing job in intake, got %d", len(snapshot.Intake))
	}
}

func TestDashboardSnapshotIncludesRootUserForRepliesAndJobs(t *testing.T) {
	service, store := testService(t)
	root := map[string]any{"ts": "1764168000.000100", "text": "Need help", "user": "U1"}
	if err := service.upsertThread("T1", "C1", "DM", "1764168000.000100", "dm", root, []map[string]any{root}, "U-me"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`
		INSERT INTO slack_users(user_id, display_name, real_name, team_id, is_bot, deleted, raw_json, updated_at)
		VALUES ('U1', 'Tim Zhou', 'Timothy Zhou', 'T1', 0, 0, '{}', ?)`,
		utcNow(),
	); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM slack_threads").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		"INSERT INTO jobs(slack_thread_id, title, status, created_at, updated_at) VALUES (?, 'Need help', 'draft_ready', ?, ?)",
		threadID,
		utcNow(),
		utcNow(),
	); err != nil {
		t.Fatal(err)
	}
	var jobID int64
	if err := store.db.QueryRow("SELECT id FROM jobs").Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`
		INSERT INTO reply_drafts(
			job_id, slack_thread_id, status, draft_text, slack_channel_id, slack_thread_ts,
			created_at, updated_at
		) VALUES (?, ?, 'draft', 'Sure', 'C1', '1764168000.000100', ?, ?)`,
		jobID,
		threadID,
		utcNow(),
		utcNow(),
	); err != nil {
		t.Fatal(err)
	}

	snapshot, err := store.DashboardSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ReplyDrafts) != 1 {
		t.Fatalf("expected one reply draft, got %d", len(snapshot.ReplyDrafts))
	}
	if got := snapshot.ReplyDrafts[0]["root_user_name"]; got != "Tim Zhou" {
		t.Fatalf("reply draft root user = %v, want Tim Zhou", got)
	}
	if len(snapshot.Jobs) != 1 {
		t.Fatalf("expected one job, got %d", len(snapshot.Jobs))
	}
	if got := snapshot.Jobs[0]["root_user_name"]; got != "Tim Zhou" {
		t.Fatalf("job root user = %v, want Tim Zhou", got)
	}
}

func TestRetryJobAfterWorkerErrorQueuesJobAndKeepsExecutionLog(t *testing.T) {
	service, store := testService(t)
	if _, err := store.db.Exec(`
		INSERT INTO slack_threads(slack_team_id, channel_id, thread_ts, source_type, status, title)
		VALUES ('T1', 'C1', '1764168000.000100', 'dm', 'job_created', 'Task')`); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM slack_threads").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`
		INSERT INTO jobs(
			slack_thread_id, title, status, current_block_reason, next_user_action,
			created_at, updated_at
		) VALUES (?, 'Task', 'queued', 'turn timed out', 'Review Codex worker failure and retry.', ?, ?)`,
		threadID,
		utcNow(),
		utcNow(),
	); err != nil {
		t.Fatal(err)
	}
	var jobID int64
	if err := store.db.QueryRow("SELECT id FROM jobs").Scan(&jobID); err != nil {
		t.Fatal(err)
	}

	service.retryJobAfterWorkerError(jobID, "Codex worker turn failed: turn timed out", map[string]any{"error": "turn timed out"})

	var status string
	var reason, nextAction sql.NullString
	if err := store.db.QueryRow("SELECT status, current_block_reason, next_user_action FROM jobs WHERE id=?", jobID).Scan(&status, &reason, &nextAction); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || reason.Valid || nextAction.Valid {
		t.Fatalf("expected queued job without block markers, status=%q reason=%q next=%q", status, reason.String, nextAction.String)
	}

	var eventCount int
	if err := store.db.QueryRow(`
		SELECT COUNT(*)
		FROM job_events
		WHERE job_id=? AND event_type='worker_retry_scheduled'
		  AND message='Codex worker turn failed: turn timed out'
		  AND json_extract(payload_json, '$.retry_status')='queued'`,
		jobID,
	).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("expected one worker retry event, got %d", eventCount)
	}
}

func TestQueueJobKeepsStatusQueuedUntilWorkerClaim(t *testing.T) {
	service, store := testService(t)
	service.jobs = make(chan int64, 1)
	service.queuedJobs = map[int64]struct{}{}
	if _, err := store.db.Exec(`
		INSERT INTO slack_threads(slack_team_id, channel_id, thread_ts, source_type, status, title)
		VALUES ('T1', 'C1', '1764168000.000100', 'dm', 'job_created', 'Task')`); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM slack_threads").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		"INSERT INTO jobs(slack_thread_id, title, status, created_at, updated_at) VALUES (?, 'Task', 'queued', ?, ?)",
		threadID,
		utcNow(),
		utcNow(),
	); err != nil {
		t.Fatal(err)
	}
	var jobID int64
	if err := store.db.QueryRow("SELECT id FROM jobs").Scan(&jobID); err != nil {
		t.Fatal(err)
	}

	result, err := service.QueueJob(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "queued" || result["queued"] != true {
		t.Fatalf("expected queued result, got %#v", result)
	}
	var status string
	if err := store.db.QueryRow("SELECT status FROM jobs WHERE id=?", jobID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "queued" {
		t.Fatalf("QueueJob should not claim the job, got %q", status)
	}
	select {
	case got := <-service.jobs:
		if got != jobID {
			t.Fatalf("queued job id = %d, want %d", got, jobID)
		}
	default:
		t.Fatal("expected job to be enqueued in memory")
	}

	if !service.claimJob(jobID) {
		t.Fatal("expected claimJob to claim queued job")
	}
	if err := store.db.QueryRow("SELECT status FROM jobs WHERE id=?", jobID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "queued" {
		t.Fatalf("claimJob should keep persisted job queued, got %q", status)
	}
	if !service.isJobWorking(jobID) {
		t.Fatal("claimJob should mark job working in memory")
	}
}

func TestRecoverInterruptedJobsRequeuesObsoleteInFlightStates(t *testing.T) {
	service, store := testService(t)
	if _, err := store.db.Exec(`
		INSERT INTO slack_threads(slack_team_id, channel_id, thread_ts, source_type, status, title)
		VALUES ('T1', 'C1', '1764168000.000100', 'dm', 'job_created', 'Task')`); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM slack_threads").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"workspace_creating", "bootstrapping", "working", "draft_ready"} {
		if _, err := store.db.Exec(
			"INSERT INTO jobs(slack_thread_id, title, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
			threadID,
			status,
			status,
			utcNow(),
			utcNow(),
		); err != nil {
			t.Fatal(err)
		}
	}

	service.recoverInterruptedJobs()

	rows, err := store.db.Query("SELECT title, status FROM jobs ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var title, status string
		if err := rows.Scan(&title, &status); err != nil {
			t.Fatal(err)
		}
		got[title] = status
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"workspace_creating", "bootstrapping", "working"} {
		if got[title] != "queued" {
			t.Fatalf("%s should be recovered to queued, got %q", title, got[title])
		}
	}
	if got["draft_ready"] != "draft_ready" {
		t.Fatalf("terminal job should not be recovered, got %q", got["draft_ready"])
	}
	var eventCount int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM job_events WHERE event_type='worker_recovered'").Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 3 {
		t.Fatalf("expected three recovery events, got %d", eventCount)
	}
}

func TestDashboardSnapshotUsesInMemoryWorkingJobs(t *testing.T) {
	service, store := testService(t)
	if _, err := store.db.Exec(`
		INSERT INTO slack_threads(slack_team_id, channel_id, thread_ts, source_type, status, title)
		VALUES ('T1', 'C1', '1764168000.000100', 'dm', 'job_created', 'Task')`); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM slack_threads").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"Active task", "Queued task"} {
		if _, err := store.db.Exec(
			"INSERT INTO jobs(slack_thread_id, title, status, created_at, updated_at) VALUES (?, ?, 'queued', ?, ?)",
			threadID,
			title,
			utcNow(),
			utcNow(),
		); err != nil {
			t.Fatal(err)
		}
	}
	var activeJobID int64
	if err := store.db.QueryRow("SELECT id FROM jobs WHERE title='Active task'").Scan(&activeJobID); err != nil {
		t.Fatal(err)
	}
	service.workingJobs = map[int64]struct{}{activeJobID: {}}

	snapshot, err := service.DashboardSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Active) != 1 || snapshot.Active[0]["title"] != "Active task" || snapshot.Active[0]["status"] != "working" {
		t.Fatalf("unexpected active jobs: %#v", snapshot.Active)
	}
	if len(snapshot.Queued) != 1 || snapshot.Queued[0]["title"] != "Queued task" {
		t.Fatalf("unexpected queued jobs: %#v", snapshot.Queued)
	}
	if snapshot.Counts["active"] != 1 || snapshot.Counts["queued"] != 1 {
		t.Fatalf("unexpected counts: %#v", snapshot.Counts)
	}
}

func TestEnsureJobWorkspaceSkipsBootstrapWhenAlreadySucceeded(t *testing.T) {
	service, store := testService(t)
	service.cfg.WorkspaceBootstrapCommand = "touch should_not_exist"
	if _, err := store.db.Exec(`
		INSERT INTO slack_threads(slack_team_id, channel_id, thread_ts, source_type, status, title)
		VALUES ('T1', 'C1', '1764168000.000100', 'dm', 'job_created', 'Task')`); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM slack_threads").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(service.cfg.WorkspaceRoot, "job-1")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		`INSERT INTO jobs(
			slack_thread_id, title, status, workspace_path, bootstrap_status, created_at, updated_at
		) VALUES (?, 'Task', 'queued', ?, 'succeeded', ?, ?)`,
		threadID,
		workspace,
		utcNow(),
		utcNow(),
	); err != nil {
		t.Fatal(err)
	}
	var jobID int64
	if err := store.db.QueryRow("SELECT id FROM jobs").Scan(&jobID); err != nil {
		t.Fatal(err)
	}

	ok := service.ensureJobWorkspace(jobID, map[string]any{
		"workspace_path":   workspace,
		"bootstrap_status": "succeeded",
	}, workspace)
	if !ok {
		t.Fatal("expected workspace setup to succeed")
	}
	if _, err := os.Stat(filepath.Join(workspace, "should_not_exist")); !os.IsNotExist(err) {
		t.Fatalf("bootstrap command should have been skipped, stat err=%v", err)
	}
	var eventCount int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM job_events WHERE job_id=? AND event_type='bootstrap_skipped'", jobID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("expected one bootstrap_skipped event, got %d", eventCount)
	}
}

func TestFinishAnalysisErrorKeepsThreadQueuedForRetry(t *testing.T) {
	service, store := testService(t)
	if _, err := store.db.Exec(`
		INSERT INTO slack_threads(slack_team_id, channel_id, thread_ts, source_type, status, title)
		VALUES ('T1', 'C1', '1764168000.000100', 'dm', 'analyzing', 'Task')`); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM slack_threads").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	result, err := store.db.Exec("INSERT INTO analysis_runs(slack_thread_id, status, started_at) VALUES (?, 'running', ?)", threadID, utcNow())
	if err != nil {
		t.Fatal(err)
	}
	analysisID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	service.finishAnalysisError(threadID, analysisID, errors.New("thread/start failed"))

	var threadStatus, runStatus, runError string
	if err := store.db.QueryRow("SELECT status FROM slack_threads WHERE id=?", threadID).Scan(&threadStatus); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT status, error FROM analysis_runs WHERE id=?", analysisID).Scan(&runStatus, &runError); err != nil {
		t.Fatal(err)
	}
	if threadStatus != "analysis_queued" || runStatus != "failed" || runError != "thread/start failed" {
		t.Fatalf("unexpected analysis retry state: thread=%q run=%q error=%q", threadStatus, runStatus, runError)
	}
}

func TestMigrateAddsSlackThreadTimestampColumnsToExistingDatabase(t *testing.T) {
	cfg := Config{DatabasePath: filepath.Join(t.TempDir(), "slack_agent.db")}
	store, err := OpenStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.db.Exec(`
		CREATE TABLE slack_threads (
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
		  last_synced_at TEXT,
		  raw_json TEXT,
		  UNIQUE(slack_team_id, channel_id, thread_ts)
		)`,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	columns := map[string]bool{}
	rows, err := store.db.Query("PRAGMA table_info(slack_threads)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"latest_slack_message_ts", "last_analyzed_slack_ts"} {
		if !columns[column] {
			t.Fatalf("expected migrated column %q", column)
		}
	}
}

func TestBackfillSlackThreadActivityUsesLatestSlackMessageTS(t *testing.T) {
	_, store := testService(t)
	if _, err := store.db.Exec(`
		INSERT INTO slack_threads(
			slack_team_id, channel_id, thread_ts, source_type, status,
			title, last_slack_activity_at, last_synced_at
		) VALUES ('T1', 'C1', '1764168000.000100', 'mention', 'collected', 'Root', '2026-06-26T10:22:33Z', '2026-06-26T10:22:33Z')`); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM slack_threads").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`
		INSERT INTO slack_messages(slack_thread_id, slack_message_ts, user_id, text)
		VALUES (?, '1764168000.000100', 'U1', 'Root'),
		       (?, '1764168120.000300', 'U2', 'Latest reply')`,
		threadID,
		threadID,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.backfillSlackThreadActivity(); err != nil {
		t.Fatal(err)
	}
	var latestTS, activityAt string
	if err := store.db.QueryRow("SELECT latest_slack_message_ts, last_slack_activity_at FROM slack_threads WHERE id=?", threadID).Scan(&latestTS, &activityAt); err != nil {
		t.Fatal(err)
	}
	if latestTS != "1764168120.000300" || activityAt != "2025-11-26T14:42:00Z" {
		t.Fatalf("expected latest Slack activity, latest=%q activity=%q", latestTS, activityAt)
	}
}

func TestExtractReplyRequiresLineStartMarkerAndRejectsProcessNotes(t *testing.T) {
	if got := extractReply("The service will create a reply from SLACK_REPLY_DRAFT: not a real reply"); got != "" {
		t.Fatalf("inline marker mention should not be extracted: %q", got)
	}
	if got := extractReply("SLACK_REPLY_DRAFT: ` marker, so I won't write directly to the database."); got != "" {
		t.Fatalf("process note should not be extracted: %q", got)
	}
	got := extractReply(`OUTCOME: completed
ACTIONS_TAKEN:
- Checked the release.
SLACK_REPLY_DRAFT:
我看了下，这次发布没有带上我的支付相关改动，可以继续推进。

NEXT_USER_ACTION:
None`)
	want := "我看了下，这次发布没有带上我的支付相关改动，可以继续推进。"
	if got != want {
		t.Fatalf("unexpected reply draft\nwant: %q\n got: %q", want, got)
	}
}

func TestParseWorkerOutputHandlesBlockedOutcome(t *testing.T) {
	result := parseWorkerOutput(`Outcome: blocked
ACTIONS_TAKEN:
- Checked the local repo.
BLOCKER_REASON:
Need a production request ID.
NEXT_USER_ACTION:
Send the failed request ID.
SLACK_REPLY_DRAFT: NONE`)

	if result.Outcome != "blocked" {
		t.Fatalf("expected blocked outcome, got %q", result.Outcome)
	}
	if result.BlockerReason != "Need a production request ID." {
		t.Fatalf("unexpected blocker: %q", result.BlockerReason)
	}
	if result.NextUserAction != "Send the failed request ID." {
		t.Fatalf("unexpected next action: %q", result.NextUserAction)
	}
	if result.ReplyDraft != "" {
		t.Fatalf("blocked result with NONE draft should not create reply: %q", result.ReplyDraft)
	}
}

func TestFinishJobFromWorkerOutputBlocksJobAndKeepsThreadOpen(t *testing.T) {
	service, store := testService(t)
	if _, err := store.db.Exec(`
		INSERT INTO slack_threads(slack_team_id, channel_id, thread_ts, source_type, status, title)
		VALUES ('T1', 'C1', '1764168000.000100', 'mention', 'job_created', 'Task')`); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM slack_threads").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		"INSERT INTO jobs(slack_thread_id, title, status, created_at, updated_at) VALUES (?, 'Task', 'queued', ?, ?)",
		threadID,
		utcNow(),
		utcNow(),
	); err != nil {
		t.Fatal(err)
	}
	var jobID int64
	if err := store.db.QueryRow("SELECT id FROM jobs").Scan(&jobID); err != nil {
		t.Fatal(err)
	}

	service.finishJobFromWorkerOutput(jobID, `OUTCOME: blocked
ACTIONS_TAKEN:
- Checked the workspace and cannot continue without the request ID.
BLOCKER_REASON:
Need a production request ID.
NEXT_USER_ACTION:
Send the failed request ID.
SLACK_REPLY_DRAFT: NONE`)

	var jobStatus, reason, nextAction, threadStatus string
	if err := store.db.QueryRow("SELECT status, current_block_reason, next_user_action FROM jobs WHERE id=?", jobID).Scan(&jobStatus, &reason, &nextAction); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT status FROM slack_threads WHERE id=?", threadID).Scan(&threadStatus); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "blocked" || reason != "Need a production request ID." || nextAction != "Send the failed request ID." {
		t.Fatalf("unexpected job state: status=%q reason=%q next=%q", jobStatus, reason, nextAction)
	}
	if threadStatus != "job_created" {
		t.Fatalf("blocked worker output should not archive thread, got %q", threadStatus)
	}
	var replyCount int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM reply_drafts WHERE job_id=?", jobID).Scan(&replyCount); err != nil {
		t.Fatal(err)
	}
	if replyCount != 0 {
		t.Fatalf("blocked worker output with NONE draft should not create reply, got %d", replyCount)
	}
	var eventCount int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM job_events WHERE job_id=? AND event_type='blocked'", jobID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("expected one blocked event, got %d", eventCount)
	}
}

func TestArchiveReplyIgnoresDraftAndArchivesThread(t *testing.T) {
	_, store := testService(t)
	if _, err := store.db.Exec(`
		INSERT INTO slack_threads(slack_team_id, channel_id, thread_ts, source_type, status, title)
		VALUES ('T1', 'C1', '1764168000.000100', 'mention', 'job_created', 'Task')`); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM slack_threads").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		"INSERT INTO jobs(slack_thread_id, title, status, created_at, updated_at) VALUES (?, 'Task', 'draft_ready', ?, ?)",
		threadID,
		utcNow(),
		utcNow(),
	); err != nil {
		t.Fatal(err)
	}
	var jobID int64
	if err := store.db.QueryRow("SELECT id FROM jobs").Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`
		INSERT INTO reply_drafts(
			job_id, slack_thread_id, status, draft_text, slack_channel_id, slack_thread_ts,
			created_at, updated_at
		) VALUES (?, ?, 'draft', 'Thanks', 'C1', '1764168000.000100', ?, ?)`,
		jobID,
		threadID,
		utcNow(),
		utcNow(),
	); err != nil {
		t.Fatal(err)
	}
	var replyID int64
	if err := store.db.QueryRow("SELECT id FROM reply_drafts").Scan(&replyID); err != nil {
		t.Fatal(err)
	}
	service := &Service{store: store}
	if _, err := service.ArchiveReply(replyID); err != nil {
		t.Fatal(err)
	}
	var replyStatus, threadStatus, jobStatus string
	if err := store.db.QueryRow("SELECT status FROM reply_drafts WHERE id=?", replyID).Scan(&replyStatus); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT status FROM slack_threads WHERE id=?", threadID).Scan(&threadStatus); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT status FROM jobs WHERE id=?", jobID).Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	if replyStatus != "ignored" || threadStatus != "archived" || jobStatus != "completed_no_reply" {
		t.Fatalf("unexpected statuses: reply=%q thread=%q job=%q", replyStatus, threadStatus, jobStatus)
	}
}

func TestSendReplyRejectsProcessNoteDraft(t *testing.T) {
	_, store := testService(t)
	if _, err := store.db.Exec(`
		INSERT INTO slack_threads(slack_team_id, channel_id, thread_ts, source_type, status, title)
		VALUES ('T1', 'C1', '1764168000.000100', 'dm', 'job_created', 'AWS reset')`); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM slack_threads").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`
		INSERT INTO reply_drafts(
			slack_thread_id, status, draft_text, slack_channel_id, slack_thread_ts,
			created_at, updated_at
		) VALUES (?, 'draft', ?, 'C1', '1764168000.000100', ?, ?)`,
		threadID,
		"The service will create a `reply_drafts` row from the final reply marker, so I won’t write directly to the database.",
		utcNow(),
		utcNow(),
	); err != nil {
		t.Fatal(err)
	}
	var replyID int64
	if err := store.db.QueryRow("SELECT id FROM reply_drafts").Scan(&replyID); err != nil {
		t.Fatal(err)
	}
	service := &Service{store: store}
	if _, err := service.SendReply(replyID); err == nil {
		t.Fatal("expected process-note draft to be rejected before Slack send")
	}
}
