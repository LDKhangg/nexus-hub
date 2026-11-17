package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ClickUpTask is the task context of an instruction (W10).
type ClickUpTask struct {
	ID          string
	Name        string
	URL         string
	Status      string
	Description string
	Comments    []string // most recent last
}

// TaskFetcher reads one ClickUp task. Tests use a fake or point
// ClickUpClient at an httptest server.
type TaskFetcher interface {
	FetchTask(ctx context.Context, taskID string) (ClickUpTask, error)
}

// ClickUpClient reads tasks with the laptop's read-only token. The token
// is read from its file per call and never logged.
type ClickUpClient struct {
	BaseURL   string
	TokenFile string
	HTTP      *http.Client
}

const (
	maxClickUpBytes    = 2 << 20
	maxTaskDescRunes   = 8000
	maxCommentRunes    = 1500
	maxRecentComments  = 10
	clickUpCallTimeout = 30 * time.Second
)

var clickUpTaskIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ErrClickUp: the task could not be read (no token, network, status).
var ErrClickUp = errors.New("clickup task unavailable")

func (c *ClickUpClient) get(ctx context.Context, token, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, clickUpCallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return ErrClickUp
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Accept", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	cp := *hc
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := cp.Do(req)
	if err != nil {
		return ErrClickUp
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxClickUpBytes))
	if err != nil || resp.StatusCode != http.StatusOK {
		return ErrClickUp
	}
	if json.Unmarshal(data, out) != nil {
		return ErrClickUp
	}
	return nil
}

// FetchTask implements TaskFetcher.
func (c *ClickUpClient) FetchTask(ctx context.Context, taskID string) (ClickUpTask, error) {
	if c == nil || c.TokenFile == "" || !clickUpTaskIDRE.MatchString(taskID) {
		return ClickUpTask{}, ErrClickUp
	}
	token, err := ReadSecretFile(c.TokenFile)
	if err != nil {
		return ClickUpTask{}, ErrClickUp
	}
	var t struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		URL         string `json:"url"`
		Description string `json:"description"`
		TextContent string `json:"text_content"`
		Status      struct {
			Status string `json:"status"`
		} `json:"status"`
	}
	if err := c.get(ctx, token, "/api/v2/task/"+url.PathEscape(taskID), &t); err != nil {
		return ClickUpTask{}, err
	}
	desc := t.Description
	if strings.TrimSpace(desc) == "" {
		desc = t.TextContent
	}
	task := ClickUpTask{ID: t.ID, Name: strings.Join(strings.Fields(t.Name), " "), URL: t.URL,
		Status: t.Status.Status, Description: clipRunes(strings.TrimSpace(desc), maxTaskDescRunes)}
	var cm struct {
		Comments []struct {
			CommentText string `json:"comment_text"`
			Date        string `json:"date"`
			User        struct {
				Username string `json:"username"`
			} `json:"user"`
		} `json:"comments"`
	}
	if c.get(ctx, token, "/api/v2/task/"+url.PathEscape(taskID)+"/comment", &cm) == nil {
		list := cm.Comments
		sort.SliceStable(list, func(a, b int) bool {
			da, _ := strconv.ParseInt(list[a].Date, 10, 64)
			db, _ := strconv.ParseInt(list[b].Date, 10, 64)
			return da < db
		})
		if len(list) > maxRecentComments {
			list = list[len(list)-maxRecentComments:]
		}
		for _, x := range list {
			text := clipRunes(strings.TrimSpace(x.CommentText), maxCommentRunes)
			if text == "" {
				continue
			}
			who := strings.Join(strings.Fields(x.User.Username), " ")
			if who == "" {
				who = "someone"
			}
			task.Comments = append(task.Comments, who+": "+text)
		}
	}
	return task, nil
}
