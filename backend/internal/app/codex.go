package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type CodexClient struct {
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	pending       map[int]chan map[string]any
	notifications chan map[string]any
	readErrors    chan error
	stderr        []string
	nextID        int
	mu            sync.Mutex
}

type CodexTurnResult struct {
	ThreadID  string           `json:"thread_id,omitempty"`
	TurnID    string           `json:"turn_id,omitempty"`
	FinalText string           `json:"final_text"`
	Events    []map[string]any `json:"events"`
	Error     string           `json:"error,omitempty"`
}

type CodexThreadOptions struct {
	CWD               string
	PermissionProfile string
	SandboxMode       string
	ApprovalPolicy    string
}

func NewCodexClient(codexBin string) (*CodexClient, error) {
	if codexBin == "" {
		return nil, fmt.Errorf("Codex binary was not found")
	}
	cmd := exec.Command(codexBin, "app-server")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	client := &CodexClient{
		cmd:           cmd,
		stdin:         stdin,
		pending:       map[int]chan map[string]any{},
		notifications: make(chan map[string]any, 1000),
		readErrors:    make(chan error, 1),
		nextID:        1,
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go client.readStdout(stdout)
	go client.readStderr(stderr)
	return client, nil
}

func (c *CodexClient) Close() {
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
}

func (c *CodexClient) Initialize() (map[string]any, error) {
	result, err := c.Request("initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "slack_agent",
			"title":   "Local Slack Agent",
			"version": "0.1.0",
		},
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
	}, 10*time.Second)
	if err != nil {
		return nil, err
	}
	if err := c.Notify("initialized", map[string]any{}); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *CodexClient) StartThread(options CodexThreadOptions) (string, error) {
	result, err := c.Request("thread/start", options.params(), 20*time.Second)
	if err != nil {
		return "", err
	}
	thread, _ := result["thread"].(map[string]any)
	threadID, _ := thread["id"].(string)
	if threadID == "" {
		threadID, _ = result["threadId"].(string)
	}
	if threadID == "" {
		threadID, _ = result["sessionId"].(string)
	}
	if threadID == "" {
		return "", fmt.Errorf("thread/start returned no thread id")
	}
	return threadID, nil
}

func (o CodexThreadOptions) params() map[string]any {
	params := map[string]any{"cwd": o.CWD}
	if o.PermissionProfile != "" {
		params["permissions"] = o.PermissionProfile
	} else if o.SandboxMode != "" {
		params["sandbox"] = o.SandboxMode
	}
	if o.ApprovalPolicy != "" {
		params["approvalPolicy"] = o.ApprovalPolicy
	}
	return params
}

func (c *CodexClient) RunTurn(threadID, prompt string, timeout time.Duration) CodexTurnResult {
	result, err := c.Request("turn/start", map[string]any{
		"threadId": threadID,
		"input": []map[string]any{
			{"type": "text", "text": prompt},
		},
	}, 20*time.Second)
	if err != nil {
		return CodexTurnResult{ThreadID: threadID, Error: err.Error()}
	}
	turn, _ := result["turn"].(map[string]any)
	turnID, _ := turn["id"].(string)
	deadline := time.After(timeout)
	var events []map[string]any
	messages := newTurnMessageCollector()
	for {
		select {
		case msg := <-c.notifications:
			events = append(events, msg)
			method, _ := msg["method"].(string)
			messages.ingest(msg)
			if method == "turn/completed" {
				return CodexTurnResult{ThreadID: threadID, TurnID: turnID, FinalText: messages.finalText(), Events: events}
			}
		case err := <-c.readErrors:
			return CodexTurnResult{ThreadID: threadID, TurnID: turnID, FinalText: messages.finalText(), Events: events, Error: err.Error()}
		case <-deadline:
			return CodexTurnResult{ThreadID: threadID, TurnID: turnID, FinalText: messages.finalText(), Events: events, Error: "turn timed out"}
		}
	}
}

type turnMessageCollector struct {
	order  []string
	seen   map[string]bool
	phase  map[string]string
	text   map[string]string
	kind   map[string]string
	anonID int
}

