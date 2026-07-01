package app

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Service struct {
	cfg             Config
	store           *Store
	slackSyncMu     sync.Mutex
	analyses        chan int64
	jobs            chan int64
	analysisAdvance chan struct{}
	jobAdvance      chan struct{}
	analysisMu      sync.Mutex
	activeAnalyses  map[int64]struct{}
	jobsMu          sync.Mutex
	queuedJobs      map[int64]struct{}
	workingJobs     map[int64]struct{}
}

type threadCollectResult struct {
	collected bool
	skipped   bool
}

func NewService(cfg Config, store *Store) *Service {
	analyzers := cfg.AnalyzerConcurrency
	if analyzers < 1 {
		analyzers = 1
	}
	workers := cfg.WorkerConcurrency
	if workers < 1 {
		workers = 1
	}
	service := &Service{
		cfg:             cfg,
		store:           store,
		analyses:        make(chan int64, 100),
		jobs:            make(chan int64, 100),
		analysisAdvance: make(chan struct{}, 1),
		jobAdvance:      make(chan struct{}, 1),
		activeAnalyses:  make(map[int64]struct{}),
		queuedJobs:      make(map[int64]struct{}),
		workingJobs:     make(map[int64]struct{}),
	}
	service.recoverInterruptedWorkerSessions()
	service.recoverInterruptedJobs()
	for i := 0; i < analyzers; i++ {
		go service.analyzerLoop()
	}
	for i := 0; i < workers; i++ {
		go service.workerLoop()
	}
	go service.analysisSchedulerLoop()
	go service.jobSchedulerLoop()
	go service.slackSyncSchedulerLoop()
	service.signalAutoAdvance()
	return service
}

func (s *Service) Health(ctx context.Context) []HealthCheck {
	var checks []HealthCheck
	if err := s.store.db.PingContext(ctx); err != nil {
		checks = append(checks, s.store.recordHealth("sqlite", "unhealthy", err.Error()))
	} else {
		checks = append(checks, s.store.recordHealth("sqlite", "healthy", s.cfg.DatabasePath))
	}

	if s.cfg.SlackToken() == "" {
		checks = append(checks, s.store.recordHealth("slack", "missing_config", "Set SLACK_USER_TOKEN or SLACK_BOT_TOKEN"))
	} else if auth, err := NewSlackClient(s.cfg).AuthTest(); err != nil {
		checks = append(checks, s.store.recordHealth("slack", "unhealthy", err.Error()))
	} else {
		checks = append(checks, s.store.recordHealth("slack", "healthy", fmt.Sprintf("%v / %v", auth["team"], auth["user_id"])))
	}

	if bin := s.cfg.ResolvedCodexBin(); bin == "" {
		checks = append(checks, s.store.recordHealth("codex_app_server", "unhealthy", "Codex binary was not found"))
	} else if info, err := s.probeCodex(bin); err != nil {
		checks = append(checks, s.store.recordHealth("codex_app_server", "unhealthy", err.Error()))
	} else {
		checks = append(checks, s.store.recordHealth("codex_app_server", "healthy", fmt.Sprint(info["userAgent"])))
	}

	if command := s.cfg.ResolvedBootstrapCommand(); command == "" {
		checks = append(checks, s.store.recordHealth("bootstrap_command", "missing_config", s.cfg.WorkspaceBootstrapCommand))
	} else {
		checks = append(checks, s.store.recordHealth("bootstrap_command", "healthy", command))
	}

	if err := os.MkdirAll(s.cfg.WorkspaceRoot, 0o755); err != nil {
		checks = append(checks, s.store.recordHealth("workspace_root", "unhealthy", err.Error()))
	} else {
		checks = append(checks, s.store.recordHealth("workspace_root", "healthy", s.cfg.WorkspaceRoot))
	}
	return checks
}

func (s *Service) DashboardSnapshot() (DashboardSnapshot, error) {
	snapshot, err := s.store.DashboardSnapshot()
	if err != nil {
		return DashboardSnapshot{}, err
	}
	workingIDs := s.workingJobIDs()
	workingIDSet := make(map[int64]struct{}, len(workingIDs))
	for _, id := range workingIDs {
		workingIDSet[id] = struct{}{}
	}
	if len(workingIDSet) > 0 {
		filteredQueued := snapshot.Queued[:0]
		for _, job := range snapshot.Queued {
			jobID, ok := int64Value(job["id"])
			if !ok {
				filteredQueued = append(filteredQueued, job)
				continue
			}
			if _, working := workingIDSet[jobID]; !working {
				filteredQueued = append(filteredQueued, job)
			}
		}
		snapshot.Queued = filteredQueued
	}
	active, err := s.store.jobsByIDs(workingIDs)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	for _, job := range active {
		job["status"] = "working"
	}
	snapshot.Active = active
	if snapshot.Counts == nil {
		snapshot.Counts = map[string]int{}
	}
	snapshot.Counts["queued"] = len(snapshot.Queued)
	snapshot.Counts["active"] = len(snapshot.Active)
	return snapshot, nil
}

func (s *Service) workingJobIDs() []int64 {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	ids := make([]int64, 0, len(s.workingJobs))
	for id := range s.workingJobs {
		ids = append(ids, id)
	}
	return ids
}

func (s *Service) probeCodex(bin string) (map[string]any, error) {
	client, err := NewCodexClient(bin)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	return client.Initialize()
}

func (s *Service) SyncSlack() (map[string]any, error) {
	if !s.slackSyncMu.TryLock() {
		return map[string]any{
			"collected":    0,
			"skipped":      0,
			"errors":       []string{},
			"reason":       "sync_already_running",
			"sync_skipped": true,
			"synced_at":    utcNow(),
		}, nil
	}
	defer s.slackSyncMu.Unlock()
	_ = s.store.setMetadata("slack_sync.last_started_at", utcNow())

	client := NewSlackClient(s.cfg)
	auth, err := client.AuthTest()
	if err != nil {
		s.recordSlackSyncResult(nil, err)
		return nil, err
	}
	userID := s.cfg.SlackUserID
	if userID == "" {
		userID, _ = auth["user_id"].(string)
	}
	teamID, _ := auth["team_id"].(string)
	if teamID == "" {
		teamID = "unknown-team"
	}
	collected := 0
	skipped := 0
	errors := []string{}
	seenIntakes := map[string]struct{}{}
	defaultCursorNow := time.Now()
	queries := []struct {
		Query  string
		Source string
	}{
		{fmt.Sprintf("<@%s>", userID), "mention"},
	}
	if s.cfg.SlackIncludeParticipated {
		userSearchName := s.cfg.SlackUserSearchName
		if userSearchName == "" {
			userSearchName, _ = auth["user"].(string)
		}
		if userSearchName != "" {
			queries = append(queries, struct {
				Query  string
				Source string
			}{fmt.Sprintf("from:@%s", userSearchName), "user_participated"})
		}
	}
	for _, query := range queries {
		cursorKey := fmt.Sprintf("slack_sync.search.%s.latest_ts", query.Source)
		storedCursor := s.store.metadata(cursorKey)
		lastSeenTS := slackCursorOrNow(storedCursor, defaultCursorNow)
		searchQuery := query.Query
		if after := slackSearchAfterDate(lastSeenTS, s.cfg.SlackSearchLookbackDays); after != "" {
			searchQuery += " after:" + after
		}
		maxSeenTS := lastSeenTS
		searchChecked := false
		pageSize := positiveInt(s.cfg.SlackSearchPageSize, 100)
		for page := 1; shouldFetchPage(page, s.cfg.SlackSearchMaxPages); page++ {
			matches, pageInfo, err := client.SearchMessages(searchQuery, pageSize, page)
			if err != nil {
				errors = append(errors, err.Error())
				break
			}
			searchChecked = true
			if len(matches) == 0 {
				break
			}
			for _, match := range matches {
				if ts := slackMessageTS(match); slackTSAfter(ts, maxSeenTS) {
					maxSeenTS = ts
				}
				result, err := s.collectThreadFromMessage(client, teamID, "", "", query.Source, match, userID, seenIntakes)
				if err != nil {
					errors = append(errors, err.Error())
					continue
				}
				if result.collected {
					collected++
				}
				if result.skipped {
					skipped++
				}
			}
			if pageInfo.PageCount > 0 && page >= pageInfo.PageCount {
				break
			}
			if pageInfo.PageCount == 0 && len(matches) < pageSize {
				break
			}
		}
		if searchChecked && (storedCursor == "" || slackTSAfter(maxSeenTS, storedCursor)) {
			if err := s.store.setMetadata(cursorKey, maxSeenTS); err != nil {
				errors = append(errors, "search cursor "+query.Source+": "+err.Error())
			}
		}
	}
	ims, err := client.ConversationsList("im")
	if err != nil {
		errors = append(errors, "dm sync: "+err.Error())
	} else {
		for index, im := range ims {
			if s.cfg.SlackDMChannelLimit > 0 && index >= s.cfg.SlackDMChannelLimit {
				break
			}
			channelID, _ := im["id"].(string)
			if channelID == "" {
				continue
			}
			cursorKey := fmt.Sprintf("slack_sync.dm.%s.latest_ts", channelID)
			storedCursor := s.store.metadata(cursorKey)
			lastSeenTS := slackCursorOrNow(storedCursor, defaultCursorNow)
			oldest := slackOldestFromCursor(lastSeenTS, s.cfg.SlackSearchLookbackDays)
			history, err := client.ConversationsHistory(
				channelID,
				positiveInt(s.cfg.SlackDMHistoryPageSize, 100),
				s.cfg.SlackDMHistoryMaxPages,
				oldest,
			)
			if err != nil {
				errors = append(errors, "dm history "+channelID+": "+err.Error())
				continue
			}
			maxSeenTS := lastSeenTS
			for _, message := range history {
				if ts := slackMessageTS(message); slackTSAfter(ts, maxSeenTS) {
					maxSeenTS = ts
				}
				if msgUser, _ := message["user"].(string); msgUser == userID {
					continue
				}
				result, err := s.collectThreadFromMessage(client, teamID, channelID, "DM", "dm", message, userID, seenIntakes)
				if err != nil {
					errors = append(errors, err.Error())
					continue
				}
				if result.collected {
					collected++
				}
				if result.skipped {
					skipped++
				}
			}
			if storedCursor == "" || slackTSAfter(maxSeenTS, storedCursor) {
				if err := s.store.setMetadata(cursorKey, maxSeenTS); err != nil {
					errors = append(errors, "dm cursor "+channelID+": "+err.Error())
				}
			}
		}
	}
	errors = append(errors, s.resolveSlackUsers(client, 120)...)
	s.signalAutoAdvance()
	result := map[string]any{
		"collected": collected,
		"skipped":   skipped,
		"errors":    errors,
		"synced_at": utcNow(),
	}
	s.recordSlackSyncResult(result, nil)
	return result, nil
}

