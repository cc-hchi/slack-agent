package app

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestUpsertIntakeItemUsesTriggerLatestActivityAndReopensResolvedItem(t *testing.T) {
	service, store := testService(t)
	root := map[string]any{"ts": "1764168000.000100", "text": "Root task"}
	reply := map[string]any{"ts": "1764168060.000200", "thread_ts": "1764168000.000100", "text": "Reply"}
	if err := service.upsertIntakeItem("T1", "C1", "backend", "1764168000.000100", "1764168000.000100", "mention", root, []map[string]any{root, reply}, "U-me"); err != nil {
		t.Fatal(err)
	}

	var title, triggerText, latestTS, latestText string
	if err := store.db.QueryRow(`
		SELECT title, trigger_text, latest_slack_message_ts, latest_text
		FROM intake_items
		WHERE slack_team_id='T1' AND channel_id='C1' AND thread_ts='1764168000.000100'`,
	).Scan(&title, &triggerText, &latestTS, &latestText); err != nil {
		t.Fatal(err)
	}
	if title != "Root task" || triggerText != "Root task" || latestTS != "1764168060.000200" || latestText != "Reply" {
		t.Fatalf("unexpected intake metadata: title=%q trigger_text=%q latest=%q latest_text=%q", title, triggerText, latestTS, latestText)
	}

	if _, err := store.db.Exec("UPDATE intake_items SET status='resolved' WHERE slack_team_id='T1' AND channel_id='C1' AND thread_ts='1764168000.000100'"); err != nil {
		t.Fatal(err)
	}
	if err := service.upsertIntakeItem("T1", "C1", "backend", "1764168000.000100", "1764168000.000100", "mention", root, []map[string]any{root, reply}, "U-me"); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := store.db.QueryRow("SELECT status FROM intake_items WHERE slack_team_id='T1' AND channel_id='C1' AND thread_ts='1764168000.000100'").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "resolved" {
		t.Fatalf("same Slack activity should not recollect terminal thread, got %q", status)
	}

	newReply := map[string]any{"ts": "1764168120.000300", "thread_ts": "1764168000.000100", "text": "New reply"}
	if err := service.upsertIntakeItem("T1", "C1", "backend", "1764168000.000100", "1764168000.000100", "mention", root, []map[string]any{root, reply, newReply}, "U-me"); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT status, latest_slack_message_ts FROM intake_items WHERE slack_team_id='T1' AND channel_id='C1' AND thread_ts='1764168000.000100'").Scan(&status, &latestTS); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || latestTS != "1764168120.000300" {
		t.Fatalf("new Slack activity should recollect thread, status=%q latest=%q", status, latestTS)
	}
}

func TestDashboardIntakeIncludesRecollectedThreadWithExistingJob(t *testing.T) {
	service, store := testService(t)
	root := map[string]any{"ts": "1764168000.000100", "text": "Root task"}
	reply := map[string]any{"ts": "1764168060.000200", "thread_ts": "1764168000.000100", "text": "Reply"}
	if err := service.upsertIntakeItem("T1", "C1", "backend", "1764168000.000100", "1764168000.000100", "mention", root, []map[string]any{root, reply}, "U-me"); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM intake_items").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("UPDATE intake_items SET status='resolved' WHERE id=?", threadID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		"INSERT INTO jobs(intake_item_id, title, status, created_at, updated_at) VALUES (?, 'Old job', 'completed_no_reply', ?, ?)",
		threadID,
		utcNow(),
		utcNow(),
	); err != nil {
		t.Fatal(err)
	}

	newReply := map[string]any{"ts": "1764168120.000300", "thread_ts": "1764168000.000100", "text": "New reply"}
	if err := service.upsertIntakeItem("T1", "C1", "backend", "1764168000.000100", "1764168000.000100", "mention", root, []map[string]any{root, reply, newReply}, "U-me"); err != nil {
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

func TestMissingSyncCursorDefaultsToCurrentSlackTimestamp(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 34, 56, 789123000, time.UTC)

	cursor := slackCursorOrNow("", now)
	if cursor != "1782822896.789123" {
		t.Fatalf("unexpected default cursor %q", cursor)
	}

	after := slackSearchAfterDate(cursor, 3)
	if after != "2026-06-27" {
		t.Fatalf("unexpected search after date: %q", after)
	}

	oldest := slackOldestFromCursor(cursor, 3)
	if oldest != "1782563696.789123" {
		t.Fatalf("unexpected oldest with lookback: %q", oldest)
	}
}

