package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type SlackClient struct {
	token string
	http  *http.Client
}

type SlackSearchPage struct {
	Page       int
	PageCount  int
	TotalCount int
}

func NewSlackClient(cfg Config) *SlackClient {
	return &SlackClient{
		token: cfg.SlackToken(),
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *SlackClient) API(method string, values url.Values) (map[string]any, error) {
	if c.token == "" {
		return nil, fmt.Errorf("Slack token is not configured")
	}
	if values == nil {
		values = url.Values{}
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequest(http.MethodPost, "https://slack.com/api/"+method, bytes.NewBufferString(values.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt == 0 {
			waitSeconds, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			_ = resp.Body.Close()
			if waitSeconds < 1 {
				waitSeconds = 1
			}
			time.Sleep(time.Duration(waitSeconds) * time.Second)
			continue
		}
		defer resp.Body.Close()
		var payload map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			return nil, err
		}
		if ok, _ := payload["ok"].(bool); !ok {
			return nil, fmt.Errorf("%s failed: %v", method, payload["error"])
		}
		return payload, nil
	}
	return nil, fmt.Errorf("%s failed: rate limited", method)
}

func (c *SlackClient) AuthTest() (map[string]any, error) {
	return c.API("auth.test", nil)
}

func (c *SlackClient) UsersInfo(userID string) (map[string]any, error) {
	return c.API("users.info", url.Values{"user": {userID}})
}

func (c *SlackClient) ChatPostMessage(channel, threadTS, text string) (map[string]any, error) {
	return c.API("chat.postMessage", url.Values{
		"channel":   {channel},
		"thread_ts": {threadTS},
		"text":      {text},
	})
}

func (c *SlackClient) SearchMessages(query string, count, page int) ([]map[string]any, SlackSearchPage, error) {
	payload, err := c.API("search.messages", url.Values{
		"query":    {query},
		"sort":     {"timestamp"},
		"sort_dir": {"desc"},
		"count":    {fmt.Sprintf("%d", count)},
		"page":     {fmt.Sprintf("%d", page)},
	})
	if err != nil {
		return nil, SlackSearchPage{}, err
	}
	messages, _ := payload["messages"].(map[string]any)
	matches, _ := messages["matches"].([]any)
	pagination, _ := messages["pagination"].(map[string]any)
	pageInfo := SlackSearchPage{
		Page:       numberAsInt(pagination["page"]),
		PageCount:  numberAsInt(pagination["page_count"]),
		TotalCount: numberAsInt(pagination["total_count"]),
	}
	return toMapSlice(matches), pageInfo, nil
}

func (c *SlackClient) ConversationsReplies(channel, ts string, limit, maxPages int) ([]map[string]any, error) {
	var messages []map[string]any
	cursor := ""
	for page := 0; maxPages <= 0 || page < maxPages; page++ {
		values := url.Values{
			"channel": {channel},
			"ts":      {ts},
			"limit":   {fmt.Sprintf("%d", limit)},
		}
		if cursor != "" {
			values.Set("cursor", cursor)
		}
		payload, err := c.API("conversations.replies", values)
		if err != nil {
			return nil, err
		}
		messages = append(messages, toMapSlice(payload["messages"])...)
		cursor = nextCursor(payload)
		if cursor == "" {
			return messages, nil
		}
	}
	return messages, nil
}

func (c *SlackClient) ConversationsList(types string) ([]map[string]any, error) {
	var channels []map[string]any
	cursor := ""
	for {
		values := url.Values{"types": {types}, "limit": {"200"}}
		if cursor != "" {
			values.Set("cursor", cursor)
		}
		payload, err := c.API("conversations.list", values)
		if err != nil {
			return nil, err
		}
		channels = append(channels, toMapSlice(payload["channels"])...)
		meta, _ := payload["response_metadata"].(map[string]any)
		cursor, _ = meta["next_cursor"].(string)
		if cursor == "" {
			return channels, nil
		}
	}
}

func (c *SlackClient) ConversationsHistory(channel string, limit, maxPages int, oldest string) ([]map[string]any, error) {
	var messages []map[string]any
	cursor := ""
	for page := 0; maxPages <= 0 || page < maxPages; page++ {
		values := url.Values{
			"channel": {channel},
			"limit":   {fmt.Sprintf("%d", limit)},
		}
		if oldest != "" {
			values.Set("oldest", oldest)
		}
		if cursor != "" {
			values.Set("cursor", cursor)
		}
		payload, err := c.API("conversations.history", values)
		if err != nil {
			return nil, err
		}
		messages = append(messages, toMapSlice(payload["messages"])...)
		cursor = nextCursor(payload)
		if cursor == "" {
			return messages, nil
		}
	}
	return messages, nil
}

func nextCursor(payload map[string]any) string {
	meta, _ := payload["response_metadata"].(map[string]any)
	cursor, _ := meta["next_cursor"].(string)
	return cursor
}

func toMapSlice(value any) []map[string]any {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if mapped, ok := item.(map[string]any); ok {
			result = append(result, mapped)
		}
	}
	return result
}

func numberAsInt(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		result, _ := typed.Int64()
		return int(result)
	default:
		return 0
	}
}