func (s *Service) slackSyncSchedulerLoop() {
	initialDelay := 5 * time.Second
	timer := time.NewTimer(initialDelay)
	defer timer.Stop()
	for {
		<-timer.C
		_, _ = s.SyncSlack()
		timer.Reset(s.slackSyncInterval())
	}
}

func (s *Service) slackSyncInterval() time.Duration {
	seconds := s.cfg.SlackSyncIntervalSeconds
	if seconds <= 0 {
		seconds = 300
	}
	return time.Duration(seconds) * time.Second
}

func (s *Service) recordSlackSyncResult(result map[string]any, syncErr error) {
	finishedAt := utcNow()
	_ = s.store.setMetadata("slack_sync.last_finished_at", finishedAt)
	if syncErr != nil {
		_ = s.store.setMetadata("slack_sync.last_error", syncErr.Error())
		return
	}
	if result == nil {
		return
	}
	_ = s.store.setMetadata("slack_sync.last_result", mustJSON(result))
	if errors, ok := result["errors"].([]string); ok && len(errors) > 0 {
		_ = s.store.setMetadata("slack_sync.last_error", strings.Join(errors, "; "))
		return
	}
	_ = s.store.setMetadata("slack_sync.last_error", "")
}

func (s *Service) collectThreadFromMessage(client *SlackClient, teamID, fallbackChannelID, fallbackChannelName, sourceType string, match map[string]any, userID string, seen map[string]struct{}) (threadCollectResult, error) {
	channelID, channelName := slackChannel(match)
	if channelID == "" {
		channelID = fallbackChannelID
	}
	if channelName == "" {
		channelName = fallbackChannelName
	}
	threadTS := slackThreadTS(match)
	triggerTS := slackMessageTS(match)
	if triggerTS == "" {
		triggerTS = threadTS
	}
	if channelID == "" || threadTS == "" || triggerTS == "" {
		return threadCollectResult{skipped: true}, nil
	}
	key := slackThreadKey(teamID, channelID, triggerTS)
	if _, exists := seen[key]; exists {
		return threadCollectResult{skipped: true}, nil
	}
	seen[key] = struct{}{}

	if latestTS, ok := candidateThreadLatestTS(match); ok && s.intakeKnownUpToDate(teamID, channelID, triggerTS, latestTS) {
		return threadCollectResult{skipped: true}, nil
	}

	messages, err := client.ConversationsReplies(
		channelID,
		threadTS,
		positiveInt(s.cfg.SlackReplyPageSize, 200),
		s.cfg.SlackReplyMaxPages,
	)
	if err != nil || len(messages) == 0 {
		messages = []map[string]any{match}
	}
	if firstString(match["permalink"]) == "" {
		if permalink, err := client.ChatGetPermalink(channelID, triggerTS); err == nil {
			match["permalink"] = permalink
		}
	}
	if err := s.upsertIntakeItem(teamID, channelID, channelName, triggerTS, threadTS, sourceType, match, messages, userID); err != nil {
		return threadCollectResult{}, err
	}
	return threadCollectResult{collected: true}, nil
}

func (s *Service) intakeKnownUpToDate(teamID, channelID, triggerTS, latestTS string) bool {
	if latestTS == "" {
		return false
	}
	var storedLatest sql.NullString
	err := s.store.db.QueryRow(`
		SELECT latest_slack_message_ts
		FROM intake_items
		WHERE slack_team_id=? AND channel_id=? AND trigger_ts=?`,
		teamID,
		channelID,
		triggerTS,
	).Scan(&storedLatest)
	if err != nil || !storedLatest.Valid || storedLatest.String == "" {
		return false
	}
	return !slackTSAfter(latestTS, storedLatest.String)
}

func (s *Service) resolveSlackUsers(client *SlackClient, limit int) []string {
	rows, err := s.store.db.Query(`
		SELECT DISTINCT ids.user_id
		FROM (
			SELECT trigger_user_id AS user_id FROM intake_items
			UNION
			SELECT latest_user_id AS user_id FROM intake_items
		) ids
		LEFT JOIN slack_users su ON su.user_id = ids.user_id
		WHERE ids.user_id IS NOT NULL
		  AND ids.user_id != ''
		  AND (su.user_id IS NULL OR COALESCE(su.display_name, '') = '')
		ORDER BY ids.user_id
		LIMIT ?`, limit)
	if err != nil {
		return []string{"user cache query: " + err.Error()}
	}
	defer rows.Close()

	var errors []string
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			errors = append(errors, "user cache scan: "+err.Error())
			continue
		}
		payload, err := client.UsersInfo(userID)
		if err != nil {
			errors = append(errors, "users.info "+userID+": "+err.Error())
			continue
		}
		user, _ := payload["user"].(map[string]any)
		displayName, realName := slackDisplayNames(user)
		teamID, _ := user["team_id"].(string)
		isBot := boolInt(user["is_bot"])
		deleted := boolInt(user["deleted"])
		if displayName == "" {
			displayName = userID
		}
		_, err = s.store.db.Exec(`
			INSERT INTO slack_users(
				user_id, display_name, real_name, team_id, is_bot, deleted, raw_json, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(user_id) DO UPDATE SET
				display_name=excluded.display_name,
				real_name=excluded.real_name,
				team_id=excluded.team_id,
				is_bot=excluded.is_bot,
				deleted=excluded.deleted,
				raw_json=excluded.raw_json,
				updated_at=excluded.updated_at`,
			userID,
			displayName,
			realName,
			teamID,
			isBot,
			deleted,
			mustJSON(user),
			utcNow(),
		)
		if err != nil {
			errors = append(errors, "user cache upsert "+userID+": "+err.Error())
			continue
		}
		_, _ = s.store.db.Exec(`
			UPDATE intake_items
			SET latest_user_name = CASE
			      WHEN latest_user_id = ? AND (COALESCE(latest_user_name, '') = '' OR latest_user_name = latest_user_id) THEN ?
			      ELSE latest_user_name
			    END
			WHERE trigger_user_id = ? OR latest_user_id = ?`,
			userID,
			displayName,
			userID,
			userID,
		)
	}
	if err := rows.Err(); err != nil {
		errors = append(errors, "user cache rows: "+err.Error())
	}
	return errors
}

