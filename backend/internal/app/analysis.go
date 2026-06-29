package app

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type analysisOutcome struct {
	ActionRequired          bool
	Confidence              *float64
	Summary                 string
	Rationale               string
	TaskTitle               string
	Urgency                 string
	WhyItMatters            string
	WorkerPlan              string
	NeededUserConfirmation  string
	parsedStructuredPayload map[string]any
}

func parseAnalysisOutput(text string) analysisOutcome {
	raw := strings.TrimSpace(text)
	outcome := analysisOutcome{
		ActionRequired: !containsNoAction(raw),
		Summary:        cleanAnalysisSummary(raw),
		Rationale:      raw,
	}
	if fields := parseAnalysisJSON(raw); fields != nil {
		outcome.parsedStructuredPayload = fields
		applyAnalysisFields(&outcome, fields)
		outcome.finalize(raw)
		return outcome
	}
	fields := parseAnalysisKeyValues(raw)
	if len(fields) > 0 {
		outcome.parsedStructuredPayload = fields
		applyAnalysisFields(&outcome, fields)
		outcome.finalize(raw)
	}
	outcome.finalize(raw)
	return outcome
}

func (o analysisOutcome) confidenceValue() any {
	if o.Confidence == nil {
		return nil
	}
	return *o.Confidence
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (o analysisOutcome) structuredResultJSON(rawFinalText string, events []map[string]any) string {
	payload := map[string]any{
		"action_required":          o.ActionRequired,
		"summary":                  o.Summary,
		"rationale":                o.Rationale,
		"task_title":               o.TaskTitle,
		"urgency":                  o.Urgency,
		"why_it_matters":           o.WhyItMatters,
		"worker_plan":              o.WorkerPlan,
		"needed_user_confirmation": o.NeededUserConfirmation,
		"raw_final_text":           rawFinalText,
		"events":                   events,
	}
	if o.Confidence != nil {
		payload["confidence"] = *o.Confidence
	}
	if o.parsedStructuredPayload != nil {
		payload["parsed"] = o.parsedStructuredPayload
	}
	return mustJSON(payload)
}

func (o *analysisOutcome) finalize(raw string) {
	if o.Summary == "" || o.Summary == raw {
		switch {
		case o.TaskTitle != "":
			o.Summary = o.TaskTitle
		case o.WhyItMatters != "":
			o.Summary = o.WhyItMatters
		case !o.ActionRequired:
			o.Summary = "No action needed"
		default:
			o.Summary = cleanAnalysisSummary(raw)
		}
	}
	if hasStructuredAnalysisFields(*o) {
		o.Rationale = renderAnalysisRationale(*o)
	}
}

func applyAnalysisFields(outcome *analysisOutcome, fields map[string]any) {
	if actionRequired, ok := parseActionRequired(fields["action_required"]); ok {
		outcome.ActionRequired = actionRequired
	}
	if confidence, ok := parseConfidence(fields["confidence"]); ok {
		outcome.Confidence = &confidence
	}
	outcome.Summary = firstAnalysisString(fields, "summary", "analyzer_summary")
	outcome.Rationale = firstAnalysisString(fields, "rationale", "reasoning", "analysis")
	outcome.TaskTitle = firstAnalysisString(fields, "task_title", "title")
	outcome.Urgency = normalizeUrgency(firstAnalysisString(fields, "urgency", "priority"))
	outcome.WhyItMatters = firstAnalysisString(fields, "why_it_matters", "why")
	outcome.WorkerPlan = firstAnalysisString(fields, "worker_plan", "plan")
	outcome.NeededUserConfirmation = firstAnalysisString(fields, "needed_user_confirmation", "needs_user_confirmation", "user_confirmation")
}

func parseAnalysisJSON(text string) map[string]any {
	for _, candidate := range jsonCandidates(text) {
		var fields map[string]any
		if err := json.Unmarshal([]byte(candidate), &fields); err == nil && len(fields) > 0 {
			return normalizeAnalysisFieldMap(fields)
		}
	}
	return nil
}

func jsonCandidates(text string) []string {
	trimmed := strings.TrimSpace(text)
	candidates := []string{trimmed, stripMarkdownFence(trimmed)}
	if strings.HasPrefix(trimmed, "```") {
		lines := strings.Split(trimmed, "\n")
		if len(lines) >= 3 {
			candidates = append(candidates, strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n")))
		}
	}
	first := strings.Index(trimmed, "{")
	last := strings.LastIndex(trimmed, "}")
	if first >= 0 && last > first {
		candidates = append(candidates, strings.TrimSpace(trimmed[first:last+1]))
	}
	seen := map[string]bool{}
	unique := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		unique = append(unique, candidate)
	}
	return unique
}

func parseAnalysisKeyValues(text string) map[string]any {
	fields := map[string]any{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "-* \t")
		if line == "" {
			continue
		}
		key, value, ok := splitAnalysisKeyValue(line)
		if !ok {
			continue
		}
		fields[key] = strings.TrimSpace(value)
	}
	return fields
}

