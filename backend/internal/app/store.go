package app

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

type Store struct {
	db  *sql.DB
	cfg Config
}

func OpenStore(cfg Config) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(cfg.DatabasePath), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON; PRAGMA journal_mode = WAL;"); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, cfg: cfg}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Migrate() error {
	if _, err := s.db.Exec(schemaSQL); err != nil {
		return err
	}
	if err := s.ensureColumn("slack_threads", "latest_slack_message_ts", "TEXT"); err != nil {
		return err
	}
	if err := s.ensureColumn("slack_threads", "last_analyzed_slack_ts", "TEXT"); err != nil {
		return err
	}
	if _, err := s.db.Exec("CREATE INDEX IF NOT EXISTS idx_slack_threads_latest_ts ON slack_threads(latest_slack_message_ts)"); err != nil {
		return err
	}
	if err := s.backfillSlackThreadActivity(); err != nil {
		return err
	}
	_, err := s.db.Exec(
		"INSERT OR REPLACE INTO metadata(key, value) VALUES (?, ?)",
		"schema_version",
		"2",
	)
	return err
}

func (s *Store) ensureColumn(table, column, definition string) error {
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + definition)
	return err
}

func (s *Store) backfillSlackThreadActivity() error {
	rows, err := s.db.Query(`
		SELECT st.id,
		       COALESCE(NULLIF(st.latest_slack_message_ts, ''), (
		         SELECT MAX(sm.slack_message_ts)
		         FROM slack_messages sm
		         WHERE sm.slack_thread_id = st.id
		       )) AS latest_slack_message_ts
		FROM slack_threads st`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type update struct {
		id         int64
		latestTS   string
		activityAt string
	}
	var updates []update
	for rows.Next() {
		var id int64
		var latestTS sql.NullString
		if err := rows.Scan(&id, &latestTS); err != nil {
			return err
		}
		if !latestTS.Valid || strings.TrimSpace(latestTS.String) == "" {
			continue
		}
		parsed, ok := parseSlackTimestamp(latestTS.String)
		if !ok {
			continue
		}
		updates = append(updates, update{
			id:         id,
			latestTS:   latestTS.String,
			activityAt: parsed.UTC().Format(time.RFC3339),
		})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, update := range updates {
		if _, err := s.db.Exec(
			"UPDATE slack_threads SET latest_slack_message_ts=?, last_slack_activity_at=? WHERE id=?",
			update.latestTS,
			update.activityAt,
			update.id,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) SeedDemoData() error {
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM slack_threads").Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	now := utcNow()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	threads := []struct {
		ChannelID   string
		ChannelName string
		ThreadTS    string
		SourceType  string
		Status      string
		Title       string
	}{
		{"C-BACKEND", "#backend", "1764168480.000100", "mention", "job_created", "API error on export"},
		{"C-PAYMENTS", "#payments", "1764168120.000200", "user_participated", "job_created", "Payment webhook failing"},
		{"D-PRIYA", "DM with Priya Shah", "1764167600.000300", "dm", "job_created", "S3 upload error on large files"},
	}
	for _, thread := range threads {
		if _, err := tx.Exec(`
			INSERT INTO slack_threads(
				slack_team_id, channel_id, channel_name, thread_ts, root_message_ts,
				source_type, status, title, permalink, last_slack_activity_at,
				last_synced_at, raw_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"T-DEMO",
			thread.ChannelID,
			thread.ChannelName,
			thread.ThreadTS,
			thread.ThreadTS,
			thread.SourceType,
			thread.Status,
			thread.Title,
			fmt.Sprintf("https://slack.example.local/archives/%s/p%s", thread.ChannelID, strings.ReplaceAll(thread.ThreadTS, ".", "")),
			now,
			now,
			mustJSON(map[string]any{"sample": true}),
		); err != nil {
			return err
		}
	}

	rows, err := tx.Query("SELECT id, thread_ts, source_type, title FROM slack_threads ORDER BY id")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var threadTS, sourceType, title string
		if err := rows.Scan(&id, &threadTS, &sourceType, &title); err != nil {
			return err
		}
		mentions := 0
		if sourceType == "mention" {
			mentions = 1
		}
		if _, err := tx.Exec(`
			INSERT INTO slack_messages(
				slack_thread_id, slack_message_ts, user_id, user_name, text,
				is_user_message, mentions_user, raw_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id,
			threadTS,
			"U-DEMO",
			"Ava K.",
			title+": can you take a look?",
			0,
			mentions,
			mustJSON(map[string]any{"sample": true}),
		); err != nil {
			return err
		}
	}

	analysis := []struct {
		ThreadID   int
		Confidence float64
		Summary    string
		Rationale  string
	}{
		{1, 0.92, "Code issue", "Export fails when total is undefined."},
		{2, 0.84, "Webhook investigation", "Need request payload or request ID to verify provider failure."},
		{3, 0.78, "Infra permission issue", "Likely S3 policy or multipart size handling."},
	}
	for _, run := range analysis {
		if _, err := tx.Exec(`
			INSERT INTO analysis_runs(
				slack_thread_id, codex_thread_id, status, action_required, confidence,
				summary, rationale, structured_result_json, started_at, completed_at, error
			) VALUES (?, NULL, 'completed', 1, ?, ?, ?, ?, ?, ?, NULL)`,
			run.ThreadID,
			run.Confidence,
			run.Summary,
			run.Rationale,
			mustJSON(map[string]any{"sample": true}),
			now,
			now,
		); err != nil {
			return err
		}
	}

	jobs := []struct {
		ThreadID       int
		AnalysisID     int
		Title          string
		Status         string
		Urgency        string
		TaskType       string
		Workspace      string
		CodexThreadID  string
		BlockReason    any
		NextUserAction any
	}{
		{1, 1, "API error on export", "draft_ready", "high", "code_issue", "data/workspaces/demo-export", "codex-demo-1", nil, nil},
		{2, 2, "Payment webhook failing", "blocked", "high", "investigation", "data/workspaces/demo-webhook", "codex-demo-2", "Need sample payload or request ID", "Confirm which failed payment/request to inspect."},
		{3, 3, "S3 upload error on large files", "blocked", "medium", "infra", "data/workspaces/demo-s3", "codex-demo-3", "Need IAM policy details", "Confirm whether worker can inspect AWS staging credentials."},
	}
	for _, job := range jobs {
		if _, err := tx.Exec(`
			INSERT INTO jobs(
				slack_thread_id, analysis_run_id, title, status, urgency, task_type,
				workspace_path, bootstrap_status, codex_thread_id, current_block_reason,
				next_user_action, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, 'succeeded', ?, ?, ?, ?, ?)`,
			job.ThreadID,
			job.AnalysisID,
			job.Title,
			job.Status,
			job.Urgency,
			job.TaskType,
			job.Workspace,
			job.CodexThreadID,
			job.BlockReason,
			job.NextUserAction,
			now,
			now,
		); err != nil {
			return err
		}
	}
	events := []struct {
		JobID     int
		EventType string
		Message   string
		Payload   map[string]any
	}{
		{1, "worker_completed", "Reproduced issue, changed guard, tests passed.", map[string]any{"tests": "8 passed"}},
		{2, "blocked", "Need sample payload or request ID.", map[string]any{"need": "request_id"}},
		{3, "blocked", "Need IAM policy details.", map[string]any{"need": "iam_policy"}},
	}
	for _, event := range events {
		if _, err := tx.Exec(
			"INSERT INTO job_events(job_id, event_type, message, payload_json, created_at) VALUES (?, ?, ?, ?, ?)",
			event.JobID,
			event.EventType,
			event.Message,
			mustJSON(event.Payload),
			now,
		); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`
		INSERT INTO reply_drafts(
			job_id, slack_thread_id, status, draft_text, edited_text, rationale,
			slack_channel_id, slack_thread_ts, slack_message_ts, sent_permalink,
			created_at, updated_at
		) VALUES (?, ?, 'draft', ?, NULL, ?, ?, ?, NULL, NULL, ?, ?)`,
		1,
		1,
		"Thanks for flagging this, Ava. I reproduced the export failure when `total` is undefined and added a fallback plus a regression test. The fix is ready in the local branch; I can open the PR next.",
		"Worker completed a localized fix and tests.",
		"C-BACKEND",
		"1764168480.000100",
		now,
		now,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DashboardSnapshot() (DashboardSnapshot, error) {
	replyDrafts, err := s.queryMaps(`
		SELECT rd.*, j.title AS job_title, j.status AS job_status,
		       st.title AS thread_title, st.channel_name, st.channel_id, st.source_type,
		       st.thread_ts, st.permalink, st.last_slack_activity_at,
		       ar.confidence, ar.summary AS analyzer_summary, ar.rationale AS analyzer_rationale,
		       (
		         SELECT COALESCE(
		           NULLIF(CASE WHEN LENGTH(TRIM(COALESCE(su.display_name, ''))) > 1 THEN su.display_name ELSE '' END, ''),
		           NULLIF(su.real_name, ''),
		           NULLIF(sm.user_name, ''),
		           sm.user_id
		         )
		         FROM slack_messages sm
		         LEFT JOIN slack_users su ON su.user_id = sm.user_id
		         WHERE sm.slack_thread_id = st.id
		         ORDER BY sm.slack_message_ts
		         LIMIT 1
		       ) AS root_user_name,
		       (
		         SELECT sm.user_id
		         FROM slack_messages sm
		         WHERE sm.slack_thread_id = st.id
		         ORDER BY sm.slack_message_ts
		         LIMIT 1
		       ) AS root_user_id,
		       (
		         SELECT sm.text
		         FROM slack_messages sm
		         WHERE sm.slack_thread_id = st.id
		         ORDER BY sm.slack_message_ts
		         LIMIT 1
		       ) AS root_text
		FROM reply_drafts rd
		LEFT JOIN jobs j ON j.id = rd.job_id
		LEFT JOIN slack_threads st ON st.id = rd.slack_thread_id
		LEFT JOIN analysis_runs ar ON ar.id = j.analysis_run_id
		WHERE rd.status IN ('draft', 'edited', 'send_failed')
		ORDER BY rd.updated_at DESC`)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	blocked, err := s.queryMaps(`
		SELECT 'job' AS item_type, j.*, st.channel_name, st.channel_id, st.source_type,
		       st.thread_ts, st.permalink, st.title AS thread_title, st.last_slack_activity_at,
		       (
		         SELECT COALESCE(
		           NULLIF(CASE WHEN LENGTH(TRIM(COALESCE(su.display_name, ''))) > 1 THEN su.display_name ELSE '' END, ''),
		           NULLIF(su.real_name, ''),
		           NULLIF(sm.user_name, ''),
		           sm.user_id
		         )
		         FROM slack_messages sm
		         LEFT JOIN slack_users su ON su.user_id = sm.user_id
		         WHERE sm.slack_thread_id = st.id
		         ORDER BY sm.slack_message_ts
		         LIMIT 1
		       ) AS root_user_name,
		       (
		         SELECT sm.user_id
		         FROM slack_messages sm
		         WHERE sm.slack_thread_id = st.id
		         ORDER BY sm.slack_message_ts
		         LIMIT 1
		       ) AS root_user_id,
		       (
		         SELECT sm.text
		         FROM slack_messages sm
		         WHERE sm.slack_thread_id = st.id
		         ORDER BY sm.slack_message_ts
		         LIMIT 1
		       ) AS root_text
		FROM jobs j
		JOIN slack_threads st ON st.id = j.slack_thread_id
		WHERE j.status IN ('blocked', 'failed')
		ORDER BY j.updated_at DESC`)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	analysisFailures, err := s.queryMaps(`
			SELECT 'thread' AS item_type, st.id, st.id AS slack_thread_id, st.title,
			       st.status, st.channel_name, st.channel_id, st.thread_ts, st.source_type, st.permalink,
			       st.last_synced_at AS updated_at,
			       COALESCE(ar.error, 'Analyzer failed') AS current_block_reason,
			       'Retry analysis after reviewing the analyzer failure.' AS next_user_action,
			       (
			         SELECT COALESCE(
			           NULLIF(CASE WHEN LENGTH(TRIM(COALESCE(su.display_name, ''))) > 1 THEN su.display_name ELSE '' END, ''),
			           NULLIF(su.real_name, ''),
			           NULLIF(sm.user_name, ''),
			           sm.user_id
			         )
			         FROM slack_messages sm
			         LEFT JOIN slack_users su ON su.user_id = sm.user_id
			         WHERE sm.slack_thread_id = st.id
			         ORDER BY sm.slack_message_ts
			         LIMIT 1
			       ) AS root_user_name,
			       (
			         SELECT sm.user_id
			         FROM slack_messages sm
			         WHERE sm.slack_thread_id = st.id
			         ORDER BY sm.slack_message_ts
			         LIMIT 1
			       ) AS root_user_id,
			       (
			         SELECT sm.text
			         FROM slack_messages sm
			         WHERE sm.slack_thread_id = st.id
			         ORDER BY sm.slack_message_ts
			         LIMIT 1
			       ) AS root_text
			FROM slack_threads st
			LEFT JOIN analysis_runs ar ON ar.id = (
				SELECT id FROM analysis_runs
			WHERE slack_thread_id = st.id
			ORDER BY id DESC
			LIMIT 1
		)
			WHERE st.status = 'analysis_failed'
			ORDER BY st.last_synced_at DESC`)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	blocked = append(blocked, analysisFailures...)
	queued, err := s.queryMaps(`
			SELECT j.*, st.channel_name, st.channel_id, st.thread_ts, st.source_type, st.permalink,
			       st.title AS thread_title, st.last_slack_activity_at,
			       (
			         SELECT COALESCE(
			           NULLIF(CASE WHEN LENGTH(TRIM(COALESCE(su.display_name, ''))) > 1 THEN su.display_name ELSE '' END, ''),
			           NULLIF(su.real_name, ''),
			           NULLIF(sm.user_name, ''),
			           sm.user_id
			         )
			         FROM slack_messages sm
			         LEFT JOIN slack_users su ON su.user_id = sm.user_id
			         WHERE sm.slack_thread_id = st.id
			         ORDER BY sm.slack_message_ts
			         LIMIT 1
			       ) AS root_user_name,
			       (
			         SELECT sm.user_id
			         FROM slack_messages sm
			         WHERE sm.slack_thread_id = st.id
			         ORDER BY sm.slack_message_ts
			         LIMIT 1
			       ) AS root_user_id,
			       (
			         SELECT sm.text
			         FROM slack_messages sm
			         WHERE sm.slack_thread_id = st.id
			         ORDER BY sm.slack_message_ts
			         LIMIT 1
			       ) AS root_text
			FROM jobs j
			JOIN slack_threads st ON st.id = j.slack_thread_id
			WHERE j.status = 'queued'
			ORDER BY j.updated_at DESC`)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	intake, err := s.queryMaps(`
		SELECT st.*, ar.confidence, ar.summary AS analyzer_summary,
		       (
		         SELECT COALESCE(
		           NULLIF(CASE WHEN LENGTH(TRIM(COALESCE(su.display_name, ''))) > 1 THEN su.display_name ELSE '' END, ''),
		           NULLIF(su.real_name, ''),
		           NULLIF(sm.user_name, ''),
		           sm.user_id
		         )
		         FROM slack_messages sm
		         LEFT JOIN slack_users su ON su.user_id = sm.user_id
		         WHERE sm.slack_thread_id = st.id
		         ORDER BY sm.slack_message_ts
		         LIMIT 1
		       ) AS root_user_name,
		       (
		         SELECT sm.user_id
		         FROM slack_messages sm
		         WHERE sm.slack_thread_id = st.id
		         ORDER BY sm.slack_message_ts
		         LIMIT 1
		       ) AS root_user_id,
		       (
		         SELECT sm.text
		         FROM slack_messages sm
		         WHERE sm.slack_thread_id = st.id
		         ORDER BY sm.slack_message_ts
		         LIMIT 1
		       ) AS root_text
			FROM slack_threads st
			LEFT JOIN analysis_runs ar ON ar.id = (
				SELECT id FROM analysis_runs
				WHERE slack_thread_id = st.id
				ORDER BY id DESC
				LIMIT 1
			)
			WHERE st.status IN ('collected', 'analysis_queued', 'analyzing')
			ORDER BY st.last_slack_activity_at DESC
			LIMIT 80`)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	archive, err := s.queryMaps(`
		SELECT st.*, ar.confidence, ar.summary AS analyzer_summary,
		       (
		         SELECT COALESCE(
		           NULLIF(CASE WHEN LENGTH(TRIM(COALESCE(su.display_name, ''))) > 1 THEN su.display_name ELSE '' END, ''),
		           NULLIF(su.real_name, ''),
		           NULLIF(sm.user_name, ''),
		           sm.user_id
		         )
		         FROM slack_messages sm
		         LEFT JOIN slack_users su ON su.user_id = sm.user_id
		         WHERE sm.slack_thread_id = st.id
		         ORDER BY sm.slack_message_ts
		         LIMIT 1
		       ) AS root_user_name,
		       (
		         SELECT sm.user_id
		         FROM slack_messages sm
		         WHERE sm.slack_thread_id = st.id
		         ORDER BY sm.slack_message_ts
		         LIMIT 1
		       ) AS root_user_id,
		       (
		         SELECT sm.text
		         FROM slack_messages sm
		         WHERE sm.slack_thread_id = st.id
		         ORDER BY sm.slack_message_ts
		         LIMIT 1
		       ) AS root_text
			FROM slack_threads st
			LEFT JOIN analysis_runs ar ON ar.id = (
				SELECT id FROM analysis_runs
				WHERE slack_thread_id = st.id
				ORDER BY id DESC
				LIMIT 1
			)
		WHERE st.status IN ('no_action', 'archived')
		ORDER BY st.last_slack_activity_at DESC
		LIMIT 80`)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	threads, err := s.queryMaps(`
		SELECT st.*, ar.confidence, ar.summary AS analyzer_summary,
		       (
		         SELECT COALESCE(
		           NULLIF(CASE WHEN LENGTH(TRIM(COALESCE(su.display_name, ''))) > 1 THEN su.display_name ELSE '' END, ''),
		           NULLIF(su.real_name, ''),
		           NULLIF(sm.user_name, ''),
		           sm.user_id
		         )
		         FROM slack_messages sm
		         LEFT JOIN slack_users su ON su.user_id = sm.user_id
		         WHERE sm.slack_thread_id = st.id
		         ORDER BY sm.slack_message_ts
		         LIMIT 1
		       ) AS root_user_name,
		       (
		         SELECT sm.user_id
		         FROM slack_messages sm
		         WHERE sm.slack_thread_id = st.id
		         ORDER BY sm.slack_message_ts
		         LIMIT 1
		       ) AS root_user_id,
		       (
		         SELECT sm.text
		         FROM slack_messages sm
		         WHERE sm.slack_thread_id = st.id
		         ORDER BY sm.slack_message_ts
		         LIMIT 1
		       ) AS root_text
			FROM slack_threads st
			LEFT JOIN analysis_runs ar ON ar.id = (
				SELECT id FROM analysis_runs
				WHERE slack_thread_id = st.id
				ORDER BY id DESC
				LIMIT 1
			)
		ORDER BY st.last_slack_activity_at DESC
		LIMIT 50`)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	jobs, err := s.queryMaps(`
			SELECT j.*, st.channel_name, st.channel_id, st.thread_ts, st.source_type, st.permalink,
			       st.title AS thread_title, st.last_slack_activity_at,
			       (
			         SELECT COALESCE(
			           NULLIF(CASE WHEN LENGTH(TRIM(COALESCE(su.display_name, ''))) > 1 THEN su.display_name ELSE '' END, ''),
			           NULLIF(su.real_name, ''),
			           NULLIF(sm.user_name, ''),
			           sm.user_id
			         )
			         FROM slack_messages sm
			         LEFT JOIN slack_users su ON su.user_id = sm.user_id
			         WHERE sm.slack_thread_id = st.id
			         ORDER BY sm.slack_message_ts
			         LIMIT 1
			       ) AS root_user_name,
			       (
			         SELECT sm.user_id
			         FROM slack_messages sm
			         WHERE sm.slack_thread_id = st.id
			         ORDER BY sm.slack_message_ts
			         LIMIT 1
			       ) AS root_user_id,
			       (
			         SELECT sm.text
			         FROM slack_messages sm
			         WHERE sm.slack_thread_id = st.id
			         ORDER BY sm.slack_message_ts
			         LIMIT 1
			       ) AS root_text
			FROM jobs j
			JOIN slack_threads st ON st.id = j.slack_thread_id
			ORDER BY j.updated_at DESC
		LIMIT 50`)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	events, err := s.queryMaps(`
		SELECT je.*, j.title AS job_title
		FROM job_events je
		JOIN jobs j ON j.id = je.job_id
		ORDER BY je.created_at DESC
		LIMIT 100`)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	health, err := s.queryMaps(`
		SELECT hc.*
		FROM health_checks hc
		JOIN (
			SELECT service, MAX(id) AS max_id FROM health_checks GROUP BY service
		) latest ON latest.max_id = hc.id
		ORDER BY hc.service`)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	totalThreads := 0
	_ = s.db.QueryRow("SELECT COUNT(*) FROM slack_threads").Scan(&totalThreads)
	totalIntake := 0
	_ = s.db.QueryRow(`
		SELECT COUNT(*)
		FROM slack_threads st
			WHERE st.status IN ('collected', 'analysis_queued', 'analyzing')`).Scan(&totalIntake)
	totalArchive := 0
	_ = s.db.QueryRow("SELECT COUNT(*) FROM slack_threads WHERE status IN ('no_action', 'archived')").Scan(&totalArchive)
	return DashboardSnapshot{
		ReplyDrafts: replyDrafts,
		Blocked:     blocked,
		Queued:      queued,
		Active:      []map[string]any{},
		Intake:      intake,
		Archive:     archive,
		Threads:     threads,
		Jobs:        jobs,
		Events:      events,
		Health:      health,
		Counts: map[string]int{
			"drafts":  len(replyDrafts),
			"blocked": len(blocked),
			"queued":  len(queued),
			"active":  0,
			"intake":  totalIntake,
			"archive": totalArchive,
			"threads": totalThreads,
		},
	}, nil
}

func (s *Store) queryMaps(query string, args ...any) ([]map[string]any, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	result := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(columns))
		for i, column := range columns {
			switch value := values[i].(type) {
			case []byte:
				row[column] = string(value)
			default:
				row[column] = value
			}
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func (s *Store) jobsByIDs(ids []int64) ([]map[string]any, error) {
	if len(ids) == 0 {
		return []map[string]any{}, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	return s.queryMaps(`
		SELECT j.*, st.channel_name, st.channel_id, st.source_type, st.permalink,
		       st.thread_ts, st.title AS thread_title, st.last_slack_activity_at,
		       (
		         SELECT COALESCE(
		           NULLIF(CASE WHEN LENGTH(TRIM(COALESCE(su.display_name, ''))) > 1 THEN su.display_name ELSE '' END, ''),
		           NULLIF(su.real_name, ''),
		           NULLIF(sm.user_name, ''),
		           sm.user_id
		         )
		         FROM slack_messages sm
		         LEFT JOIN slack_users su ON su.user_id = sm.user_id
		         WHERE sm.slack_thread_id = st.id
		         ORDER BY sm.slack_message_ts
		         LIMIT 1
		       ) AS root_user_name,
		       (
		         SELECT sm.user_id
		         FROM slack_messages sm
		         WHERE sm.slack_thread_id = st.id
		         ORDER BY sm.slack_message_ts
		         LIMIT 1
		       ) AS root_user_id,
		       (
		         SELECT sm.text
		         FROM slack_messages sm
		         WHERE sm.slack_thread_id = st.id
		         ORDER BY sm.slack_message_ts
		         LIMIT 1
		       ) AS root_text
		FROM jobs j
		JOIN slack_threads st ON st.id = j.slack_thread_id
		WHERE j.id IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY j.updated_at DESC`, args...)
}

func (s *Store) exec(query string, args ...any) error {
	_, err := s.db.Exec(query, args...)
	return err
}

func (s *Store) metadata(key string) string {
	var value string
	if err := s.db.QueryRow("SELECT value FROM metadata WHERE key=?", key).Scan(&value); err != nil {
		return ""
	}
	return value
}

func (s *Store) setMetadata(key, value string) error {
	_, err := s.db.Exec(
		"INSERT INTO metadata(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
		key,
		value,
	)
	return err
}

func (s *Store) recordHealth(service, status, detail string) HealthCheck {
	check := HealthCheck{Service: service, Status: status, Detail: detail, CheckedAt: utcNow()}
	_ = s.exec(
		"INSERT INTO health_checks(service, status, detail, checked_at) VALUES (?, ?, ?, ?)",
		check.Service,
		check.Status,
		check.Detail,
		check.CheckedAt,
	)
	return check
}

func utcNow() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func mustJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(data)
}
