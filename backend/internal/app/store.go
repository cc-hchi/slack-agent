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

const intakeTriggerSelect = `
		       COALESCE(
		         (
		           SELECT COALESCE(
		             NULLIF(CASE WHEN LENGTH(TRIM(COALESCE(su.display_name, ''))) > 1 THEN su.display_name ELSE '' END, ''),
		             NULLIF(su.real_name, '')
		           )
		           FROM slack_users su
		           WHERE su.user_id = st.trigger_user_id
		         ),
		         st.trigger_user_id
		       ) AS trigger_display_name,
		       COALESCE(
		         (
		           SELECT COALESCE(
		             NULLIF(CASE WHEN LENGTH(TRIM(COALESCE(su.display_name, ''))) > 1 THEN su.display_name ELSE '' END, ''),
		             NULLIF(su.real_name, '')
		           )
		           FROM slack_users su
		           WHERE su.user_id = st.trigger_user_id
		         ),
		         st.trigger_user_id
		       ) AS root_display_name,
		       st.trigger_user_id,
		       st.trigger_user_id AS root_user_id,
		       st.trigger_text,
		       st.trigger_text AS root_text`

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
	if err := s.resetLegacySchemaIfNeeded(); err != nil {
		return err
	}
	if _, err := s.db.Exec(schemaSQL); err != nil {
		return err
	}
	_, err := s.db.Exec(
		"INSERT OR REPLACE INTO metadata(key, value) VALUES (?, ?)",
		"schema_version",
		"6",
	)
	return err
}

func (s *Store) resetLegacySchemaIfNeeded() error {
	legacy := false
	if exists, err := s.tableExists("slack_threads"); err != nil {
		return err
	} else if exists {
		legacy = true
	}
	if exists, err := s.tableExists("intake_items"); err != nil {
		return err
	} else if exists {
		hasTriggerTS, err := s.columnExists("intake_items", "trigger_ts")
		if err != nil {
			return err
		}
		if !hasTriggerTS {
			legacy = true
		}
	}
	for _, table := range []string{"analysis_runs", "jobs", "reply_drafts"} {
		exists, err := s.tableExists(table)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		hasColumn, err := s.columnExists(table, "intake_item_id")
		if err != nil {
			return err
		}
		if !hasColumn {
			legacy = true
			break
		}
	}
	if !legacy {
		return nil
	}
	_, err := s.db.Exec(`
		PRAGMA foreign_keys = OFF;
		DROP TABLE IF EXISTS reply_drafts;
		DROP TABLE IF EXISTS worker_artifacts;
		DROP TABLE IF EXISTS worker_sessions;
		DROP TABLE IF EXISTS job_events;
		DROP TABLE IF EXISTS jobs;
		DROP TABLE IF EXISTS analysis_runs;
		DROP TABLE IF EXISTS slack_messages;
		DROP TABLE IF EXISTS slack_threads;
		DROP TABLE IF EXISTS intake_items;
		DROP TABLE IF EXISTS slack_users;
		DROP TABLE IF EXISTS health_checks;
		DROP TABLE IF EXISTS metadata;
		PRAGMA foreign_keys = ON;`)
	return err
}

func (s *Store) columnExists(table, column string) (bool, error) {
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) tableExists(table string) (bool, error) {
	var count int
	err := s.db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?",
		table,
	).Scan(&count)
	return count > 0, err
}

