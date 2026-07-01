package app

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestAnalysisPromptRequiresChineseUserFacingStrings(t *testing.T) {
	prompt := analysisPrompt(map[string]any{"title": "Need help"}, nil)

	for _, want := range []string{
		`"task_title": "简短中文工作项标题"`,
		"Keep JSON keys exactly as shown",
		"write user-facing string values in Simplified Chinese",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("analysis prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestAnalysisPromptRequiresSlackContextRouting(t *testing.T) {
	prompt := analysisPrompt(map[string]any{"title": "Need help", "source_type": "dm"}, nil)

	for _, want := range []string{
		"trigger event",
		"stores only Slack trigger metadata",
		"Slack context skill",
		"fetch the latest relevant Slack messages",
		"nearby messages in the same DM",
		"Related jobs from the same Slack DM/channel",
		"at most 20 jobs",
		"existing/running job",
		"my own Slack reply",
		"action_required=false when this trigger is only additional context",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("analysis prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestWorkerPromptRequiresSlackContextRefresh(t *testing.T) {
	prompt := workerPrompt(map[string]any{"title": "Need help"})

	for _, want := range []string{
		"stores only Slack trigger metadata",
		"refresh the latest relevant Slack context",
		"channel_id, thread_ts, and permalink",
		"nearby messages in the same DM",
		"already handled by another active job",
		"my own Slack reply after the latest relevant external message",
		"Before drafting a Slack reply, check again",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("worker prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestParseAnalysisOutputStructuredJSON(t *testing.T) {
	result := parseAnalysisOutput(`{
	  "action_required": true,
	  "confidence": 0.82,
	  "summary": "Investigate the Datadog request trace.",
	  "task_title": "Investigate request id",
	  "urgency": "high",
	  "why_it_matters": "The thread asks for a concrete production answer.",
	  "worker_plan": "Search logs by request id and draft a Slack reply.",
	  "needed_user_confirmation": "Review before sending.",
	  "rationale": "The request is actionable and specific."
	}`)

	if !result.ActionRequired {
		t.Fatal("expected action_required=true")
	}
	assertConfidence(t, result.Confidence, 0.82)
	if result.Summary != "Investigate the Datadog request trace." {
		t.Fatalf("unexpected summary: %q", result.Summary)
	}
	if result.TaskTitle != "Investigate request id" || result.Urgency != "high" {
		t.Fatalf("unexpected task metadata: title=%q urgency=%q", result.TaskTitle, result.Urgency)
	}
	if result.Rationale == "" || result.Rationale == "The request is actionable and specific." {
		t.Fatalf("expected rendered structured rationale, got %q", result.Rationale)
	}
}

func TestParseAnalysisOutputFencedJSONWithPercentConfidence(t *testing.T) {
	result := parseAnalysisOutput("```json\n{\"action_required\":false,\"confidence\":\"85%\",\"task_title\":\"No follow-up needed\",\"urgency\":\"low\"}\n```")

	if result.ActionRequired {
		t.Fatal("expected action_required=false")
	}
	assertConfidence(t, result.Confidence, 0.85)
	if result.Summary != "No follow-up needed" {
		t.Fatalf("expected task title fallback summary, got %q", result.Summary)
	}
}

func TestParseAnalysisOutputLegacyKeyValues(t *testing.T) {
	result := parseAnalysisOutput(`NO_ACTION

action_required: false
confidence: medium-low
task_title: Courtesy acknowledgement
urgency: low
why_it_matters: No worker task is needed.`)

	if result.ActionRequired {
		t.Fatal("expected action_required=false")
	}
	assertConfidence(t, result.Confidence, 0.50)
	if result.Summary != "Courtesy acknowledgement" {
		t.Fatalf("unexpected summary: %q", result.Summary)
	}
	if result.Urgency != "low" {
		t.Fatalf("unexpected urgency: %q", result.Urgency)
	}
}

func TestParseAnalysisOutputCommentaryPrefixedKeyValues(t *testing.T) {
	result := parseAnalysisOutput(`I will inspect context first.action_required: yes

confidence: low

task_title: Clarify referenced Slack item

urgency: low

why_it_matters: The captured thread points at missing context.

worker_plan: Inspect the surrounding DM messages and draft a reply.

needed_user_confirmation: Confirm the referenced item before sending.`)

	if !result.ActionRequired {
		t.Fatal("expected action_required=true")
	}
	assertConfidence(t, result.Confidence, 0.35)
	if result.TaskTitle != "Clarify referenced Slack item" {
		t.Fatalf("unexpected task title: %q", result.TaskTitle)
	}
	if result.Summary != "Clarify referenced Slack item" {
		t.Fatalf("expected task title fallback summary, got %q", result.Summary)
	}
}

func TestAnalysisStructuredResultJSONOmitsMissingConfidence(t *testing.T) {
	result := parseAnalysisOutput(`{"action_required":true,"task_title":"Check release risk"}`)
	var payload map[string]any
	if err := json.Unmarshal([]byte(result.structuredResultJSON("raw", nil)), &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["confidence"]; ok {
		t.Fatalf("missing confidence should not be serialized as a fake number: %#v", payload["confidence"])
	}
}

func TestParseAnalysisOutputBareNoAction(t *testing.T) {
	result := parseAnalysisOutput("NO_ACTION")
	if result.ActionRequired {
		t.Fatal("expected bare NO_ACTION to remain supported")
	}
	if result.Summary != "No action needed" {
		t.Fatalf("unexpected summary: %q", result.Summary)
	}
}

func assertConfidence(t *testing.T, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("expected confidence %.2f, got nil", want)
	}
	if math.Abs(*got-want) > 0.0001 {
		t.Fatalf("expected confidence %.2f, got %.4f", want, *got)
	}
}