func newTurnMessageCollector() *turnMessageCollector {
	return &turnMessageCollector{
		seen:  map[string]bool{},
		phase: map[string]string{},
		text:  map[string]string{},
		kind:  map[string]string{},
	}
}

func (c *turnMessageCollector) ingest(msg map[string]any) {
	method, _ := msg["method"].(string)
	params, _ := msg["params"].(map[string]any)
	switch method {
	case "item/started", "item/completed":
		item, _ := params["item"].(map[string]any)
		id := stringValue(item["id"])
		if id == "" {
			return
		}
		c.remember(id)
		if phase := stringValue(item["phase"]); phase != "" {
			c.phase[id] = phase
		}
		if kind := stringValue(item["type"]); kind != "" {
			c.kind[id] = kind
		}
		if text := stringValue(item["text"]); text != "" {
			c.text[id] = text
		}
	case "item/agentMessage/delta":
		id := stringValue(params["itemId"])
		if id == "" {
			c.anonID++
			id = fmt.Sprintf("agent-message-%d", c.anonID)
		}
		c.remember(id)
		c.kind[id] = "agentMessage"
		if delta := stringValue(params["delta"]); delta != "" {
			c.text[id] += delta
		}
	}
}

func (c *turnMessageCollector) finalText() string {
	if text := c.join(func(id string) bool {
		return c.phase[id] == "final_answer"
	}); strings.TrimSpace(text) != "" {
		return text
	}
	return c.join(func(id string) bool {
		return c.kind[id] == "" || c.kind[id] == "agentMessage"
	})
}

func (c *turnMessageCollector) remember(id string) {
	if c.seen[id] {
		return
	}
	c.seen[id] = true
	c.order = append(c.order, id)
}

func (c *turnMessageCollector) join(include func(string) bool) string {
	var parts []string
	for _, id := range c.order {
		if !include(id) {
			continue
		}
		if text := c.text[id]; text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "")
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func (c *CodexClient) Request(method string, params map[string]any, timeout time.Duration) (map[string]any, error) {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	ch := make(chan map[string]any, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case msg := <-ch:
		if rawErr, ok := msg["error"]; ok {
			return nil, fmt.Errorf("%v", rawErr)
		}
		result, _ := msg["result"].(map[string]any)
		return result, nil
	case err := <-c.readErrors:
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	case <-time.After(timeout):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("%s timed out", method)
	}
}

func (c *CodexClient) Notify(method string, params map[string]any) error {
	return c.write(map[string]any{"method": method, "params": params})
}

func (c *CodexClient) write(payload map[string]any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = c.stdin.Write(append(data, '\n'))
	return err
}

func (c *CodexClient) readStdout(reader io.Reader) {
	bufReader := bufio.NewReader(reader)
	for {
		line, err := bufReader.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			if err != io.EOF {
				c.reportReadError(fmt.Errorf("codex app-server stdout read failed: %w", err))
			}
			return
		}
		var msg map[string]any
		if unmarshalErr := json.Unmarshal(line, &msg); unmarshalErr == nil {
			if idFloat, ok := msg["id"].(float64); ok {
				id := int(idFloat)
				c.mu.Lock()
				ch := c.pending[id]
				delete(c.pending, id)
				c.mu.Unlock()
				if ch != nil {
					ch <- msg
					if err != nil {
						if err != io.EOF {
							c.reportReadError(fmt.Errorf("codex app-server stdout read failed: %w", err))
						}
						return
					}
					continue
				}
			}
			c.notifications <- msg
		}
		if err != nil {
			if err != io.EOF {
				c.reportReadError(fmt.Errorf("codex app-server stdout read failed: %w", err))
			}
			return
		}
	}
}

func (c *CodexClient) reportReadError(err error) {
	select {
	case c.readErrors <- err:
	default:
	}
}

func (c *CodexClient) readStderr(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		c.mu.Lock()
		c.stderr = append(c.stderr, scanner.Text())
		if len(c.stderr) > 40 {
			c.stderr = c.stderr[len(c.stderr)-40:]
		}
		c.mu.Unlock()
	}
}
