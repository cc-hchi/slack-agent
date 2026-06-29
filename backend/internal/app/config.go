package app

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Addr                      string
	DatabasePath              string
	WorkspaceRoot             string
	SeedDemoData              bool
	AutoAdvance               bool
	SlackBotToken             string
	SlackUserToken            string
	SlackUserID               string
	SlackUserSearchName       string
	SlackIncludeParticipated  bool
	CodexBin                  string
	CodexPermissionProfile    string
	CodexSandboxMode          string
	CodexApprovalPolicy       string
	WorkspaceBootstrapCommand string
	AnalyzerConcurrency       int
	WorkerConcurrency         int
	SlackSearchPageSize       int
	SlackSearchMaxPages       int
	SlackSearchLookbackDays   int
	SlackDMChannelLimit       int
	SlackDMHistoryPageSize    int
	SlackDMHistoryMaxPages    int
	SlackReplyPageSize        int
	SlackReplyMaxPages        int
}

func LoadConfig() Config {
	loadDotEnv(".env")
	loadDotEnv("../.env")
	cfg := Config{
		Addr:                      env("SLACK_AGENT_ADDR", "127.0.0.1:8787"),
		DatabasePath:              env("SLACK_AGENT_DB_PATH", "data/slack_agent.db"),
		WorkspaceRoot:             env("SLACK_AGENT_WORKSPACE_ROOT", "data/workspaces"),
		SeedDemoData:              envBool("SLACK_AGENT_SEED_DEMO", false),
		AutoAdvance:               envBool("SLACK_AGENT_AUTO_ADVANCE", true),
		SlackBotToken:             os.Getenv("SLACK_BOT_TOKEN"),
		SlackUserToken:            os.Getenv("SLACK_USER_TOKEN"),
		SlackUserID:               os.Getenv("SLACK_USER_ID"),
		SlackUserSearchName:       os.Getenv("SLACK_AGENT_USER_SEARCH_NAME"),
		SlackIncludeParticipated:  envBool("SLACK_AGENT_INCLUDE_USER_PARTICIPATED", false),
		CodexBin:                  env("SLACK_AGENT_CODEX_BIN", "/Applications/Codex.app/Contents/Resources/codex"),
		CodexPermissionProfile:    envAllowEmpty("SLACK_AGENT_CODEX_PERMISSION_PROFILE", ":workspace"),
		CodexSandboxMode:          envAllowEmpty("SLACK_AGENT_CODEX_SANDBOX", "workspace-write"),
		CodexApprovalPolicy:       envAllowEmpty("SLACK_AGENT_CODEX_APPROVAL_POLICY", "never"),
		WorkspaceBootstrapCommand: env("SLACK_AGENT_WORKSPACE_BOOTSTRAP_COMMAND", "install-chi-skills"),
		AnalyzerConcurrency:       envInt("SLACK_AGENT_ANALYZER_CONCURRENCY", 1),
		WorkerConcurrency:         envInt("SLACK_AGENT_WORKER_CONCURRENCY", 2),
		SlackSearchPageSize:       envInt("SLACK_AGENT_SEARCH_PAGE_SIZE", 100),
		SlackSearchMaxPages:       envInt("SLACK_AGENT_SEARCH_MAX_PAGES", 20),
		SlackSearchLookbackDays:   envInt("SLACK_AGENT_SEARCH_LOOKBACK_DAYS", 14),
		SlackDMChannelLimit:       envInt("SLACK_AGENT_DM_CHANNEL_LIMIT", 0),
		SlackDMHistoryPageSize:    envInt("SLACK_AGENT_DM_HISTORY_PAGE_SIZE", 100),
		SlackDMHistoryMaxPages:    envInt("SLACK_AGENT_DM_HISTORY_MAX_PAGES", 10),
		SlackReplyPageSize:        envInt("SLACK_AGENT_REPLY_PAGE_SIZE", 200),
		SlackReplyMaxPages:        envInt("SLACK_AGENT_REPLY_MAX_PAGES", 10),
	}
	cfg.SlackUserID = blankPlaceholder(cfg.SlackUserID, "U1234567890")
	cfg.SlackUserSearchName = blankPlaceholder(cfg.SlackUserSearchName, "your.slack.username")
	cfg.SlackUserToken = blankPlaceholder(cfg.SlackUserToken, "xoxp-your-user-token")
	cfg.SlackBotToken = blankPlaceholder(cfg.SlackBotToken, "xoxb-your-bot-token")
	return cfg
}

func (c Config) SlackToken() string {
	if c.SlackUserToken != "" {
		return c.SlackUserToken
	}
	return c.SlackBotToken
}

func (c Config) ResolvedCodexBin() string {
	if c.CodexBin == "" {
		return ""
	}
	if filepath.IsAbs(c.CodexBin) {
		if _, err := os.Stat(c.CodexBin); err == nil {
			return c.CodexBin
		}
	}
	if path, err := exec.LookPath(c.CodexBin); err == nil {
		return path
	}
	return ""
}

func (c Config) CodexThreadOptions(cwd string) CodexThreadOptions {
	return CodexThreadOptions{
		CWD:               cwd,
		PermissionProfile: strings.TrimSpace(c.CodexPermissionProfile),
		SandboxMode:       strings.TrimSpace(c.CodexSandboxMode),
		ApprovalPolicy:    strings.TrimSpace(c.CodexApprovalPolicy),
	}
}

func (c Config) ResolvedBootstrapCommand() string {
	command := strings.TrimSpace(c.WorkspaceBootstrapCommand)
	if command == "" {
		return ""
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	if filepath.IsAbs(fields[0]) {
		if _, err := os.Stat(fields[0]); err == nil {
			return command
		}
		return ""
	}
	if _, err := exec.LookPath(fields[0]); err == nil {
		return command
	}
	return ""
}

func env(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func envAllowEmpty(key, fallback string) string {
	value, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	return value
}

func envBool(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func blankPlaceholder(value, placeholder string) string {
	if strings.TrimSpace(value) == placeholder {
		return ""
	}
	return value
}

func loadDotEnv(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		key := strings.TrimSpace(parts[0])
		value := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
		if key != "" && os.Getenv(key) == "" {
			_ = os.Setenv(key, value)
		}
	}
}
