package app

type HealthCheck struct {
	Service   string `json:"service"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
}

type DashboardSnapshot struct {
	ReplyDrafts []map[string]any `json:"reply_drafts"`
	Blocked     []map[string]any `json:"blocked"`
	Queued      []map[string]any `json:"queued"`
	Active      []map[string]any `json:"active"`
	Intake      []map[string]any `json:"intake"`
	Archive     []map[string]any `json:"archive"`
	Threads     []map[string]any `json:"threads"`
	Jobs        []map[string]any `json:"jobs"`
	Events      []map[string]any `json:"events"`
	Health      []map[string]any `json:"health"`
	Counts      map[string]int   `json:"counts"`
}