func slackDisplayNames(user map[string]any) (string, string) {
	profile, _ := user["profile"].(map[string]any)
	displayName := firstString(
		profile["display_name_normalized"],
		profile["display_name"],
		profile["real_name_normalized"],
		profile["real_name"],
		user["real_name"],
		user["name"],
	)
	realName := firstString(
		profile["real_name_normalized"],
		profile["real_name"],
		user["real_name"],
		user["name"],
	)
	return displayName, realName
}

func firstString(values ...any) string {
	for _, value := range values {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

func boolInt(value any) int {
	if boolValue, ok := value.(bool); ok && boolValue {
		return 1
	}
	return 0
}

func positiveInt(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

func shouldFetchPage(page, maxPages int) bool {
	return maxPages <= 0 || page <= maxPages
}

func slackChannel(message map[string]any) (string, string) {
	channel, _ := message["channel"].(map[string]any)
	channelID, _ := channel["id"].(string)
	channelName, _ := channel["name"].(string)
	if channelID == "" {
		channelID, _ = message["channel"].(string)
	}
	if channelName == "" {
		channelName, _ = message["channel_name"].(string)
	}
	return channelID, channelName
}

func slackThreadTS(message map[string]any) string {
	if ts, _ := message["thread_ts"].(string); ts != "" {
		return ts
	}
	return slackMessageTS(message)
}

func slackMessageTS(message map[string]any) string {
	ts, _ := message["ts"].(string)
	return ts
}

func slackThreadKey(teamID, channelID, threadTS string) string {
	return teamID + "|" + channelID + "|" + threadTS
}

func candidateThreadLatestTS(message map[string]any) (string, bool) {
	for _, key := range []string{"latest_reply", "latest"} {
		if ts, _ := message[key].(string); ts != "" {
			return ts, true
		}
	}
	ts := slackMessageTS(message)
	threadTS, _ := message["thread_ts"].(string)
	if threadTS != "" && threadTS != ts {
		return "", false
	}
	if numberAsInt(message["reply_count"]) > 0 {
		return "", false
	}
	return ts, ts != ""
}

func latestSlackMessage(messages []map[string]any) map[string]any {
	var latest map[string]any
	latestTS := ""
	for _, message := range messages {
		if ts := slackMessageTS(message); slackTSAfter(ts, latestTS) {
			latestTS = ts
			latest = message
		}
	}
	return latest
}

func slackMessageUserName(message map[string]any) string {
	return firstString(message["username"], message["user_name"])
}

func slackMessageText(message map[string]any) string {
	return firstString(message["text"])
}

func latestSlackTSFromThread(thread map[string]any) string {
	if ts, ok := thread["latest_slack_message_ts"].(string); ok && ts != "" {
		return ts
	}
	if ts, ok := thread["thread_ts"].(string); ok {
		return ts
	}
	return ""
}

func shouldRecollectThread(status string) bool {
	switch status {
	case "pending":
		return false
	default:
		return true
	}
}

func slackTSAfter(left, right string) bool {
	if left == "" {
		return false
	}
	if right == "" {
		return true
	}
	leftTime, leftOK := parseSlackTimestamp(left)
	rightTime, rightOK := parseSlackTimestamp(right)
	if leftOK && rightOK {
		return leftTime.After(rightTime)
	}
	return left > right
}

func slackSearchAfterDate(cursor string, lookbackDays int) string {
	if cursor == "" {
		return ""
	}
	parsed, ok := parseSlackTimestamp(cursor)
	if !ok {
		return ""
	}
	if lookbackDays > 0 {
		parsed = parsed.AddDate(0, 0, -lookbackDays)
	}
	return parsed.UTC().Format("2006-01-02")
}

func slackOldestFromCursor(cursor string, lookbackDays int) string {
	if cursor == "" {
		return ""
	}
	parsed, ok := parseSlackTimestamp(cursor)
	if !ok {
		return ""
	}
	if lookbackDays > 0 {
		parsed = parsed.AddDate(0, 0, -lookbackDays)
	}
	return formatSlackTimestamp(parsed)
}

func slackCursorOrNow(cursor string, now time.Time) string {
	if cursor != "" {
		return cursor
	}
	return formatSlackTimestamp(now)
}

func parseSlackTimestamp(ts string) (time.Time, bool) {
	parts := strings.SplitN(ts, ".", 2)
	seconds, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	micros := int64(0)
	if len(parts) == 2 {
		fraction := parts[1]
		if len(fraction) > 6 {
			fraction = fraction[:6]
		}
		for len(fraction) < 6 {
			fraction += "0"
		}
		micros, err = strconv.ParseInt(fraction, 10, 64)
		if err != nil {
			return time.Time{}, false
		}
	}
	return time.Unix(seconds, micros*1000).UTC(), true
}

func formatSlackTimestamp(value time.Time) string {
	utc := value.UTC()
	return fmt.Sprintf("%d.%06d", utc.Unix(), utc.Nanosecond()/1000)
}

func (s *Service) upsertIntakeItem(teamID, channelID, channelName, triggerTS, threadTS, sourceType string, trigger map[string]any, messages []map[string]any, userID string) error {
	if len(messages) == 0 {
		messages = []map[string]any{trigger}
	}
	latest := latestSlackMessage(messages)
	if latest == nil {
		latest = trigger
	}

	title := slackMessageText(trigger)
	title = strings.TrimSpace(strings.ReplaceAll(title, "\n", " "))
	if len(title) > 90 {
		title = title[:90]
	}
	if title == "" {
		title = "Untitled intake item"
	}
	latestTS := slackMessageTS(latest)
	if latestTS == "" {
		latestTS = triggerTS
	}
	triggerUserID := firstString(trigger["user"])
	latestUserID := firstString(latest["user"])
	now := utcNow()
	tx, err := s.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var existingStatus sql.NullString
	var existingResolution sql.NullString
	var existingLatestTS sql.NullString
	err = tx.QueryRow(`
		SELECT status, resolution, latest_slack_message_ts
		FROM intake_items
		WHERE slack_team_id=? AND channel_id=? AND trigger_ts=?`,
		teamID,
		channelID,
		triggerTS,
	).Scan(&existingStatus, &existingResolution, &existingLatestTS)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	hasExisting := err == nil
	status := "pending"
	var resolution any
	if hasExisting {
		status = existingStatus.String
		if status == "" {
			status = "pending"
		}
		if existingResolution.Valid && existingResolution.String != "" {
			resolution = existingResolution.String
		}
	}
	hasNewSlackActivity := !hasExisting || !existingLatestTS.Valid || existingLatestTS.String == "" || slackTSAfter(latestTS, existingLatestTS.String)
	if hasExisting && hasNewSlackActivity && shouldRecollectThread(status) {
		status = "pending"
		resolution = nil
	}
	if hasExisting && existingLatestTS.Valid && existingLatestTS.String != "" && !slackTSAfter(latestTS, existingLatestTS.String) {
		latestTS = existingLatestTS.String
	}

	if _, err := tx.Exec(`
		INSERT INTO intake_items(
			slack_team_id, channel_id, channel_name, trigger_ts, thread_ts, source_type,
			status, resolution, title, permalink, trigger_user_id, trigger_text,
			latest_slack_message_ts, latest_user_id, latest_user_name,
			latest_text, last_synced_at, raw_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(slack_team_id, channel_id, trigger_ts) DO UPDATE SET
			channel_name=excluded.channel_name,
			thread_ts=excluded.thread_ts,
			source_type=excluded.source_type,
			status=excluded.status,
			resolution=excluded.resolution,
			title=excluded.title,
			permalink=COALESCE(NULLIF(excluded.permalink, ''), intake_items.permalink),
			trigger_user_id=COALESCE(NULLIF(excluded.trigger_user_id, ''), intake_items.trigger_user_id),
			trigger_text=COALESCE(NULLIF(excluded.trigger_text, ''), intake_items.trigger_text),
			latest_slack_message_ts=excluded.latest_slack_message_ts,
			latest_user_id=CASE
				WHEN excluded.latest_slack_message_ts = intake_items.latest_slack_message_ts THEN COALESCE(NULLIF(intake_items.latest_user_id, ''), NULLIF(excluded.latest_user_id, ''))
				ELSE COALESCE(NULLIF(excluded.latest_user_id, ''), intake_items.latest_user_id)
			END,
			latest_user_name=CASE
				WHEN excluded.latest_slack_message_ts = intake_items.latest_slack_message_ts THEN COALESCE(NULLIF(intake_items.latest_user_name, ''), NULLIF(excluded.latest_user_name, ''))
				ELSE COALESCE(NULLIF(excluded.latest_user_name, ''), intake_items.latest_user_name)
			END,
			latest_text=CASE
				WHEN excluded.latest_slack_message_ts = intake_items.latest_slack_message_ts THEN COALESCE(NULLIF(intake_items.latest_text, ''), NULLIF(excluded.latest_text, ''))
				ELSE COALESCE(NULLIF(excluded.latest_text, ''), intake_items.latest_text)
			END,
			last_synced_at=excluded.last_synced_at,
			raw_json=excluded.raw_json`,
		teamID,
		channelID,
		channelName,
		triggerTS,
		threadTS,
		sourceType,
		status,
		resolution,
		title,
		trigger["permalink"],
		triggerUserID,
		slackMessageText(trigger),
		latestTS,
		latestUserID,
		slackMessageUserName(latest),
		slackMessageText(latest),
		now,
		mustJSON(trigger),
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) QueueAnalysis(threadID int64) (map[string]any, error) {
	var currentStatus string
	if err := s.store.db.QueryRow("SELECT status FROM intake_items WHERE id=?", threadID).Scan(&currentStatus); err != nil {
		return nil, err
	}
	if s.isAnalysisActive(threadID) {
		return map[string]any{"queued": false, "intake_item_id": threadID, "status": currentStatus, "reason": "already_analyzing"}, nil
	}
	if currentStatus != "pending" {
		return map[string]any{"queued": false, "intake_item_id": threadID, "status": currentStatus, "reason": "not_analyzable"}, nil
	}
	queued := s.enqueueAnalysis(threadID)
	reason := ""
	if !queued {
		reason = "analysis_queue_full"
	}
	return map[string]any{"queued": queued, "intake_item_id": threadID, "status": currentStatus, "reason": reason}, nil
}

func (s *Service) QueueJob(jobID int64) (map[string]any, error) {
	job, err := s.job(jobID)
	if err != nil {
		return nil, err
	}
	status := fmt.Sprint(job["status"])
	if !canQueueJobStatus(status) {
		return map[string]any{"queued": false, "job_id": jobID, "status": status, "reason": "job_not_runnable"}, nil
	}
	if status != "queued" {
		result, err := s.store.db.Exec(
			`UPDATE jobs
			 SET status='queued', current_block_reason=NULL, next_user_action=NULL, updated_at=?
			 WHERE id=? AND status IN ('blocked', 'failed', 'completed_no_reply')`,
			utcNow(),
			jobID,
		)
		if err != nil {
			return nil, err
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if updated == 0 {
			rows, err := s.store.queryMaps("SELECT status FROM jobs WHERE id=?", jobID)
			if err != nil {
				return nil, err
			}
			if len(rows) == 0 {
				return nil, sql.ErrNoRows
			}
			return map[string]any{"queued": false, "job_id": jobID, "status": rows[0]["status"], "reason": "status_changed"}, nil
		}
	}
	queued := s.enqueueJob(jobID)
	reason := ""
	if !queued {
		reason = "already_queued_or_working_or_queue_full"
	}
	return map[string]any{"queued": queued, "job_id": jobID, "status": "queued", "reason": reason}, nil
}

func canQueueJobStatus(status string) bool {
	switch status {
	case "queued", "blocked", "failed", "completed_no_reply":
		return true
	default:
		return false
	}
}

func (s *Service) analyzerLoop() {
	for threadID := range s.analyses {
		advanced := s.runAnalysis(threadID)
		s.analysisMu.Lock()
		delete(s.activeAnalyses, threadID)
		s.analysisMu.Unlock()
		if advanced {
			s.signalAutoAdvance()
		}
	}
}

func (s *Service) workerLoop() {
	for jobID := range s.jobs {
		advanced := s.runJob(jobID)
		s.jobsMu.Lock()
		delete(s.queuedJobs, jobID)
		delete(s.workingJobs, jobID)
		s.jobsMu.Unlock()
		if advanced {
			s.signalAutoAdvance()
		}
	}
}

func (s *Service) signalAutoAdvance() {
	s.signalAnalysisAdvance()
	s.signalJobAdvance()
}

func (s *Service) signalAnalysisAdvance() {
	select {
	case s.analysisAdvance <- struct{}{}:
	default:
	}
}

func (s *Service) signalJobAdvance() {
	select {
	case s.jobAdvance <- struct{}{}:
	default:
	}
}

func (s *Service) analysisSchedulerLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.analysisAdvance:
			s.queuePendingAnalyses()
		case <-ticker.C:
			s.queuePendingAnalyses()
		}
	}
}

func (s *Service) jobSchedulerLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.jobAdvance:
			s.queuePendingJobs()
		case <-ticker.C:
			s.queuePendingJobs()
		}
	}
}

