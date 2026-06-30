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
	cfg            Config
	store          *Store
	analyses       chan int64
	jobs           chan int64
	autoAdvance    chan struct{}
	analysisMu     sync.Mutex
	queuedAnalyses map[int64]struct{}
	jobsMu         sync.Mutex
	queuedJobs     map[int64]struct{}
	workingJobs    map[int64]struct{}
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
		cfg:            cfg,
		store:          store,
		analyses:       make(chan int64, 100),
		jobs:           make(chan int64, 100),
		autoAdvance:    make(chan struct{}, 1),
		queuedAnalyses: make(map[int64]struct{}),
		queuedJobs:     make(map[int64]struct{}),
		workingJobs:    make(map[int64]struct{}),
	}
	service.recoverInterruptedJobs()
	for i := 0; i < analyzers; i++ {
		go service.analyzerLoop()
	}
	for i := 0; i < workers; i++ {
		go service.workerLoop()
	}
	if cfg.AutoAdvance {
		go service.autoAdvanceLoop()
		service.signalAutoAdvance()
	}
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
	client := NewSlackClient(s.cfg)
	auth, err := client.AuthTest()
	if err != nil {
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
	seenThreads := map[string]struct{}{}
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
		lastSeenTS := s.store.metadata(cursorKey)
		searchQuery := query.Query
		if after := slackSearchAfterDate(lastSeenTS, s.cfg.SlackSearchLookbackDays); after != "" {
			searchQuery += " after:" + after
		}
		maxSeenTS := lastSeenTS
		pageSize := positiveInt(s.cfg.SlackSearchPageSize, 100)
		for page := 1; shouldFetchPage(page, s.cfg.SlackSearchMaxPages); page++ {
			matches, pageInfo, err := client.SearchMessages(searchQuery, pageSize, page)
			if err != nil {
				errors = append(errors, err.Error())
				break
			}
			if len(matches) == 0 {
				break
			}
			for _, match := range matches {
				if ts := slackMessageTS(match); slackTSAfter(ts, maxSeenTS) {
					maxSeenTS = ts
				}
				result, err := s.collectThreadFromMessage(client, teamID, "", "", query.Source, match, userID, seenThreads)
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
		if slackTSAfter(maxSeenTS, lastSeenTS) {
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
			lastSeenTS := s.store.metadata(cursorKey)
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
				result, err := s.collectThreadFromMessage(client, teamID, channelID, "DM", "dm", message, userID, seenThreads)
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
			if slackTSAfter(maxSeenTS, lastSeenTS) {
				if err := s.store.setMetadata(cursorKey, maxSeenTS); err != nil {
					errors = append(errors, "dm cursor "+channelID+": "+err.Error())
				}
			}
		}
	}
	errors = append(errors, s.resolveSlackUsers(client, 120)...)
	s.signalAutoAdvance()
	return map[string]any{
		"collected":    collected,
		"skipped":      skipped,
		"errors":       errors,
		"synced_at":    utcNow(),
		"auto_advance": s.cfg.AutoAdvance,
	}, nil
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
	if channelID == "" || threadTS == "" {
		return threadCollectResult{skipped: true}, nil
	}
	key := slackThreadKey(teamID, channelID, threadTS)
	if _, exists := seen[key]; exists {
		return threadCollectResult{skipped: true}, nil
	}
	seen[key] = struct{}{}

	if latestTS, ok := candidateThreadLatestTS(match); ok && s.threadKnownUpToDate(teamID, channelID, threadTS, latestTS) {
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
	root := match
	if len(messages) > 0 {
		root = messages[0]
	}
	if firstString(root["permalink"]) == "" {
		if permalink, err := client.ChatGetPermalink(channelID, threadTS); err == nil {
			root["permalink"] = permalink
		}
	}
	if err := s.upsertThread(teamID, channelID, channelName, threadTS, sourceType, root, messages, userID); err != nil {
		return threadCollectResult{}, err
	}
	return threadCollectResult{collected: true}, nil
}

func (s *Service) threadKnownUpToDate(teamID, channelID, threadTS, latestTS string) bool {
	if latestTS == "" {
		return false
	}
	var storedLatest sql.NullString
	err := s.store.db.QueryRow(`
		SELECT COALESCE(st.latest_slack_message_ts, (
		         SELECT MAX(sm.slack_message_ts)
		         FROM slack_messages sm
		         WHERE sm.slack_thread_id = st.id
		       )) AS latest_slack_message_ts
		FROM slack_threads st
		WHERE st.slack_team_id=? AND st.channel_id=? AND st.thread_ts=?`,
		teamID,
		channelID,
		threadTS,
	).Scan(&storedLatest)
	if err != nil || !storedLatest.Valid || storedLatest.String == "" {
		return false
	}
	return !slackTSAfter(latestTS, storedLatest.String)
}

func (s *Service) resolveSlackUsers(client *SlackClient, limit int) []string {
	rows, err := s.store.db.Query(`
		SELECT DISTINCT sm.user_id
		FROM slack_messages sm
		LEFT JOIN slack_users su ON su.user_id = sm.user_id
		WHERE sm.user_id IS NOT NULL
		  AND sm.user_id != ''
		  AND (su.user_id IS NULL OR COALESCE(su.display_name, '') = '')
		ORDER BY sm.user_id
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
		_, _ = s.store.db.Exec(
			"UPDATE slack_messages SET user_name=? WHERE user_id=? AND COALESCE(user_name, '') = ''",
			displayName,
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

func latestSlackMessageTS(messages []map[string]any) string {
	latest := ""
	for _, message := range messages {
		if ts := slackMessageTS(message); slackTSAfter(ts, latest) {
			latest = ts
		}
	}
	return latest
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
	case "collected", "analysis_queued", "analyzing":
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

func slackTimestampToUTC(ts string) string {
	parsed, ok := parseSlackTimestamp(ts)
	if !ok {
		return utcNow()
	}
	return parsed.UTC().Format(time.RFC3339)
}

func slackSearchAfterDate(cursor string, lookbackDays int) string {
	if cursor == "" || lookbackDays <= 0 {
		return ""
	}
	parsed, ok := parseSlackTimestamp(cursor)
	if !ok {
		return ""
	}
	return parsed.AddDate(0, 0, -lookbackDays).UTC().Format("2006-01-02")
}

func slackOldestFromCursor(cursor string, lookbackDays int) string {
	if cursor == "" || lookbackDays <= 0 {
		return ""
	}
	parsed, ok := parseSlackTimestamp(cursor)
	if !ok {
		return ""
	}
	return formatSlackTimestamp(parsed.AddDate(0, 0, -lookbackDays))
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

func (s *Service) upsertThread(teamID, channelID, channelName, threadTS, sourceType string, root map[string]any, messages []map[string]any, userID string) error {
	title, _ := root["text"].(string)
	title = strings.TrimSpace(strings.ReplaceAll(title, "\n", " "))
	if len(title) > 90 {
		title = title[:90]
	}
	if title == "" {
		title = "Untitled Slack thread"
	}
	rootTS, _ := root["ts"].(string)
	if rootTS == "" {
		rootTS = threadTS
	}
	latestTS := latestSlackMessageTS(messages)
	if latestTS == "" {
		latestTS = slackMessageTS(root)
	}
	if latestTS == "" {
		latestTS = threadTS
	}
	activityAt := slackTimestampToUTC(latestTS)
	now := utcNow()
	tx, err := s.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var existingID int64
	var existingStatus sql.NullString
	var existingLatestTS sql.NullString
	var existingActivityAt sql.NullString
	err = tx.QueryRow(`
		SELECT st.id, st.status,
		       COALESCE(st.latest_slack_message_ts, (
		         SELECT MAX(sm.slack_message_ts)
		         FROM slack_messages sm
		         WHERE sm.slack_thread_id = st.id
		       )) AS latest_slack_message_ts,
		       st.last_slack_activity_at
		FROM slack_threads st
		WHERE st.slack_team_id=? AND st.channel_id=? AND st.thread_ts=?`,
		teamID,
		channelID,
		threadTS,
	).Scan(&existingID, &existingStatus, &existingLatestTS, &existingActivityAt)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	hasExisting := err == nil
	status := "collected"
	if hasExisting {
		status = existingStatus.String
		if status == "" {
			status = "collected"
		}
	}
	hasNewSlackActivity := !hasExisting || !existingLatestTS.Valid || existingLatestTS.String == "" || slackTSAfter(latestTS, existingLatestTS.String)
	if hasExisting && hasNewSlackActivity && shouldRecollectThread(status) {
		status = "collected"
	}
	if hasExisting && existingLatestTS.Valid && existingLatestTS.String != "" && !slackTSAfter(latestTS, existingLatestTS.String) {
		latestTS = existingLatestTS.String
		if existingActivityAt.Valid && existingActivityAt.String != "" {
			activityAt = existingActivityAt.String
		}
	}

	if _, err := tx.Exec(`
		INSERT INTO slack_threads(
			slack_team_id, channel_id, channel_name, thread_ts, root_message_ts,
			source_type, status, title, permalink, last_slack_activity_at,
			latest_slack_message_ts, last_synced_at, raw_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(slack_team_id, channel_id, thread_ts) DO UPDATE SET
			channel_name=excluded.channel_name,
			root_message_ts=excluded.root_message_ts,
			source_type=excluded.source_type,
			status=excluded.status,
			title=excluded.title,
			permalink=COALESCE(NULLIF(excluded.permalink, ''), slack_threads.permalink),
			last_slack_activity_at=excluded.last_slack_activity_at,
			latest_slack_message_ts=excluded.latest_slack_message_ts,
			last_synced_at=excluded.last_synced_at,
			raw_json=excluded.raw_json`,
		teamID,
		channelID,
		channelName,
		threadTS,
		rootTS,
		sourceType,
		status,
		title,
		root["permalink"],
		activityAt,
		latestTS,
		now,
		mustJSON(root),
	); err != nil {
		return err
	}
	var threadID int64
	if err := tx.QueryRow(
		"SELECT id FROM slack_threads WHERE slack_team_id=? AND channel_id=? AND thread_ts=?",
		teamID,
		channelID,
		threadTS,
	).Scan(&threadID); err != nil {
		return err
	}
	for _, message := range messages {
		ts, _ := message["ts"].(string)
		if ts == "" {
			continue
		}
		text, _ := message["text"].(string)
		msgUser, _ := message["user"].(string)
		mentions := 0
		if strings.Contains(text, "<@"+userID+">") {
			mentions = 1
		}
		isUser := 0
		if msgUser == userID {
			isUser = 1
		}
		if _, err := tx.Exec(`
			INSERT INTO slack_messages(
				slack_thread_id, slack_message_ts, user_id, user_name, text,
				is_user_message, mentions_user, raw_json
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(slack_thread_id, slack_message_ts) DO UPDATE SET
					user_id=excluded.user_id,
					user_name=excluded.user_name,
					text=excluded.text,
					is_user_message=excluded.is_user_message,
					mentions_user=excluded.mentions_user,
					raw_json=excluded.raw_json`,
			threadID,
			ts,
			msgUser,
			message["username"],
			text,
			isUser,
			mentions,
			mustJSON(message),
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Service) QueueAnalysis(threadID int64) (map[string]any, error) {
	var currentStatus string
	if err := s.store.db.QueryRow("SELECT status FROM slack_threads WHERE id=?", threadID).Scan(&currentStatus); err != nil {
		return nil, err
	}
	if currentStatus == "analysis_queued" {
		queued := s.enqueueAnalysis(threadID)
		return map[string]any{"queued": queued, "thread_id": threadID, "status": currentStatus, "reason": "already_queued"}, nil
	}
	if currentStatus == "analyzing" {
		return map[string]any{"queued": false, "thread_id": threadID, "status": currentStatus, "reason": "already_analyzing"}, nil
	}
	if currentStatus != "collected" && currentStatus != "analysis_failed" {
		return map[string]any{"queued": false, "thread_id": threadID, "status": currentStatus, "reason": "not_analyzable"}, nil
	}
	result, err := s.store.db.Exec(
		"UPDATE slack_threads SET status='analysis_queued' WHERE id=? AND status IN ('collected', 'analysis_failed')",
		threadID,
	)
	if err != nil {
		return nil, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if updated == 0 {
		return map[string]any{"queued": false, "thread_id": threadID, "reason": "status_changed"}, nil
	}
	s.enqueueAnalysis(threadID)
	return map[string]any{"queued": true, "thread_id": threadID, "status": "analysis_queued"}, nil
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
		delete(s.queuedAnalyses, threadID)
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
	if !s.cfg.AutoAdvance {
		return
	}
	select {
	case s.autoAdvance <- struct{}{}:
	default:
	}
}

func (s *Service) autoAdvanceLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.autoAdvance:
			s.advanceIntake()
		case <-ticker.C:
			s.advanceIntake()
		}
	}
}

func (s *Service) advanceIntake() {
	s.queuePendingAnalyses()
	s.queuePendingJobs()
}

func (s *Service) queuePendingAnalyses() {
	rows, err := s.store.db.Query(`
		SELECT st.id, st.status
		FROM slack_threads st
		WHERE st.status IN ('collected', 'analysis_queued', 'analyzing')
		ORDER BY st.last_slack_activity_at ASC, st.id ASC`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var threadID int64
		var status string
		if err := rows.Scan(&threadID, &status); err != nil {
			continue
		}
		if status == "analysis_queued" || status == "analyzing" {
			s.enqueueAnalysis(threadID)
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
	if _, exists := s.queuedAnalyses[threadID]; exists {
		s.analysisMu.Unlock()
		return false
	}
	s.queuedAnalyses[threadID] = struct{}{}
	s.analysisMu.Unlock()
	s.analyses <- threadID
	return true
}

func (s *Service) runAnalysis(threadID int64) bool {
	started := utcNow()
	_, _ = s.store.db.Exec("UPDATE slack_threads SET status='analyzing' WHERE id=?", threadID)
	result, err := s.store.db.Exec("INSERT INTO analysis_runs(slack_thread_id, status, started_at) VALUES (?, 'running', ?)", threadID, started)
	if err != nil {
		return false
	}
	analysisID, _ := result.LastInsertId()
	thread, messages, err := s.threadContext(threadID)
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
	codexThreadID, err := client.StartThread(s.cfg.CodexThreadOptions(workspace))
	if err != nil {
		s.finishAnalysisError(threadID, analysisID, err)
		return false
	}
	turn := client.RunTurn(codexThreadID, analysisPrompt(thread, messages, s.relatedJobsForThread(threadID, 20)), 10*time.Minute)
	final := turn.FinalText
	analysis := parseAnalysisOutput(final)
	status := "completed"
	if turn.Error != "" {
		status = "failed"
	}
	_, _ = s.store.db.Exec(`
		UPDATE analysis_runs
		SET status=?, action_required=?, confidence=?, summary=?, rationale=?,
			structured_result_json=?, codex_thread_id=?, completed_at=?, error=?
			WHERE id=?`,
		status,
		boolToInt(analysis.ActionRequired),
		analysis.confidenceValue(),
		truncate(analysis.Summary, 800),
		truncate(analysis.Rationale, 800),
		analysis.structuredResultJSON(final, turn.Events),
		codexThreadID,
		utcNow(),
		nullIfEmpty(turn.Error),
		analysisID,
	)
	if turn.Error != "" {
		_, _ = s.store.db.Exec("UPDATE slack_threads SET status='analysis_queued' WHERE id=?", threadID)
		return false
	}
	analyzedSlackTS := latestSlackTSFromThread(thread)
	if !analysis.ActionRequired {
		_, _ = s.store.db.Exec("UPDATE slack_threads SET status='no_action', last_analyzed_slack_ts=? WHERE id=?", analyzedSlackTS, threadID)
		return true
	}
	_, _ = s.store.db.Exec("UPDATE slack_threads SET status='job_created', last_analyzed_slack_ts=? WHERE id=?", analyzedSlackTS, threadID)
	title := analysis.TaskTitle
	if title == "" {
		title, _ = thread["title"].(string)
	}
	if title == "" {
		title = fmt.Sprintf("Slack thread %d", threadID)
	}
	urgency := analysis.Urgency
	if urgency == "" {
		urgency = "medium"
	}
	insertResult, err := s.store.db.Exec(`
		INSERT INTO jobs(
			slack_thread_id, analysis_run_id, title, status, urgency, task_type,
			workspace_path, bootstrap_status, created_at, updated_at
		) VALUES (?, ?, ?, 'queued', ?, 'slack_task', ?, 'succeeded', ?, ?)`,
		threadID,
		analysisID,
		title,
		urgency,
		workspace,
		utcNow(),
		utcNow(),
	)
	if err != nil {
		s.finishAnalysisError(threadID, analysisID, err)
		return false
	}
	if jobID, err := insertResult.LastInsertId(); err == nil && s.cfg.AutoAdvance {
		_, _ = s.QueueJob(jobID)
	}
	return true
}

func (s *Service) finishAnalysisError(threadID, analysisID int64, err error) {
	_, _ = s.store.db.Exec("UPDATE analysis_runs SET status='failed', completed_at=?, error=? WHERE id=?", utcNow(), err.Error(), analysisID)
	_, _ = s.store.db.Exec("UPDATE slack_threads SET status='analysis_queued' WHERE id=?", threadID)
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
	return filepath.Join(s.cfg.WorkspaceRoot, fmt.Sprintf("thread-%d", threadID))
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
	codexThreadID, err := client.StartThread(s.cfg.CodexThreadOptions(workspace))
	if err != nil {
		s.retryJobAfterWorkerError(jobID, "Codex thread startup failed: "+err.Error(), map[string]any{"error": err.Error()})
		return false
	}
	s.updateJob(jobID, map[string]any{"codex_thread_id": codexThreadID})
	turn := client.RunTurn(codexThreadID, workerPrompt(job, s.threadMessages(jobID)), time.Hour)
	s.event(jobID, "worker_completed", truncate(turn.FinalText, 2000), map[string]any{"events": turn.Events, "error": turn.Error})
	if turn.Error != "" {
		s.retryJobAfterWorkerError(jobID, "Codex worker turn failed: "+turn.Error, map[string]any{"error": turn.Error, "codex_thread_id": codexThreadID})
		return false
	}
	s.finishJobFromWorkerOutput(jobID, turn.FinalText)
	return true
}

func (s *Service) jobWorkspace(jobID int64, job map[string]any) string {
	if workspace, ok := nonEmptyMapString(job["workspace_path"]); ok {
		return workspace
	}
	if threadID, ok := int64Value(job["slack_thread_id"]); ok && threadID > 0 {
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
	_, _ = s.store.db.Exec("UPDATE slack_threads SET status='archived' WHERE id=?", reply["slack_thread_id"])
	return map[string]any{"reply_id": replyID, "slack": payload, "sent_permalink": permalink}, nil
}

func (s *Service) ArchiveReply(replyID int64) (map[string]any, error) {
	var jobID sql.NullInt64
	var threadID int64
	var status string
	if err := s.store.db.QueryRow(
		"SELECT job_id, slack_thread_id, status FROM reply_drafts WHERE id=?",
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
	if _, err := tx.Exec("UPDATE slack_threads SET status='archived' WHERE id=?", threadID); err != nil {
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
	return map[string]any{"reply_id": replyID, "thread_id": threadID, "status": "ignored"}, nil
}

func (s *Service) threadContext(threadID int64) (map[string]any, []map[string]any, error) {
	threads, err := s.store.queryMaps("SELECT * FROM slack_threads WHERE id=?", threadID)
	if err != nil {
		return nil, nil, err
	}
	if len(threads) == 0 {
		return nil, nil, sql.ErrNoRows
	}
	messages, err := s.store.queryMaps("SELECT * FROM slack_messages WHERE slack_thread_id=? ORDER BY slack_message_ts", threadID)
	return threads[0], messages, err
}

func (s *Service) relatedJobsForThread(threadID int64, limit int) []map[string]any {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.store.queryMaps(`
		SELECT j.id, j.slack_thread_id, j.title, j.status, j.urgency, j.task_type,
		       j.current_block_reason, j.next_user_action, j.created_at, j.updated_at,
		       st.channel_id, st.channel_name, st.thread_ts, st.source_type,
		       st.title AS thread_title, st.last_slack_activity_at, st.latest_slack_message_ts,
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
		JOIN slack_threads st ON st.id = j.slack_thread_id
		JOIN slack_threads trigger_thread ON trigger_thread.id = ?
		LEFT JOIN analysis_runs ar ON ar.id = (
			SELECT id
			FROM analysis_runs
			WHERE slack_thread_id = st.id
			ORDER BY id DESC
			LIMIT 1
		)
		WHERE st.slack_team_id = trigger_thread.slack_team_id
		  AND st.channel_id = trigger_thread.channel_id
		ORDER BY COALESCE(st.last_slack_activity_at, j.updated_at, j.created_at) DESC,
		         j.updated_at DESC,
		         j.id DESC
		LIMIT ?`, threadID, limit)
	if err != nil {
		return nil
	}
	return rows
}

func (s *Service) job(jobID int64) (map[string]any, error) {
	rows, err := s.store.queryMaps("SELECT * FROM jobs WHERE id=?", jobID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, sql.ErrNoRows
	}
	return rows[0], nil
}

func (s *Service) threadMessages(jobID int64) []map[string]any {
	rows, err := s.store.queryMaps(`
		SELECT sm.*
		FROM slack_messages sm
		JOIN jobs j ON j.slack_thread_id = sm.slack_thread_id
		WHERE j.id = ?
		ORDER BY sm.slack_message_ts`, jobID)
	if err != nil {
		return nil
	}
	return rows
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

func (s *Service) updateThreadStatusForJob(jobID int64, status string) {
	_, _ = s.store.db.Exec(
		"UPDATE slack_threads SET status=? WHERE id=(SELECT slack_thread_id FROM jobs WHERE id=?)",
		status,
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
		SELECT j.slack_thread_id, st.channel_id, st.thread_ts
		FROM jobs j
		JOIN slack_threads st ON st.id = j.slack_thread_id
		WHERE j.id = ?`, jobID)
	if err != nil || len(rows) == 0 {
		return
	}
	row := rows[0]
	_, _ = s.store.db.Exec(`
		INSERT INTO reply_drafts(
			job_id, slack_thread_id, status, draft_text, edited_text, rationale,
			slack_channel_id, slack_thread_ts, created_at, updated_at
		) VALUES (?, ?, 'draft', ?, NULL, ?, ?, ?, ?, ?)`,
		jobID,
		row["slack_thread_id"],
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
		s.updateThreadStatusForJob(jobID, "archived")
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
		s.updateThreadStatusForJob(jobID, "archived")
	}
}

func analysisPrompt(thread map[string]any, messages []map[string]any, relatedJobs []map[string]any) string {
	return fmt.Sprintf(`Analyze this Slack intake trigger for my personal work agent.

This Slack message/thread is a trigger event. Do not assume it is a standalone task.

Before deciding, use the available Slack context skill/capabilities from the workspace when useful. For DMs especially, inspect nearby messages in the same DM around the trigger timestamp; people often continue the same topic as separate DM messages instead of Slack thread replies. If the trigger belongs to a native Slack thread, inspect the full thread as well.

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

Slack thread:
%v

Messages:
%s

Related jobs from same Slack DM/channel:
%v
`, thread, transcript(messages), relatedJobs)
}

func workerPrompt(job map[string]any, messages []map[string]any) string {
	return fmt.Sprintf(`You are a Codex worker for my personal Slack Agent.

You may use local files, commands, repos, tests, and tools that work inside the local Codex workspace to move this task forward.
Network access and permissions outside the local workspace are intentionally unavailable by default. If they are required, report the blocker instead of waiting for approval.
Before working, use the available Slack context skill/capabilities from the workspace when useful to refresh the latest relevant Slack context. For DM jobs, inspect nearby messages in the same DM around the original trigger and any newer messages in the same conversation; people often continue the same topic as separate DM messages instead of Slack thread replies.
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

Messages:
%s
`, job, transcript(messages))
}

func transcript(messages []map[string]any) string {
	var lines []string
	for _, message := range messages {
		name := fmt.Sprint(message["user_name"])
		if name == "" || name == "<nil>" {
			name = fmt.Sprint(message["user_id"])
		}
		lines = append(lines, fmt.Sprintf("%s: %s", name, message["text"]))
	}
	return strings.Join(lines, "\n")
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