func splitAnalysisKeyValue(line string) (string, string, bool) {
	key, value, ok := strings.Cut(line, ":")
	if !ok {
		return "", "", false
	}
	key = normalizeAnalysisKey(key)
	if isKnownAnalysisKey(key) {
		return key, value, true
	}
	lower := strings.ToLower(line)
	bestStart := -1
	bestAlias := analysisKeyAlias{}
	for _, alias := range analysisKeyAliases {
		marker := alias.Alias + ":"
		if idx := strings.LastIndex(lower, marker); idx >= 0 && idx > bestStart {
			bestStart = idx
			bestAlias = alias
		}
	}
	if bestStart < 0 {
		return "", "", false
	}
	valueStart := bestStart + len(bestAlias.Alias) + 1
	return bestAlias.Key, line[valueStart:], true
}

type analysisKeyAlias struct {
	Alias string
	Key   string
}

var analysisKeyAliases = []analysisKeyAlias{
	{"action_required", "action_required"},
	{"action required", "action_required"},
	{"needed_user_confirmation", "needed_user_confirmation"},
	{"needs_user_confirmation", "needs_user_confirmation"},
	{"user_confirmation", "user_confirmation"},
	{"why_it_matters", "why_it_matters"},
	{"worker_plan", "worker_plan"},
	{"analyzer_summary", "analyzer_summary"},
	{"task_title", "task_title"},
	{"confidence", "confidence"},
	{"rationale", "rationale"},
	{"reasoning", "reasoning"},
	{"analysis", "analysis"},
	{"summary", "summary"},
	{"urgency", "urgency"},
	{"priority", "priority"},
	{"title", "title"},
	{"plan", "plan"},
	{"why", "why"},
}

var knownAnalysisKeys = map[string]bool{
	"action_required":          true,
	"confidence":               true,
	"summary":                  true,
	"analyzer_summary":         true,
	"rationale":                true,
	"reasoning":                true,
	"analysis":                 true,
	"task_title":               true,
	"title":                    true,
	"urgency":                  true,
	"priority":                 true,
	"why_it_matters":           true,
	"why":                      true,
	"worker_plan":              true,
	"plan":                     true,
	"needed_user_confirmation": true,
	"needs_user_confirmation":  true,
	"user_confirmation":        true,
}

func isKnownAnalysisKey(key string) bool {
	return knownAnalysisKeys[key]
}

func normalizeAnalysisFieldMap(fields map[string]any) map[string]any {
	normalized := map[string]any{}
	for key, value := range fields {
		normalized[normalizeAnalysisKey(key)] = value
	}
	return normalized
}

func normalizeAnalysisKey(key string) string {
	key = strings.TrimSpace(strings.ToLower(key))
	key = strings.Trim(key, "`*_ ")
	key = strings.NewReplacer("-", "_", " ", "_").Replace(key)
	return key
}