func (s *Service) advanceIntake() {
	s.queuePendingJobs()
	s.queuePendingAnalyses()
}

func (s *Service) queuePendingAnalyses() {
	rows, err := s.store.db.Query(`
		SELECT st.id
		FROM intake_items st
		WHERE st.status = 'pending'
		ORDER BY COALESCE(st.latest_slack_message_ts, st.trigger_ts) ASC, st.id ASC`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var threadID int64
		if err := rows.Scan(&threadID); err != nil {
			continue
		}
		if s.isAnalysisActive(threadID) {
			continue
		}
		_, _ = s.QueueAnalysis(threadID)
	}
}

func (s *Service) queuePendingJobs() {
	rows, err := s.store.db.Query("SELECT id FROM jobs WHERE status='queued' ORDER BY updated_at ASC, id ASC")
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var jobID int64
		if err := rows.Scan(&jobID); err != nil {
			continue
		}
		if s.isJobWorking(jobID) {
			continue
		}
		_, _ = s.QueueJob(jobID)
	}
}

func (s *Service) enqueueJob(jobID int64) bool {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	if s.jobs == nil {
		s.jobs = make(chan int64, 100)
	}
	if s.queuedJobs == nil {
		s.queuedJobs = make(map[int64]struct{})
	}
	if s.workingJobs == nil {
		s.workingJobs = make(map[int64]struct{})
	}
	if _, exists := s.workingJobs[jobID]; exists {
		return false
	}
	if _, exists := s.queuedJobs[jobID]; exists {
		return false
	}
	select {
	case s.jobs <- jobID:
		s.queuedJobs[jobID] = struct{}{}
		return true
	default:
		return false
	}
}

func (s *Service) isJobWorking(jobID int64) bool {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	_, exists := s.workingJobs[jobID]
	return exists
}

func (s *Service) enqueueAnalysis(threadID int64) bool {
	s.analysisMu.Lock()
	defer s.analysisMu.Unlock()
	if s.analyses == nil {
		s.analyses = make(chan int64, 100)
	}
	if s.activeAnalyses == nil {
		s.activeAnalyses = make(map[int64]struct{})
	}
	if _, exists := s.activeAnalyses[threadID]; exists {
		return false
	}
	select {
	case s.analyses <- threadID:
		s.activeAnalyses[threadID] = struct{}{}
		return true
	default:
		return false
	}
}

func (s *Service) isAnalysisActive(threadID int64) bool {
	s.analysisMu.Lock()
	defer s.analysisMu.Unlock()
	_, exists := s.activeAnalyses[threadID]
	return exists
}

