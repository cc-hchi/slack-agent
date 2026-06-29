package app

import (
	"database/sql"
	"encoding/json"
	"strings"
)

type AnalysisRepairResult struct {
	AnalysisRuns int
	Jobs         int
	Skipped      int
}

func (s *Store) RepairAnalysisStructuredOutputs() (AnalysisRepairResult, error) {
	rows, err := s.db.Query(`
		SELECT id, structured_result_json
		FROM analysis_runs
		WHERE structured_result_json IS NOT NULL
		  AND json_extract(structured_result_json, '$.raw_final_text') IS NOT NULL
		  AND (
		    confidence = 0.7
		    OR summary LIKE '%action_required:%'
		    OR rationale LIKE '%action_required:%'
		  )
		ORDER BY id`)
	if err != nil {
		return AnalysisRepairResult{}, err
	}
	defer rows.Close()

	type candidate struct {
		id     int64
		raw    string
		events []map[string]any
	}
	var candidates []candidate
	var skipped int
	for rows.Next() {
		var id int64
		var structured sql.NullString
		if err := rows.Scan(&id, &structured); err != nil {
			return AnalysisRepairResult{}, err
		}
		if !structured.Valid {
			skipped++
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(structured.String), &payload); err != nil {
			skipped++
			continue
		}
		raw, _ := payload["raw_final_text"].(string)
		if strings.TrimSpace(raw) == "" {
			skipped++
			continue
		}
		candidates = append(candidates, candidate{
			id:     id,
			raw:    raw,
			events: repairEvents(payload["events"]),
		})
	}
	if err := rows.Err(); err != nil {
		return AnalysisRepairResult{}, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return AnalysisRepairResult{}, err
	}
	defer tx.Rollback()

	var repaired AnalysisRepairResult
	repaired.Skipped = skipped
	for _, candidate := range candidates {
		analysis := parseAnalysisOutput(candidate.raw)
		if _, err := tx.Exec(`
			UPDATE analysis_runs
			SET action_required=?, confidence=?, summary=?, rationale=?, structured_result_json=?
			WHERE id=?`,
			boolToInt(analysis.ActionRequired),
			analysis.confidenceValue(),
			truncate(analysis.Summary, 800),
			truncate(analysis.Rationale, 800),
			analysis.structuredResultJSON(candidate.raw, candidate.events),
			candidate.id,
		); err != nil {
			return AnalysisRepairResult{}, err
		}
		repaired.AnalysisRuns++

		title := strings.TrimSpace(analysis.TaskTitle)
		if title == "" {
			continue
		}
		result, err := tx.Exec(`
			UPDATE jobs
			SET title=?
			WHERE analysis_run_id=?
			  AND title<>?
			  AND (
			    title=''
			    OR title LIKE 'Slack thread %'
			    OR EXISTS (
			      SELECT 1
			      FROM slack_threads st
			      WHERE st.id=jobs.slack_thread_id
			        AND jobs.title=st.title
			    )
			  )`,
			title,
			candidate.id,
			title,
		)
		if err != nil {
			return AnalysisRepairResult{}, err
		}
		if affected, err := result.RowsAffected(); err == nil {
			repaired.Jobs += int(affected)
		}
	}
	if err := tx.Commit(); err != nil {
		return AnalysisRepairResult{}, err
	}
	return repaired, nil
}

func repairEvents(value any) []map[string]any {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	events := make([]map[string]any, 0, len(items))
	for _, item := range items {
		event, ok := item.(map[string]any)
		if ok {
			events = append(events, event)
		}
	}
	return events
}