func parseActionRequired(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case float64:
		return typed != 0, true
	case string:
		normalized := strings.ToLower(strings.TrimSpace(typed))
		switch normalized {
		case "true", "yes", "y", "required", "action_required", "action required", "1":
			return true, true
		case "false", "no", "n", "none", "no_action", "no action", "not required", "0":
			return false, true
		}
		if strings.Contains(normalized, "no action") || strings.Contains(normalized, "not required") {
			return false, true
		}
		if strings.Contains(normalized, "action") || strings.Contains(normalized, "required") {
			return true, true
		}
	}
	return false, false
}

func parseConfidence(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return normalizeConfidenceNumber(typed)
	case string:
		return parseConfidenceString(typed)
	case json.Number:
		number, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return normalizeConfidenceNumber(number)
	}
	return 0, false
}

func parseConfidenceString(value string) (float64, bool) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = strings.Trim(normalized, "`*_ ")
	if normalized == "" {
		return 0, false
	}
	if strings.HasSuffix(normalized, "%") {
		percent, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(normalized, "%")), 64)
		if err == nil {
			return normalizeConfidenceNumber(percent)
		}
	}
	if number, err := strconv.ParseFloat(normalized, 64); err == nil {
		return normalizeConfidenceNumber(number)
	}
	normalized = strings.NewReplacer("_", " ", "-", " ").Replace(normalized)
	switch {
	case strings.Contains(normalized, "very high"):
		return 0.95, true
	case strings.Contains(normalized, "medium high"):
		return 0.75, true
	case strings.Contains(normalized, "high"):
		return 0.85, true
	case strings.Contains(normalized, "medium low"):
		return 0.50, true
	case strings.Contains(normalized, "very low"):
		return 0.20, true
	case strings.Contains(normalized, "low"):
		return 0.35, true
	case strings.Contains(normalized, "medium"):
		return 0.65, true
	}
	return 0, false
}

func normalizeConfidenceNumber(number float64) (float64, bool) {
	if number < 0 {
		return 0, false
	}
	if number > 1 && number <= 100 {
		number = number / 100
	}
	if number > 1 {
		return 0, false
	}
	return number, true
}

func firstAnalysisString(fields map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := fields[key]; ok {
			text := strings.TrimSpace(fmt.Sprint(value))
			if text != "" && text != "<nil>" {
				return text
			}
		}
	}
	return ""
}

func normalizeUrgency(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "urgent", "high", "medium", "low":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return strings.TrimSpace(value)
	}
}

func hasStructuredAnalysisFields(outcome analysisOutcome) bool {
	return outcome.TaskTitle != "" ||
		outcome.Urgency != "" ||
		outcome.WhyItMatters != "" ||
		outcome.WorkerPlan != "" ||
		outcome.NeededUserConfirmation != "" ||
		outcome.Confidence != nil
}

func renderAnalysisRationale(outcome analysisOutcome) string {
	lines := []string{fmt.Sprintf("action_required: %t", outcome.ActionRequired)}
	if outcome.Confidence != nil {
		lines = append(lines, fmt.Sprintf("confidence: %.2f", *outcome.Confidence))
	}
	appendField := func(key, value string) {
		if strings.TrimSpace(value) != "" {
			lines = append(lines, fmt.Sprintf("%s: %s", key, strings.TrimSpace(value)))
		}
	}
	appendField("task_title", outcome.TaskTitle)
	appendField("urgency", outcome.Urgency)
	appendField("why_it_matters", outcome.WhyItMatters)
	appendField("worker_plan", outcome.WorkerPlan)
	appendField("needed_user_confirmation", outcome.NeededUserConfirmation)
	appendField("rationale", outcome.Rationale)
	return strings.Join(lines, "\n")
}

func cleanAnalysisSummary(text string) string {
	text = stripMarkdownFence(strings.TrimSpace(text))
	if text == "" {
		return ""
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.EqualFold(line, "NO_ACTION") {
			return line
		}
	}
	return text
}

func containsNoAction(text string) bool {
	upper := strings.ToUpper(text)
	return strings.Contains(upper, "NO_ACTION") || strings.Contains(upper, `"ACTION_REQUIRED": FALSE`)
}