func (s *Service) runAnalysis(threadID int64) bool {
	started := utcNow()
	result, err := s.store.db.Exec("INSERT INTO analysis_runs(intake_item_id, status, started_at) VALUES (?, 'running', ?)", threadID, started)
	if err != nil {
		return false
	}
	analysisID, _ := result.LastInsertId()
	thread, err := s.threadContext(threadID)
	if err != nil {
		s.finishAnalysisError(threadID, analysisID, err)
		return false
	}
	workspace := s.threadWorkspace(threadID)
	if ok := s.ensureAnalyzerWorkspace(threadID, analysisID, workspace); !ok {
		return false
	}
	client, err := NewCodexClient(s.cfg.ResolvedCodexBin())
	if err != nil {
		s.finishAnalysisError(threadID, analysisID, err)
		return false
	}
	defer client.Close()
	if _, err := client.Initialize(); err != nil {
		s.finishAnalysisError(threadID, analysisID, err)
		return false
	}
	codexThread, err := client.StartThread(s.cfg.CodexThreadOptions(workspace))
	if err != nil {
		s.finishAnalysisError(threadID, analysisID, err)
		return false
	}
	turn := client.RunTurn(codexThread.ThreadID, analysisPrompt(thread, s.relatedJobsForThread(threadID, 20)), 10*time.Minute)
	final := turn.FinalText
	analysis := parseAnalysisOutput(final)
	status := "completed"
	if turn.Error != "" {
		status = "failed"
	}
	_, _ = s.store.db.Exec(`
		UPDATE analysis_runs
		SET status=?, action_required=?, confidence=?, summary=?, rationale=?,
			structured_result_json=?, codex_thread_id=?, codex_session_id=?, completed_at=?, error=?
			WHERE id=?`,
		status,
		boolToInt(analysis.ActionRequired),
		analysis.confidenceValue(),
		truncate(analysis.Summary, 800),
		truncate(analysis.Rationale, 800),
		analysis.structuredResultJSON(final, turn.Events),
		codexThread.ThreadID,
		codexThread.SessionID,
		utcNow(),
		nullIfEmpty(turn.Error),
		analysisID,
	)
	if turn.Error != "" {
		return false
	}
	analyzedSlackTS := latestSlackTSFromThread(thread)
	if !analysis.ActionRequired {
		s.resolveIntakeItemIfCurrent(threadID, analyzedSlackTS, "no_action")
		return true
	}
	title := analysis.TaskTitle
	if title == "" {
		title, _ = thread["title"].(string)
	}
	if title == "" {
		title = fmt.Sprintf("Intake item %d", threadID)
	}
	urgency := analysis.Urgency
	if urgency == "" {
		urgency = "medium"
	}
	jobID, err := s.createJobIfIntakeCurrent(threadID, analysisID, analyzedSlackTS, title, urgency, workspace)
	if err != nil {
		s.finishAnalysisError(threadID, analysisID, err)
		return false
	}
	if jobID > 0 {
		_, _ = s.QueueJob(jobID)
	}
	return true
}

func (s *Service) finishAnalysisError(threadID, analysisID int64, err error) {
	_, _ = s.store.db.Exec("UPDATE analysis_runs SET status='failed', completed_at=?, error=? WHERE id=?", utcNow(), err.Error(), analysisID)
}

func (s *Service) resolveIntakeItemIfCurrent(threadID int64, analyzedSlackTS, resolution string) bool {
	result, err := s.store.db.Exec(`
		UPDATE intake_items
		SET status='resolved', resolution=?, last_analyzed_slack_ts=?
		WHERE id=?
		  AND status='pending'
		  AND COALESCE(latest_slack_message_ts, trigger_ts)=?`,
		resolution,
		analyzedSlackTS,
		threadID,
		analyzedSlackTS,
	)
	if err != nil {
		return false
	}
	updated, err := result.RowsAffected()
	return err == nil && updated > 0
}

