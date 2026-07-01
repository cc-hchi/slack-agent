package app

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCodexThreadOptionsPreferPermissionProfile(t *testing.T) {
	params := CodexThreadOptions{
		CWD:               "/tmp/slack-agent/job-1",
		PermissionProfile: ":workspace",
		SandboxMode:       "danger-full-access",
		ApprovalPolicy:    "never",
	}.params()

	if params["cwd"] != "/tmp/slack-agent/job-1" {
		t.Fatalf("unexpected cwd: %#v", params["cwd"])
	}
	if params["permissions"] != ":workspace" {
		t.Fatalf("expected workspace permission profile, got %#v", params["permissions"])
	}
	if _, ok := params["sandbox"]; ok {
		t.Fatalf("permissions and sandbox cannot be combined: %#v", params)
	}
	if params["approvalPolicy"] != "never" {
		t.Fatalf("expected approvalPolicy=never, got %#v", params["approvalPolicy"])
	}
}

func TestConfigCodexThreadOptionsFallbackToSandbox(t *testing.T) {
	cfg := Config{
		CodexPermissionProfile: " ",
		CodexSandboxMode:       " workspace-write ",
		CodexApprovalPolicy:    " never ",
	}

	params := cfg.CodexThreadOptions("/tmp/slack-agent/job-2").params()

	if _, ok := params["permissions"]; ok {
		t.Fatalf("blank permission profile should not be sent: %#v", params)
	}
	if params["sandbox"] != "workspace-write" {
		t.Fatalf("expected sandbox fallback, got %#v", params["sandbox"])
	}
	if params["approvalPolicy"] != "never" {
		t.Fatalf("expected trimmed approval policy, got %#v", params["approvalPolicy"])
	}
}

func TestCodexThreadRefFromStartResultKeepsSessionID(t *testing.T) {
	ref, err := codexThreadRefFromStartResult(map[string]any{
		"thread": map[string]any{
			"id":        "thread-1",
			"sessionId": "session-1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ref.ThreadID != "thread-1" || ref.SessionID != "session-1" {
		t.Fatalf("unexpected thread ref: %#v", ref)
	}

	ref, err = codexThreadRefFromStartResult(map[string]any{"sessionId": "session-only"})
	if err != nil {
		t.Fatal(err)
	}
	if ref.ThreadID != "session-only" || ref.SessionID != "session-only" {
		t.Fatalf("expected session fallback, got %#v", ref)
	}
}

func TestReadStdoutHandlesLargeNotificationLines(t *testing.T) {
	largeText := strings.Repeat("x", 90*1024)
	notification := map[string]any{
		"method": "item/started",
		"params": map[string]any{"text": largeText},
	}
	response := map[string]any{
		"id":     1,
		"result": map[string]any{"ok": true},
	}
	notificationData, err := json.Marshal(notification)
	if err != nil {
		t.Fatal(err)
	}
	responseData, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}

	responseCh := make(chan map[string]any, 1)
	client := &CodexClient{
		pending:       map[int]chan map[string]any{1: responseCh},
		notifications: make(chan map[string]any, 1),
		readErrors:    make(chan error, 1),
	}

	client.readStdout(strings.NewReader(string(notificationData) + "\n" + string(responseData) + "\n"))

	select {
	case got := <-client.notifications:
		if got["method"] != "item/started" {
			t.Fatalf("unexpected notification: %#v", got)
		}
		params, _ := got["params"].(map[string]any)
		if params["text"] != largeText {
			t.Fatalf("large notification payload was not preserved")
		}
	default:
		t.Fatal("expected large notification to be delivered")
	}

	select {
	case got := <-responseCh:
		if got["id"] != float64(1) {
			t.Fatalf("unexpected response: %#v", got)
		}
	default:
		t.Fatal("expected response after large notification to be delivered")
	}

	select {
	case err := <-client.readErrors:
		t.Fatalf("unexpected read error: %v", err)
	default:
	}
}

func TestTurnMessageCollectorPrefersFinalAnswer(t *testing.T) {
	collector := newTurnMessageCollector()
	collector.ingest(map[string]any{
		"method": "item/started",
		"params": map[string]any{"item": map[string]any{
			"id":    "commentary-1",
			"type":  "agentMessage",
			"phase": "commentary",
		}},
	})
	collector.ingest(map[string]any{
		"method": "item/agentMessage/delta",
		"params": map[string]any{"itemId": "commentary-1", "delta": "I will inspect context first."},
	})
	collector.ingest(map[string]any{
		"method": "item/started",
		"params": map[string]any{"item": map[string]any{
			"id":    "final-1",
			"type":  "agentMessage",
			"phase": "final_answer",
		}},
	})
	collector.ingest(map[string]any{
		"method": "item/agentMessage/delta",
		"params": map[string]any{"itemId": "final-1", "delta": `{"action_required":true`},
	})
	collector.ingest(map[string]any{
		"method": "item/agentMessage/delta",
		"params": map[string]any{"itemId": "final-1", "delta": `,"confidence":0.91}`},
	})

	if got := collector.finalText(); got != `{"action_required":true,"confidence":0.91}` {
		t.Fatalf("unexpected final text: %q", got)
	}
}

func TestTurnMessageCollectorFallsBackWhenPhaseMissing(t *testing.T) {
	collector := newTurnMessageCollector()
	collector.ingest(map[string]any{
		"method": "item/agentMessage/delta",
		"params": map[string]any{"itemId": "message-1", "delta": "legacy "},
	})
	collector.ingest(map[string]any{
		"method": "item/agentMessage/delta",
		"params": map[string]any{"itemId": "message-1", "delta": "text"},
	})

	if got := collector.finalText(); got != "legacy text" {
		t.Fatalf("unexpected fallback text: %q", got)
	}
}