func (s *Store) SeedDemoData() error {
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM intake_items").Scan(&count); err != nil {
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
		Resolution  string
		Title       string
	}{
		{"C-BACKEND", "#backend", "1764168480.000100", "mention", "resolved", "job_created", "API error on export"},
		{"C-PAYMENTS", "#payments", "1764168120.000200", "user_participated", "resolved", "job_created", "Payment webhook failing"},
		{"D-PRIYA", "DM with Priya Shah", "1764167600.000300", "dm", "resolved", "job_created", "S3 upload error on large files"},
	}
	for _, thread := range threads {
		triggerText := thread.Title + ": can you take a look?"
		if _, err := tx.Exec(`
			INSERT INTO intake_items(
				slack_team_id, channel_id, channel_name, trigger_ts, thread_ts,
				source_type, status, resolution, title, permalink, trigger_user_id, trigger_text,
				latest_slack_message_ts, latest_user_id, latest_user_name,
				latest_text, last_synced_at, raw_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"T-DEMO",
			thread.ChannelID,
			thread.ChannelName,
			thread.ThreadTS,
			thread.ThreadTS,
			thread.SourceType,
			thread.Status,
			thread.Resolution,
			thread.Title,
			fmt.Sprintf("https://slack.example.local/archives/%s/p%s", thread.ChannelID, strings.ReplaceAll(thread.ThreadTS, ".", "")),
			"U-DEMO",
			triggerText,
			thread.ThreadTS,
			"U-DEMO",
			"Ava K.",
			triggerText,
			now,
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
				intake_item_id, codex_thread_id, status, action_required, confidence,
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
				intake_item_id, analysis_run_id, title, status, urgency, task_type,
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
			job_id, intake_item_id, status, draft_text, edited_text, rationale,
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
		       st.trigger_ts, st.thread_ts, st.permalink, st.latest_slack_message_ts,
		       ar.confidence, ar.summary AS analyzer_summary, ar.rationale AS analyzer_rationale,
` + intakeTriggerSelect + `
		FROM reply_drafts rd
		LEFT JOIN jobs j ON j.id = rd.job_id
		LEFT JOIN intake_items st ON st.id = rd.intake_item_id
		LEFT JOIN analysis_runs ar ON ar.id = j.analysis_run_id
		WHERE rd.status IN ('draft', 'edited', 'send_failed')
		ORDER BY rd.updated_at DESC`)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	blocked, err := s.queryMaps(`
		SELECT 'job' AS item_type, j.*, st.channel_name, st.channel_id, st.source_type,
		       st.trigger_ts, st.thread_ts, st.permalink, st.title AS thread_title, st.latest_slack_message_ts,
` + intakeTriggerSelect + `
		FROM jobs j
		JOIN intake_items st ON st.id = j.intake_item_id
		WHERE j.status IN ('blocked', 'failed')
		ORDER BY j.updated_at DESC`)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	queued, err := s.queryMaps(`
			SELECT j.*, st.channel_name, st.channel_id, st.trigger_ts, st.thread_ts, st.source_type, st.permalink,
			       st.title AS thread_title, st.latest_slack_message_ts,
` + intakeTriggerSelect + `
			FROM jobs j
			JOIN intake_items st ON st.id = j.intake_item_id
			WHERE j.status = 'queued'
			ORDER BY j.updated_at DESC`)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	intake, err := s.queryMaps(`
		SELECT st.*, ar.confidence, ar.summary AS analyzer_summary,
` + intakeTriggerSelect + `
			FROM intake_items st
			LEFT JOIN analysis_runs ar ON ar.id = (
				SELECT id FROM analysis_runs
				WHERE intake_item_id = st.id
				ORDER BY id DESC
				LIMIT 1
			)
			WHERE st.status = 'pending'
			ORDER BY COALESCE(st.latest_slack_message_ts, st.trigger_ts) DESC
			LIMIT 80`)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	archive, err := s.queryMaps(`
		SELECT st.*, ar.confidence, ar.summary AS analyzer_summary,
` + intakeTriggerSelect + `
			FROM intake_items st
			LEFT JOIN analysis_runs ar ON ar.id = (
				SELECT id FROM analysis_runs
				WHERE intake_item_id = st.id
				ORDER BY id DESC
				LIMIT 1
			)
		WHERE st.status = 'resolved'
		  AND COALESCE(st.resolution, '') != 'job_created'
		ORDER BY COALESCE(st.latest_slack_message_ts, st.trigger_ts) DESC
		LIMIT 80`)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	threads, err := s.queryMaps(`
		SELECT st.*, ar.confidence, ar.summary AS analyzer_summary,
` + intakeTriggerSelect + `
			FROM intake_items st
			LEFT JOIN analysis_runs ar ON ar.id = (
				SELECT id FROM analysis_runs
				WHERE intake_item_id = st.id
				ORDER BY id DESC
				LIMIT 1
			)
		ORDER BY COALESCE(st.latest_slack_message_ts, st.trigger_ts) DESC
		LIMIT 50`)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	jobs, err := s.queryMaps(`
			SELECT j.*, st.channel_name, st.channel_id, st.trigger_ts, st.thread_ts, st.source_type, st.permalink,
			       st.title AS thread_title, st.latest_slack_message_ts,
` + intakeTriggerSelect + `
			FROM jobs j
			JOIN intake_items st ON st.id = j.intake_item_id
			ORDER BY j.updated_at DESC
		LIMIT 50`)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	events, err := s.queryMaps(`
		SELECT je.id, je.job_id, je.event_type, je.message, je.created_at, j.title AS job_title
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
	_ = s.db.QueryRow("SELECT COUNT(*) FROM intake_items").Scan(&totalThreads)
	totalIntake := 0
	_ = s.db.QueryRow(`
		SELECT COUNT(*)
		FROM intake_items st
			WHERE st.status = 'pending'`).Scan(&totalIntake)
	totalArchive := 0
	_ = s.db.QueryRow("SELECT COUNT(*) FROM intake_items WHERE status = 'resolved' AND COALESCE(resolution, '') != 'job_created'").Scan(&totalArchive)
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
		       st.trigger_ts, st.thread_ts, st.title AS thread_title, st.latest_slack_message_ts,
`+intakeTriggerSelect+`
		FROM jobs j
		JOIN intake_items st ON st.id = j.intake_item_id
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