func (s *Service) createJobIfIntakeCurrent(threadID, analysisID int64, analyzedSlackTS, title, urgency, workspace string) (int64, error) {
	now := utcNow()
	tx, err := s.store.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	result, err := tx.Exec(`
		UPDATE intake_items
		SET status='resolved', resolution='job_created', last_analyzed_slack_ts=?
		WHERE id=?
		  AND status='pending'
		  AND COALESCE(latest_slack_message_ts, trigger_ts)=?`,
		analyzedSlackTS,
		threadID,
		analyzedSlackTS,
	)
	if err != nil {
		return 0, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if updated == 0 {
		return 0, tx.Commit()
	}

	insertResult, err := tx.Exec(`
		INSERT INTO jobs(
			intake_item_id, analysis_run_id, title, status, urgency, task_type,
			workspace_path, bootstrap_status, created_at, updated_at
		) VALUES (?, ?, ?, 'queued', ?, 'slack_task', ?, 'succeeded', ?, ?)`,
		threadID,
		analysisID,
		title,
		urgency,
		workspace,
		now,
		now,
	)
	if err != nil {
		return 0, err
	}
	jobID, err := insertResult.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return jobID, nil
}

func (s *Service) ensureAnalyzerWorkspace(threadID, analysisID int64, workspace string) bool {
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		s.finishAnalysisError(threadID, analysisID, fmt.Errorf("analyzer workspace creation failed: %w", err))
		return false
	}
	bootstrap := s.cfg.ResolvedBootstrapCommand()
	if bootstrap == "" {
		s.finishAnalysisError(threadID, analysisID, fmt.Errorf("analyzer workspace bootstrap command was not found: %s", s.cfg.WorkspaceBootstrapCommand))
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-lc", bootstrap)
	cmd.Dir = workspace
	output, err := cmd.CombinedOutput()
	if err != nil {
		s.finishAnalysisError(threadID, analysisID, fmt.Errorf("analyzer workspace bootstrap failed: %w: %s", err, truncate(string(output), 4000)))
		return false
	}
	return true
}

func (s *Service) threadWorkspace(threadID int64) string {
	return filepath.Join(s.cfg.WorkspaceRoot, fmt.Sprintf("intake-%d", threadID))
}

func (s *Service) runJob(jobID int64) bool {
	if !s.claimJob(jobID) {
		return false
	}
	job, err := s.job(jobID)
	if err != nil {
		return false
	}
	workspace := s.jobWorkspace(jobID, job)
	if ok := s.ensureJobWorkspace(jobID, job, workspace); !ok {
		return false
	}

	client, err := NewCodexClient(s.cfg.ResolvedCodexBin())
	if err != nil {
		s.retryJobAfterWorkerError(jobID, "Codex app-server configuration failed: "+err.Error(), map[string]any{"error": err.Error()})
		return false
	}
	defer client.Close()
	if _, err := client.Initialize(); err != nil {
		s.retryJobAfterWorkerError(jobID, "Codex app-server initialization failed: "+err.Error(), map[string]any{"error": err.Error()})
		return false
	}
	codexThread, err := client.StartThread(s.cfg.CodexThreadOptions(workspace))
	if err != nil {
		s.retryJobAfterWorkerError(jobID, "Codex thread startup failed: "+err.Error(), map[string]any{"error": err.Error()})
		return false
	}
	workerSessionID := s.startWorkerSession(jobID, codexThread)
	s.updateJob(jobID, map[string]any{
		"codex_thread_id":  codexThread.ThreadID,
		"codex_session_id": codexThread.SessionID,
	})
	s.event(jobID, "codex_session_started", codexThread.SessionID, map[string]any{
		"codex_thread_id":   codexThread.ThreadID,
		"codex_session_id":  codexThread.SessionID,
		"worker_session_id": workerSessionID,
		"workspace":         workspace,
	})
	turn := client.RunTurn(codexThread.ThreadID, workerPrompt(job), time.Hour)
	s.event(jobID, "worker_completed", truncate(turn.FinalText, 2000), map[string]any{
		"events":            turn.Events,
		"error":             turn.Error,
		"codex_thread_id":   codexThread.ThreadID,
		"codex_session_id":  codexThread.SessionID,
		"worker_session_id": workerSessionID,
	})
	if turn.Error != "" {
		s.finishWorkerSession(workerSessionID, "failed", turn.Error)
		s.retryJobAfterWorkerError(jobID, "Codex worker turn failed: "+turn.Error, map[string]any{
			"error":             turn.Error,
			"codex_thread_id":   codexThread.ThreadID,
			"codex_session_id":  codexThread.SessionID,
			"worker_session_id": workerSessionID,
		})
		return false
	}
	s.finishWorkerSession(workerSessionID, "completed", "")
	s.finishJobFromWorkerOutput(jobID, turn.FinalText)
	return true
}

func (s *Service) startWorkerSession(jobID int64, ref CodexThreadRef) int64 {
	result, err := s.store.db.Exec(`
		INSERT INTO worker_sessions(
			job_id, codex_thread_id, codex_session_id, status, started_at
		) VALUES (?, ?, ?, 'running', ?)`,
		jobID,
		ref.ThreadID,
		ref.SessionID,
		utcNow(),
	)
	if err != nil {
		return 0
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0
	}
	return id
}

func (s *Service) finishWorkerSession(workerSessionID int64, status, errorText string) {
	if workerSessionID == 0 {
		return
	}
	_, _ = s.store.db.Exec(
		"UPDATE worker_sessions SET status=?, completed_at=?, error=? WHERE id=?",
		status,
		utcNow(),
		nullIfEmpty(errorText),
		workerSessionID,
	)
}

func (s *Service) jobWorkspace(jobID int64, job map[string]any) string {
	if workspace, ok := nonEmptyMapString(job["workspace_path"]); ok {
		return workspace
	}
	if threadID, ok := int64Value(job["intake_item_id"]); ok && threadID > 0 {
		return s.threadWorkspace(threadID)
	}
	return filepath.Join(s.cfg.WorkspaceRoot, fmt.Sprintf("job-%d", jobID))
}

func (s *Service) claimJob(jobID int64) bool {
	var status string
	if err := s.store.db.QueryRow("SELECT status FROM jobs WHERE id=?", jobID).Scan(&status); err != nil {
		return false
	}
	if status != "queued" {
		return false
	}
	s.jobsMu.Lock()
	if s.workingJobs == nil {
		s.workingJobs = make(map[int64]struct{})
	}
	if _, exists := s.workingJobs[jobID]; exists {
		s.jobsMu.Unlock()
		return false
	}
	s.workingJobs[jobID] = struct{}{}
	s.jobsMu.Unlock()

	s.updateJob(jobID, map[string]any{"current_block_reason": nil, "next_user_action": nil})
	s.event(jobID, "worker_started", "Worker claimed queued job.", map[string]any{"runtime_status": "working"})
	return true
}

func (s *Service) ensureJobWorkspace(jobID int64, job map[string]any, workspace string) bool {
	workspaceExists := false
	if info, err := os.Stat(workspace); err == nil && info.IsDir() {
		workspaceExists = true
	}
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		s.retryJobAfterWorkerError(jobID, "Workspace creation failed: "+err.Error(), map[string]any{"error": err.Error(), "workspace": workspace})
		return false
	}
	if workspaceExists {
		s.event(jobID, "workspace_reused", workspace, map[string]any{"workspace": workspace})
	} else {
		s.event(jobID, "workspace_created", workspace, map[string]any{"workspace": workspace})
	}
	s.updateJob(jobID, map[string]any{"workspace_path": workspace})

	if fmt.Sprint(job["workspace_path"]) == workspace && fmt.Sprint(job["bootstrap_status"]) == "succeeded" && workspaceExists {
		s.event(jobID, "bootstrap_skipped", "Workspace already bootstrapped.", map[string]any{"workspace": workspace})
		return true
	}

	bootstrap := s.cfg.ResolvedBootstrapCommand()
	if bootstrap == "" {
		s.updateJob(jobID, map[string]any{
			"status":               "blocked",
			"bootstrap_status":     "missing",
			"current_block_reason": "Workspace bootstrap command was not found",
			"next_user_action":     "Confirm the path for install-chi-skills or disable bootstrap.",
		})
		s.event(jobID, "blocked", "Bootstrap command missing", map[string]any{"command": s.cfg.WorkspaceBootstrapCommand})
		return false
	}

	s.updateJob(jobID, map[string]any{"bootstrap_status": "running"})
	s.event(jobID, "bootstrap_started", bootstrap, map[string]any{"command": bootstrap})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-lc", bootstrap)
	cmd.Dir = workspace
	output, err := cmd.CombinedOutput()
	if err != nil {
		s.updateJob(jobID, map[string]any{
			"status":               "blocked",
			"bootstrap_status":     "failed",
			"current_block_reason": "Bootstrap command failed: " + err.Error(),
			"next_user_action":     "Fix the workspace bootstrap command or approve running without it.",
		})
		s.event(jobID, "bootstrap_failed", truncate(string(output), 4000), map[string]any{"error": err.Error()})
		return false
	}
	s.event(jobID, "bootstrap_finished", truncate(string(output), 4000), map[string]any{"command": bootstrap})
	s.updateJob(jobID, map[string]any{"bootstrap_status": "succeeded"})
	return true
}

func (s *Service) UpdateReply(replyID int64, text string) (map[string]any, error) {
	if _, err := s.store.db.Exec("UPDATE reply_drafts SET edited_text=?, status='edited', updated_at=? WHERE id=?", text, utcNow(), replyID); err != nil {
		return nil, err
	}
	rows, err := s.store.queryMaps("SELECT * FROM reply_drafts WHERE id=?", replyID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, sql.ErrNoRows
	}
	return rows[0], nil
}

func (s *Service) SendReply(replyID int64) (map[string]any, error) {
	rows, err := s.store.queryMaps("SELECT * FROM reply_drafts WHERE id=?", replyID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, sql.ErrNoRows
	}
	reply := rows[0]
	text := fmt.Sprint(reply["draft_text"])
	if edited := fmt.Sprint(reply["edited_text"]); edited != "" && edited != "<nil>" {
		text = edited
	}
	if isEmptyReplyDraft(text) || looksLikeProcessNote(text) {
		return nil, fmt.Errorf("reply draft looks like worker process notes; edit it before sending")
	}
	client := NewSlackClient(s.cfg)
	payload, err := client.ChatPostMessage(fmt.Sprint(reply["slack_channel_id"]), fmt.Sprint(reply["slack_thread_ts"]), text)
	if err != nil {
		_, _ = s.store.db.Exec("UPDATE reply_drafts SET status='send_failed', updated_at=? WHERE id=?", utcNow(), replyID)
		return nil, err
	}
	ts, _ := payload["ts"].(string)
	channel, _ := payload["channel"].(string)
	permalink := ""
	if ts != "" && channel != "" {
		permalink, _ = client.ChatGetPermalink(channel, ts)
	}
	_, _ = s.store.db.Exec("UPDATE reply_drafts SET status='sent', slack_message_ts=?, sent_permalink=?, updated_at=? WHERE id=?", ts, permalink, utcNow(), replyID)
	_, _ = s.store.db.Exec("UPDATE intake_items SET status='resolved', resolution='reply_sent' WHERE id=?", reply["intake_item_id"])
	return map[string]any{"reply_id": replyID, "slack": payload, "sent_permalink": permalink}, nil
}

func (s *Service) ArchiveReply(replyID int64) (map[string]any, error) {
	var jobID sql.NullInt64
	var threadID int64
	var status string
	if err := s.store.db.QueryRow(
		"SELECT job_id, intake_item_id, status FROM reply_drafts WHERE id=?",
		replyID,
	).Scan(&jobID, &threadID, &status); err != nil {
		return nil, err
	}
	if status == "sent" {
		return nil, fmt.Errorf("sent replies cannot be archived as ignored")
	}

	now := utcNow()
	tx, err := s.store.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE reply_drafts SET status='ignored', updated_at=? WHERE id=?", now, replyID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec("UPDATE intake_items SET status='resolved', resolution='reply_ignored' WHERE id=?", threadID); err != nil {
		return nil, err
	}
	if jobID.Valid {
		if _, err := tx.Exec(`
			UPDATE jobs
			SET status='completed_no_reply',
			    current_block_reason=NULL,
			    next_user_action=NULL,
			    updated_at=?
			WHERE id=?`, now, jobID.Int64); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(
			"INSERT INTO job_events(job_id, event_type, message, payload_json, created_at) VALUES (?, 'reply_ignored', ?, ?, ?)",
			jobID.Int64,
			"Reply draft ignored and item archived.",
			mustJSON(map[string]any{"reply_id": replyID}),
			now,
		); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"reply_id": replyID, "intake_item_id": threadID, "status": "ignored"}, nil
}

