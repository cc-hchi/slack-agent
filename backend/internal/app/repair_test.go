package app

import "testing"

func TestRepairAnalysisStructuredOutputsBackfillsLegacyRuns(t *testing.T) {
	_, store := testService(t)
	now := utcNow()
	if _, err := store.db.Exec(`
		INSERT INTO intake_items(
			slack_team_id, channel_id, trigger_ts, thread_ts, source_type, status, title
		) VALUES ('T1', 'D1', '1782114253.555859', '1782114253.555859', 'dm', 'resolved', '这个是不是也要的 ↑')`); err != nil {
		t.Fatal(err)
	}
	raw := `I will inspect context first.action_required: yes

confidence: low

task_title: Clarify referenced Slack item

urgency: low

why_it_matters: The captured thread points at missing context.

worker_plan: Inspect surrounding DM messages and draft a reply.

needed_user_confirmation: Confirm before sending.`
	if _, err := store.db.Exec(`
		INSERT INTO analysis_runs(
			intake_item_id, status, action_required, confidence, summary, rationale,
			structured_result_json, started_at, completed_at
		) VALUES (1, 'completed', 1, 0.7, ?, ?, ?, ?, ?)`,
		raw,
		raw,
		mustJSON(map[string]any{
			"raw_final_text": raw,
			"events": []map[string]any{
				{"method": "turn/completed"},
			},
		}),
		now,
		now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`
		INSERT INTO jobs(
			intake_item_id, analysis_run_id, title, status, urgency, task_type, created_at, updated_at
		) VALUES (1, 1, '这个是不是也要的 ↑', 'queued', 'medium', 'slack_task', ?, ?)`,
		now,
		now,
	); err != nil {
		t.Fatal(err)
	}

	result, err := store.RepairAnalysisStructuredOutputs()
	if err != nil {
		t.Fatal(err)
	}
	if result.AnalysisRuns != 1 || result.Jobs != 1 {
		t.Fatalf("unexpected repair result: %#v", result)
	}

	var confidence float64
	var summary, title string
	if err := store.db.QueryRow(`
		SELECT ar.confidence, ar.summary, j.title
		FROM analysis_runs ar
		JOIN jobs j ON j.analysis_run_id=ar.id
		WHERE ar.id=1`,
	).Scan(&confidence, &summary, &title); err != nil {
		t.Fatal(err)
	}
	if confidence != 0.35 {
		t.Fatalf("expected low confidence to be normalized to 0.35, got %.2f", confidence)
	}
	if summary != "Clarify referenced Slack item" || title != "Clarify referenced Slack item" {
		t.Fatalf("expected structured task title backfill, summary=%q title=%q", summary, title)
	}
}