func TestSyncBoundsUseCursorWhenLookbackDisabled(t *testing.T) {
	after := slackSearchAfterDate("1782822896.789123", 0)
	if after != "2026-06-30" {
		t.Fatalf("expected cursor date when search lookback disabled, got %q", after)
	}

	oldest := slackOldestFromCursor("1782822896.789123", 0)
	if oldest != "1782822896.789123" {
		t.Fatalf("expected cursor oldest when lookback disabled, got %q", oldest)
	}
}

func TestSlackSyncIntervalDefaultsAndUsesConfig(t *testing.T) {
	service := &Service{}
	if got := service.slackSyncInterval(); got != 5*time.Minute {
		t.Fatalf("expected default sync interval 5m, got %s", got)
	}

	service.cfg.SlackSyncIntervalSeconds = 42
	if got := service.slackSyncInterval(); got != 42*time.Second {
		t.Fatalf("expected configured sync interval 42s, got %s", got)
	}
}

func TestRelatedJobsForThreadUsesSameChannelMostRecentLimit(t *testing.T) {
	service, store := testService(t)
	if _, err := store.db.Exec(`
		INSERT INTO intake_items(
			slack_team_id, channel_id, channel_name, trigger_ts, thread_ts, source_type, status,
			title, latest_slack_message_ts
		) VALUES ('T1', 'D1', 'DM', '1764167000.000100', '1764167000.000100', 'dm', 'pending', 'Trigger', '1764167000.000100')`); err != nil {
		t.Fatal(err)
	}
	var triggerID int64
	if err := store.db.QueryRow("SELECT id FROM intake_items WHERE title='Trigger'").Scan(&triggerID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 22; i++ {
		result, err := store.db.Exec(`
			INSERT INTO intake_items(
				slack_team_id, channel_id, channel_name, trigger_ts, thread_ts, source_type, status,
				title, latest_slack_message_ts
			) VALUES ('T1', 'D1', 'DM', ?, ?, 'dm', 'resolved', ?, ?)`,
			fmt.Sprintf("1764168%03d.000100", i),
			fmt.Sprintf("1764168%03d.000100", i),
			fmt.Sprintf("Thread %02d", i),
			fmt.Sprintf("1764168%03d.000100", i),
		)
		if err != nil {
			t.Fatal(err)
		}
		threadID, _ := result.LastInsertId()
		if _, err := store.db.Exec(
			"INSERT INTO jobs(intake_item_id, title, status, created_at, updated_at) VALUES (?, ?, 'queued', ?, ?)",
			threadID,
			fmt.Sprintf("same-%02d", i),
			"2026-06-26T09:00:00Z",
			fmt.Sprintf("2026-06-26T10:%02d:30Z", i),
		); err != nil {
			t.Fatal(err)
		}
	}
	result, err := store.db.Exec(`
		INSERT INTO intake_items(
			slack_team_id, channel_id, channel_name, trigger_ts, thread_ts, source_type, status,
			title, latest_slack_message_ts
		) VALUES ('T1', 'D2', 'Other DM', '1764169000.000100', '1764169000.000100', 'dm', 'resolved', 'Other channel', '1764169000.000100')`)
	if err != nil {
		t.Fatal(err)
	}
	otherThreadID, _ := result.LastInsertId()
	if _, err := store.db.Exec(
		"INSERT INTO jobs(intake_item_id, title, status, created_at, updated_at) VALUES (?, 'other-channel', 'queued', ?, ?)",
		otherThreadID,
		"2026-06-26T11:00:00Z",
		"2026-06-26T11:00:00Z",
	); err != nil {
		t.Fatal(err)
	}

	related := service.relatedJobsForThread(triggerID, 20)
	if len(related) != 20 {
		t.Fatalf("expected 20 related jobs, got %d", len(related))
	}
	if related[0]["title"] != "same-21" {
		t.Fatalf("expected newest same-channel job first, got %#v", related[0]["title"])
	}
	if related[len(related)-1]["title"] != "same-02" {
		t.Fatalf("expected limit to keep top 20 same-channel jobs, got last title %#v", related[len(related)-1]["title"])
	}
	for _, job := range related {
		if job["title"] == "other-channel" {
			t.Fatalf("related jobs should not include other channels: %#v", related)
		}
	}
}

func TestDashboardSnapshotIncludesRootUserForRepliesAndJobs(t *testing.T) {
	service, store := testService(t)
	root := map[string]any{"ts": "1764168000.000100", "text": "Need help", "user": "U1"}
	if err := service.upsertIntakeItem("T1", "C1", "DM", "1764168000.000100", "1764168000.000100", "dm", root, []map[string]any{root}, "U-me"); err != nil {
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
	if err := store.db.QueryRow("SELECT id FROM intake_items").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		"INSERT INTO jobs(intake_item_id, title, status, created_at, updated_at) VALUES (?, 'Need help', 'draft_ready', ?, ?)",
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
			job_id, intake_item_id, status, draft_text, slack_channel_id, slack_thread_ts,
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
	if got := snapshot.ReplyDrafts[0]["root_display_name"]; got != "Tim Zhou" {
		t.Fatalf("reply draft root user = %v, want Tim Zhou", got)
	}
	if len(snapshot.Jobs) != 1 {
		t.Fatalf("expected one job, got %d", len(snapshot.Jobs))
	}
	if got := snapshot.Jobs[0]["root_display_name"]; got != "Tim Zhou" {
		t.Fatalf("job root user = %v, want Tim Zhou", got)
	}
}

func TestRetryJobAfterWorkerErrorQueuesJobAndKeepsExecutionLog(t *testing.T) {
	service, store := testService(t)
	if _, err := store.db.Exec(`
		INSERT INTO intake_items(slack_team_id, channel_id, trigger_ts, thread_ts, source_type, status, title)
		VALUES ('T1', 'C1', '1764168000.000100', '1764168000.000100', 'dm', 'resolved', 'Task')`); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM intake_items").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`
		INSERT INTO jobs(
			intake_item_id, title, status, current_block_reason, next_user_action,
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
		INSERT INTO intake_items(slack_team_id, channel_id, trigger_ts, thread_ts, source_type, status, title)
		VALUES ('T1', 'C1', '1764168000.000100', '1764168000.000100', 'dm', 'resolved', 'Task')`); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM intake_items").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		"INSERT INTO jobs(intake_item_id, title, status, created_at, updated_at) VALUES (?, 'Task', 'queued', ?, ?)",
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

func TestAdvanceIntakeDoesNotLetFullAnalysisQueueBlockJobs(t *testing.T) {
	service, store := testService(t)
	service.analyses = make(chan int64, 1)
	service.jobs = make(chan int64, 1)
	service.activeAnalyses = map[int64]struct{}{}
	service.queuedJobs = map[int64]struct{}{}
	service.workingJobs = map[int64]struct{}{}
	service.analyses <- 999

	if _, err := store.db.Exec(`
		INSERT INTO intake_items(slack_team_id, channel_id, trigger_ts, thread_ts, source_type, status, title, latest_slack_message_ts)
		VALUES ('T1', 'C-analysis', '1764168000.000100', '1764168000.000100', 'dm', 'pending', 'Analysis backlog', '1764168000.000100')`); err != nil {
		t.Fatal(err)
	}
	var analysisThreadID int64
	if err := store.db.QueryRow("SELECT id FROM intake_items WHERE channel_id='C-analysis'").Scan(&analysisThreadID); err != nil {
		t.Fatal(err)
	}

	if _, err := store.db.Exec(`
		INSERT INTO intake_items(slack_team_id, channel_id, trigger_ts, thread_ts, source_type, status, title)
		VALUES ('T1', 'C-job', '1764168060.000200', '1764168060.000200', 'dm', 'resolved', 'Worker task')`); err != nil {
		t.Fatal(err)
	}
	var jobThreadID int64
	if err := store.db.QueryRow("SELECT id FROM intake_items WHERE channel_id='C-job'").Scan(&jobThreadID); err != nil {
		t.Fatal(err)
	}
	result, err := store.db.Exec(
		"INSERT INTO jobs(intake_item_id, title, status, created_at, updated_at) VALUES (?, 'Worker task', 'queued', ?, ?)",
		jobThreadID,
		utcNow(),
		utcNow(),
	)
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		service.advanceIntake()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("advanceIntake blocked behind a full analysis queue")
	}
	select {
	case got := <-service.jobs:
		if got != jobID {
			t.Fatalf("queued job id = %d, want %d", got, jobID)
		}
	default:
		t.Fatal("expected queued job to be enqueued despite full analysis queue")
	}
	if _, exists := service.activeAnalyses[analysisThreadID]; exists {
		t.Fatal("full analysis queue should not mark the thread as queued in memory")
	}
}

func TestRecoverInterruptedJobsRequeuesObsoleteInFlightStates(t *testing.T) {
	service, store := testService(t)
	if _, err := store.db.Exec(`
		INSERT INTO intake_items(slack_team_id, channel_id, trigger_ts, thread_ts, source_type, status, title)
		VALUES ('T1', 'C1', '1764168000.000100', '1764168000.000100', 'dm', 'resolved', 'Task')`); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM intake_items").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"workspace_creating", "bootstrapping", "working", "draft_ready"} {
		if _, err := store.db.Exec(
			"INSERT INTO jobs(intake_item_id, title, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
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
		INSERT INTO intake_items(slack_team_id, channel_id, trigger_ts, thread_ts, source_type, status, title)
		VALUES ('T1', 'C1', '1764168000.000100', '1764168000.000100', 'dm', 'resolved', 'Task')`); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM intake_items").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"Active task", "Queued task"} {
		if _, err := store.db.Exec(
			"INSERT INTO jobs(intake_item_id, title, status, created_at, updated_at) VALUES (?, ?, 'queued', ?, ?)",
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
		INSERT INTO intake_items(slack_team_id, channel_id, trigger_ts, thread_ts, source_type, status, title)
		VALUES ('T1', 'C1', '1764168000.000100', '1764168000.000100', 'dm', 'resolved', 'Task')`); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM intake_items").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(service.cfg.WorkspaceRoot, "job-1")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		`INSERT INTO jobs(
			intake_item_id, title, status, workspace_path, bootstrap_status, created_at, updated_at
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

func TestJobWorkspaceFallsBackToSlackThreadWorkspace(t *testing.T) {
	service, _ := testService(t)

	workspace := service.jobWorkspace(7, map[string]any{"intake_item_id": int64(42)})
	want := service.threadWorkspace(42)
	if workspace != want {
		t.Fatalf("job workspace = %q, want %q", workspace, want)
	}
}

func TestJobWorkspacePrefersPersistedWorkspacePath(t *testing.T) {
	service, _ := testService(t)
	persisted := filepath.Join(service.cfg.WorkspaceRoot, "custom")

	workspace := service.jobWorkspace(7, map[string]any{
		"intake_item_id": int64(42),
		"workspace_path": persisted,
	})
	if workspace != persisted {
		t.Fatalf("job workspace = %q, want persisted path %q", workspace, persisted)
	}
}

func TestEnsureAnalyzerWorkspaceRunsBootstrap(t *testing.T) {
	service, _ := testService(t)
	service.cfg.WorkspaceBootstrapCommand = "touch analyzer_bootstrap_marker"
	workspace := service.threadWorkspace(1)

	if ok := service.ensureAnalyzerWorkspace(1, 1, workspace); !ok {
		t.Fatal("expected analyzer workspace setup to succeed")
	}
	if _, err := os.Stat(filepath.Join(workspace, "analyzer_bootstrap_marker")); err != nil {
		t.Fatalf("expected analyzer bootstrap marker, stat err=%v", err)
	}
}

func TestFinishAnalysisErrorKeepsThreadQueuedForRetry(t *testing.T) {
	service, store := testService(t)
	if _, err := store.db.Exec(`
		INSERT INTO intake_items(slack_team_id, channel_id, trigger_ts, thread_ts, source_type, status, title)
		VALUES ('T1', 'C1', '1764168000.000100', '1764168000.000100', 'dm', 'pending', 'Task')`); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM intake_items").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	result, err := store.db.Exec("INSERT INTO analysis_runs(intake_item_id, status, started_at) VALUES (?, 'running', ?)", threadID, utcNow())
	if err != nil {
		t.Fatal(err)
	}
	analysisID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	service.finishAnalysisError(threadID, analysisID, errors.New("thread/start failed"))

	var threadStatus, runStatus, runError string
	if err := store.db.QueryRow("SELECT status FROM intake_items WHERE id=?", threadID).Scan(&threadStatus); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT status, error FROM analysis_runs WHERE id=?", analysisID).Scan(&runStatus, &runError); err != nil {
		t.Fatal(err)
	}
	if threadStatus != "pending" || runStatus != "failed" || runError != "thread/start failed" {
		t.Fatalf("unexpected analysis retry state: thread=%q run=%q error=%q", threadStatus, runStatus, runError)
	}
}

func TestWorkerSessionStoresCodexSessionID(t *testing.T) {
	service, store := testService(t)
	if _, err := store.db.Exec(`
		INSERT INTO intake_items(slack_team_id, channel_id, trigger_ts, thread_ts, source_type, status, title)
		VALUES ('T1', 'C1', '1764168000.000100', '1764168000.000100', 'dm', 'resolved', 'Task')`); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM intake_items").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	result, err := store.db.Exec(
		"INSERT INTO jobs(intake_item_id, title, status, created_at, updated_at) VALUES (?, 'Task', 'queued', ?, ?)",
		threadID,
		utcNow(),
		utcNow(),
	)
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	sessionID := service.startWorkerSession(jobID, CodexThreadRef{ThreadID: "thread-1", SessionID: "session-1"})
	service.finishWorkerSession(sessionID, "completed", "")

	var threadRef, sessionRef, status string
	if err := store.db.QueryRow(
		"SELECT codex_thread_id, codex_session_id, status FROM worker_sessions WHERE id=?",
		sessionID,
	).Scan(&threadRef, &sessionRef, &status); err != nil {
		t.Fatal(err)
	}
	if threadRef != "thread-1" || sessionRef != "session-1" || status != "completed" {
		t.Fatalf("unexpected worker session: thread=%q session=%q status=%q", threadRef, sessionRef, status)
	}
}

func TestRecoverInterruptedWorkerSessionsMarksRunningFailed(t *testing.T) {
	service, store := testService(t)
	if _, err := store.db.Exec(`
		INSERT INTO intake_items(slack_team_id, channel_id, trigger_ts, thread_ts, source_type, status, title)
		VALUES ('T1', 'C1', '1764168000.000100', '1764168000.000100', 'dm', 'resolved', 'Task')`); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM intake_items").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	result, err := store.db.Exec(
		"INSERT INTO jobs(intake_item_id, title, status, created_at, updated_at) VALUES (?, 'Task', 'queued', ?, ?)",
		threadID,
		utcNow(),
		utcNow(),
	)
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		`INSERT INTO worker_sessions(job_id, codex_thread_id, codex_session_id, status, started_at)
		 VALUES (?, 'thread-1', 'session-1', 'running', ?)`,
		jobID,
		utcNow(),
	); err != nil {
		t.Fatal(err)
	}

	service.recoverInterruptedWorkerSessions()

	var status, sessionErr string
	if err := store.db.QueryRow("SELECT status, error FROM worker_sessions WHERE job_id=?", jobID).Scan(&status, &sessionErr); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || sessionErr != "Worker session interrupted before completion." {
		t.Fatalf("unexpected recovered session: status=%q error=%q", status, sessionErr)
	}
}

func TestMigrateResetsLegacySlackThreadSchema(t *testing.T) {
	cfg := Config{DatabasePath: filepath.Join(t.TempDir(), "slack_agent.db")}
	store, err := OpenStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.db.Exec(`
CREATE TABLE metadata (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
INSERT INTO metadata(key, value) VALUES ('slack_sync.search.mention.latest_ts', '9999999999.000000');
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
);
INSERT INTO slack_threads(slack_team_id, channel_id, thread_ts, source_type, status, title)
VALUES ('T1', 'C1', '1764168000.000100', 'mention', 'collected', 'legacy');
CREATE TABLE analysis_runs (
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
CREATE TABLE jobs (
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
CREATE TABLE worker_sessions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  codex_thread_id TEXT,
  status TEXT NOT NULL,
  started_at TEXT,
  completed_at TEXT,
  error TEXT
)`,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}

	if exists, err := store.tableExists("slack_threads"); err != nil {
		t.Fatal(err)
	} else if exists {
		t.Fatal("legacy slack_threads table should be dropped")
	}
	if exists, err := store.tableExists("intake_items"); err != nil {
		t.Fatal(err)
	} else if !exists {
		t.Fatal("expected intake_items table")
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM intake_items").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("legacy intake cache should be reset, got %d rows", count)
	}
	var cursorCount int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM metadata WHERE key LIKE 'slack_sync.%'").Scan(&cursorCount); err != nil {
		t.Fatal(err)
	}
	if cursorCount != 0 {
		t.Fatalf("legacy Slack cursors should be reset, got %d", cursorCount)
	}

	tableColumns := func(table string) map[string]bool {
		t.Helper()
		columns := map[string]bool{}
		rows, err := store.db.Query("PRAGMA table_info(" + table + ")")
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
		return columns
	}
	assertColumns := func(table string, expected []string) {
		t.Helper()
		columns := tableColumns(table)
		for _, column := range expected {
			if !columns[column] {
				t.Fatalf("expected migrated column %s.%s", table, column)
			}
		}
	}
	assertMissingColumns := func(table string, expected []string) {
		t.Helper()
		columns := tableColumns(table)
		for _, column := range expected {
			if columns[column] {
				t.Fatalf("expected migrated column %s.%s to be removed", table, column)
			}
		}
	}
	assertColumns("intake_items", []string{
		"trigger_ts",
		"trigger_user_id",
		"trigger_text",
		"resolution",
		"latest_slack_message_ts",
		"latest_user_id",
		"latest_user_name",
		"latest_text",
		"last_analyzed_slack_ts",
	})
	assertMissingColumns("intake_items", []string{
		"root_message_ts",
		"root_user_name",
		"last_slack_activity_at",
		"message_count",
		"mentions_user",
	})
	assertColumns("analysis_runs", []string{"codex_session_id"})
	assertColumns("jobs", []string{"codex_session_id"})
	assertColumns("worker_sessions", []string{"codex_session_id"})
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
		INSERT INTO intake_items(slack_team_id, channel_id, trigger_ts, thread_ts, source_type, status, title)
		VALUES ('T1', 'C1', '1764168000.000100', '1764168000.000100', 'mention', 'resolved', 'Task')`); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM intake_items").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		"INSERT INTO jobs(intake_item_id, title, status, created_at, updated_at) VALUES (?, 'Task', 'queued', ?, ?)",
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
	if err := store.db.QueryRow("SELECT status FROM intake_items WHERE id=?", threadID).Scan(&threadStatus); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "blocked" || reason != "Need a production request ID." || nextAction != "Send the failed request ID." {
		t.Fatalf("unexpected job state: status=%q reason=%q next=%q", jobStatus, reason, nextAction)
	}
	if threadStatus != "resolved" {
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
		INSERT INTO intake_items(slack_team_id, channel_id, trigger_ts, thread_ts, source_type, status, title)
		VALUES ('T1', 'C1', '1764168000.000100', '1764168000.000100', 'mention', 'resolved', 'Task')`); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM intake_items").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		"INSERT INTO jobs(intake_item_id, title, status, created_at, updated_at) VALUES (?, 'Task', 'draft_ready', ?, ?)",
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
			job_id, intake_item_id, status, draft_text, slack_channel_id, slack_thread_ts,
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
	if err := store.db.QueryRow("SELECT status FROM intake_items WHERE id=?", threadID).Scan(&threadStatus); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT status FROM jobs WHERE id=?", jobID).Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	if replyStatus != "ignored" || threadStatus != "resolved" || jobStatus != "completed_no_reply" {
		t.Fatalf("unexpected statuses: reply=%q thread=%q job=%q", replyStatus, threadStatus, jobStatus)
	}
}

func TestSendReplyRejectsProcessNoteDraft(t *testing.T) {
	_, store := testService(t)
	if _, err := store.db.Exec(`
		INSERT INTO intake_items(slack_team_id, channel_id, trigger_ts, thread_ts, source_type, status, title)
		VALUES ('T1', 'C1', '1764168000.000100', '1764168000.000100', 'dm', 'resolved', 'AWS reset')`); err != nil {
		t.Fatal(err)
	}
	var threadID int64
	if err := store.db.QueryRow("SELECT id FROM intake_items").Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`
		INSERT INTO reply_drafts(
			intake_item_id, status, draft_text, slack_channel_id, slack_thread_ts,
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