func (s *Service) threadContext(threadID int64) (map[string]any, error) {
	threads, err := s.store.queryMaps("SELECT * FROM intake_items WHERE id=?", threadID)
	if err != nil {
		return nil, err
	}
	if len(threads) == 0 {
		return nil, sql.ErrNoRows
	}
	return threads[0], nil
}

func (s *Service) relatedJobsForThread(threadID int64, limit int) []map[string]any {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.store.queryMaps(`
		SELECT j.id, j.intake_item_id, j.title, j.status, j.urgency, j.task_type,
		       j.current_block_reason, j.next_user_action, j.created_at, j.updated_at,
		       st.channel_id, st.channel_name, st.trigger_ts, st.thread_ts, st.source_type,
		       st.title AS thread_title, st.latest_slack_message_ts,
		       ar.summary AS analyzer_summary, ar.rationale AS analyzer_rationale,
		       (
		         SELECT rd.status
		         FROM reply_drafts rd
		         WHERE rd.job_id = j.id
		         ORDER BY rd.updated_at DESC, rd.id DESC
		         LIMIT 1
		       ) AS latest_reply_status,
		       (
		         SELECT rd.updated_at
		         FROM reply_drafts rd
		         WHERE rd.job_id = j.id
		         ORDER BY rd.updated_at DESC, rd.id DESC
		         LIMIT 1
		       ) AS latest_reply_updated_at
		FROM jobs j
		JOIN intake_items st ON st.id = j.intake_item_id
		JOIN intake_items trigger_thread ON trigger_thread.id = ?
		LEFT JOIN analysis_runs ar ON ar.id = (
			SELECT id
			FROM analysis_runs
			WHERE intake_item_id = st.id
			ORDER BY id DESC
			LIMIT 1
		)
		WHERE st.slack_team_id = trigger_thread.slack_team_id
		  AND st.channel_id = trigger_thread.channel_id
		ORDER BY COALESCE(st.latest_slack_message_ts, st.trigger_ts) DESC,
		         j.updated_at DESC,
		         j.id DESC
		LIMIT ?`, threadID, limit)
	if err != nil {
		return nil
	}
	return rows
}

func (s *Service) job(jobID int64) (map[string]any, error) {
	rows, err := s.store.queryMaps(`
		SELECT j.*,
		       st.slack_team_id, st.channel_id, st.channel_name, st.trigger_ts, st.thread_ts,
		       st.source_type, st.permalink,
		       st.title AS thread_title, st.trigger_user_id, st.trigger_text,
		       st.trigger_user_id AS root_user_id, st.trigger_text AS root_text,
		       st.latest_slack_message_ts, st.latest_user_id,
		       st.latest_user_name, st.latest_text
		FROM jobs j
		JOIN intake_items st ON st.id = j.intake_item_id
		WHERE j.id=?`, jobID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, sql.ErrNoRows
	}
	return rows[0], nil
}

func (s *Service) updateJob(jobID int64, fields map[string]any) {
	fields["updated_at"] = utcNow()
	var sets []string
	var args []any
	for key, value := range fields {
		sets = append(sets, key+"=?")
		args = append(args, value)
	}
	args = append(args, jobID)
	_, _ = s.store.db.Exec("UPDATE jobs SET "+strings.Join(sets, ", ")+" WHERE id=?", args...)
}

func (s *Service) retryJobAfterWorkerError(jobID int64, reason string, payload map[string]any) {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["retry_status"] = "queued"
	s.event(jobID, "worker_retry_scheduled", reason, payload)
	s.updateJob(jobID, map[string]any{
		"status":               "queued",
		"current_block_reason": nil,
		"next_user_action":     nil,
	})
}

func (s *Service) recoverInterruptedJobs() {
	rows, err := s.store.queryMaps(`
		SELECT id, status
		FROM jobs
		WHERE status IN ('workspace_creating', 'bootstrapping', 'working')
		ORDER BY id`)
	if err != nil {
		return
	}
	for _, row := range rows {
		jobID, ok := int64Value(row["id"])
		if !ok {
			continue
		}
		previousStatus := fmt.Sprint(row["status"])
		result, err := s.store.db.Exec(
			`UPDATE jobs
			 SET status='queued', current_block_reason=NULL, next_user_action=NULL, updated_at=?
			 WHERE id=? AND status=?`,
			utcNow(),
			jobID,
			previousStatus,
		)
		if err != nil {
			continue
		}
		updated, err := result.RowsAffected()
		if err != nil || updated == 0 {
			continue
		}
		s.event(jobID, "worker_recovered", "Recovered interrupted worker state.", map[string]any{
			"previous_status": previousStatus,
			"retry_status":    "queued",
		})
	}
}

func (s *Service) recoverInterruptedWorkerSessions() {
	_, _ = s.store.db.Exec(`
		UPDATE worker_sessions
		SET status='failed', completed_at=?, error=?
		WHERE status='running'`,
		utcNow(),
		"Worker session interrupted before completion.",
	)
}

func (s *Service) updateIntakeResolutionForJob(jobID int64, resolution string) {
	_, _ = s.store.db.Exec(
		"UPDATE intake_items SET status='resolved', resolution=? WHERE id=(SELECT intake_item_id FROM jobs WHERE id=?)",
		resolution,
		jobID,
	)
}

func (s *Service) event(jobID int64, eventType, message string, payload map[string]any) {
	_, _ = s.store.db.Exec(
		"INSERT INTO job_events(job_id, event_type, message, payload_json, created_at) VALUES (?, ?, ?, ?, ?)",
		jobID,
		eventType,
		message,
		mustJSON(payload),
		utcNow(),
	)
}

func (s *Service) createReply(jobID int64, draftText, rationale string) {
	rows, err := s.store.queryMaps(`
		SELECT j.intake_item_id, st.channel_id, st.thread_ts
		FROM jobs j
		JOIN intake_items st ON st.id = j.intake_item_id
		WHERE j.id = ?`, jobID)
	if err != nil || len(rows) == 0 {
		return
	}
	row := rows[0]
	_, _ = s.store.db.Exec(`
		INSERT INTO reply_drafts(
			job_id, intake_item_id, status, draft_text, edited_text, rationale,
			slack_channel_id, slack_thread_ts, created_at, updated_at
		) VALUES (?, ?, 'draft', ?, NULL, ?, ?, ?, ?, ?)`,
		jobID,
		row["intake_item_id"],
		draftText,
		rationale,
		row["channel_id"],
		row["thread_ts"],
		utcNow(),
		utcNow(),
	)
}

func (s *Service) finishJobFromWorkerOutput(jobID int64, finalText string) {
	result := parseWorkerOutput(finalText)
	if result.ReplyDraft != "" {
		s.createReply(jobID, result.ReplyDraft, "Generated from Codex worker result.")
	}
	switch result.Outcome {
	case "blocked":
		reason := result.BlockerReason
		if reason == "" {
			reason = "Worker reported blocked."
		}
		s.updateJob(jobID, map[string]any{
			"status":               "blocked",
			"current_block_reason": reason,
			"next_user_action":     nullIfEmpty(result.NextUserAction),
		})
		s.event(jobID, "blocked", reason, map[string]any{
			"outcome":          result.Outcome,
			"next_user_action": result.NextUserAction,
			"reply_draft":      result.ReplyDraft != "",
		})
	case "failed":
		reason := result.BlockerReason
		if reason == "" {
			reason = "Worker reported failed."
		}
		s.updateJob(jobID, map[string]any{
			"status":               "failed",
			"current_block_reason": reason,
			"next_user_action":     nullIfEmpty(result.NextUserAction),
		})
		s.event(jobID, "worker_failed", reason, map[string]any{
			"outcome":          result.Outcome,
			"next_user_action": result.NextUserAction,
			"reply_draft":      result.ReplyDraft != "",
		})
	case "":
		fallthrough
	case "completed", "advanced":
		if result.ReplyDraft != "" {
			s.updateJob(jobID, map[string]any{
				"status":               "draft_ready",
				"current_block_reason": nil,
				"next_user_action":     nil,
			})
			return
		}
		s.updateJob(jobID, map[string]any{
			"status":               "completed_no_reply",
			"current_block_reason": nil,
			"next_user_action":     nil,
		})
		s.updateIntakeResolutionForJob(jobID, "completed_no_reply")
	default:
		if result.ReplyDraft != "" {
			s.updateJob(jobID, map[string]any{
				"status":               "draft_ready",
				"current_block_reason": nil,
				"next_user_action":     nil,
			})
			return
		}
		s.updateJob(jobID, map[string]any{
			"status":               "completed_no_reply",
			"current_block_reason": nil,
			"next_user_action":     nil,
		})
		s.updateIntakeResolutionForJob(jobID, "completed_no_reply")
	}
}

func analysisPrompt(thread map[string]any, relatedJobs []map[string]any) string {
	return fmt.Sprintf(`Analyze this Slack intake trigger for my personal work agent.

This Slack message/thread is a trigger event. Do not assume it is a standalone task.

The local database stores only Slack trigger metadata, not a full Slack message transcript. Before deciding, use the available Slack context skill/capabilities from the workspace to fetch the latest relevant Slack messages by channel_id, thread_ts, and permalink. For DMs especially, inspect nearby messages in the same DM around the trigger timestamp; people often continue the same topic as separate DM messages instead of Slack thread replies. If the trigger belongs to a native Slack thread, inspect the full thread as well.

Related jobs from the same Slack DM/channel are included below, sorted by most recent activity, with at most 20 jobs. Also check whether the same topic is already covered by an existing or running job, a pending reply draft, a sent system reply, or my own Slack reply. If it is already covered or already answered, do not create another worker job.

Decide whether I need to take action. If action is needed, summarize the task and what a worker should try.

Return exactly one JSON object and no Markdown or prose. Use this schema:
{
  "action_required": true,
  "confidence": 0.0,
  "summary": "一句简短中文总结",
  "task_title": "简短中文工作项标题",
  "urgency": "low | medium | high | urgent",
  "why_it_matters": "中文说明为何需要关注，或空字符串",
  "worker_plan": "给 Codex worker 的中文具体首步，或空字符串",
  "needed_user_confirmation": "Slack 发送前需要我确认的中文事项，或空字符串",
  "rationale": "中文简短决策理由"
}

Rules:
- Use action_required=false when no worker should be created.
- Use action_required=false when this trigger is only additional context for an existing/running job, or when the latest relevant user-facing Slack reply already answered it.
- Confidence must be a number from 0 to 1, not a word.
- Keep values Slack-work focused and concise.
- Keep JSON keys exactly as shown, but write user-facing string values in Simplified Chinese, including summary, task_title, why_it_matters, worker_plan, needed_user_confirmation, and rationale.

Slack trigger metadata:
%v

Related jobs from same Slack DM/channel:
%v
`, thread, relatedJobs)
}

func workerPrompt(job map[string]any) string {
	return fmt.Sprintf(`You are a Codex worker for my personal Slack Agent.

You may use local files, commands, repos, tests, and tools that work inside the local Codex workspace to move this task forward.
Network access and permissions outside the local workspace are intentionally unavailable by default. If they are required, report the blocker instead of waiting for approval.
The local database stores only Slack trigger metadata, not a full Slack message transcript. Before working, use the available Slack context skill/capabilities from the workspace to refresh the latest relevant Slack context by channel_id, thread_ts, and permalink. For DM jobs, inspect nearby messages in the same DM around the original trigger and any newer messages in the same conversation; people often continue the same topic as separate DM messages instead of Slack thread replies.
If refreshed context shows this topic is already handled by another active job, a sent system reply, or my own Slack reply after the latest relevant external message, stop without drafting another reply and report completed_no_reply with evidence.
Hard policy: do not send Slack messages. If a Slack reply is useful, draft it only.
Before drafting a Slack reply, check again whether a Slack reply has already been sent or I have already replied after the latest relevant external message.
The Slack reply draft must be only the exact text I could send to Slack. Do not include process notes, database details, marker names, implementation commentary, or anything about this worker format. If no safe direct Slack reply is ready, write SLACK_REPLY_DRAFT: NONE.

When finished, include:
OUTCOME: completed | advanced | blocked | failed
ACTIONS_TAKEN:
EVIDENCE:
CHANGED_FILES:
BLOCKER_REASON:
NEXT_USER_ACTION:
SLACK_REPLY_DRAFT:

Job:
%v
`, job)
}

type workerOutput struct {
	Outcome        string
	BlockerReason  string
	NextUserAction string
	ReplyDraft     string
}

func parseWorkerOutput(text string) workerOutput {
	return workerOutput{
		Outcome:        normalizeWorkerOutcome(extractWorkerSection(text, "OUTCOME")),
		BlockerReason:  cleanWorkerField(extractWorkerSection(text, "BLOCKER_REASON")),
		NextUserAction: cleanWorkerField(extractWorkerSection(text, "NEXT_USER_ACTION")),
		ReplyDraft:     extractReply(text),
	}
}

func extractReply(text string) string {
	draft := extractWorkerSection(text, "SLACK_REPLY_DRAFT")
	draft = stripMarkdownFence(draft)
	if isEmptyReplyDraft(draft) || looksLikeProcessNote(draft) {
		return ""
	}
	return draft
}

var workerSectionMarker = regexp.MustCompile(`(?im)^[ \t]*(OUTCOME|ACTIONS_TAKEN|EVIDENCE|CHANGED_FILES|BLOCKER_REASON|NEXT_USER_ACTION|SLACK_REPLY_DRAFT):[ \t]*`)

func extractWorkerSection(text, section string) string {
	matches := workerSectionMarker.FindAllStringSubmatchIndex(text, -1)
	for i, match := range matches {
		if len(match) < 4 || !strings.EqualFold(text[match[2]:match[3]], section) {
			continue
		}
		valueStart := match[1]
		valueEnd := len(text)
		if i+1 < len(matches) {
			valueEnd = matches[i+1][0]
		}
		if lineEnd := strings.Index(text[valueStart:valueEnd], "\n"); lineEnd >= 0 {
			inline := strings.TrimSpace(text[valueStart : valueStart+lineEnd])
			if inline != "" {
				return inline
			}
			valueStart = valueStart + lineEnd + 1
		}
		return strings.TrimSpace(text[valueStart:valueEnd])
	}
	return ""
}

func normalizeWorkerOutcome(text string) string {
	value := cleanWorkerField(text)
	if value == "" {
		return ""
	}
	value = strings.ToLower(value)
	value = strings.NewReplacer("-", " ", "_", " ", "|", " ").Replace(value)
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return ""
	}
	switch fields[0] {
	case "completed", "complete", "done":
		return "completed"
	case "advanced", "advance":
		return "advanced"
	case "blocked", "blocker":
		return "blocked"
	case "failed", "failure", "error":
		return "failed"
	default:
		return fields[0]
	}
}

func cleanWorkerField(text string) string {
	value := stripMarkdownFence(text)
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "`*_ ")
	if isEmptyWorkerField(value) {
		return ""
	}
	return value
}

func isEmptyWorkerField(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "", "none", "n/a", "na", "no", "no action", "no user action", "not needed", "not required", "-":
		return true
	default:
		return false
	}
}

func isEmptyReplyDraft(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "", "none", "n/a", "na", "no reply", "no slack reply", "not needed":
		return true
	default:
		return false
	}
}

func looksLikeProcessNote(text string) bool {
	lower := strings.ToLower(text)
	processSignals := []string{
		"slack_reply_draft",
		"reply_drafts",
		"final reply marker",
		"worker format",
		"structured final response",
		"write directly to the database",
		"i'm reading",
		"i’m reading",
		"i'll inspect",
		"i’ll inspect",
		"i am checking",
		"i’m checking",
		"workspace only contains",
	}
	for _, signal := range processSignals {
		if strings.Contains(lower, signal) {
			return true
		}
	}
	return false
}

func stripMarkdownFence(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) >= 2 && strings.HasPrefix(strings.TrimSpace(lines[0]), "```") && strings.TrimSpace(lines[len(lines)-1]) == "```" {
		return strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
	}
	return strings.TrimSpace(text)
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nonEmptyMapString(value any) (string, bool) {
	text := strings.TrimSpace(fmt.Sprint(value))
	if text == "" || text == "<nil>" {
		return "", false
	}
	return text, true
}

func int64Value(value any) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case float64:
		return int64(typed), true
	case string:
		parsed, err := strconv.ParseInt(typed, 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}
