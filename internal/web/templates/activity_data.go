package templates

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type BoardActivityData struct {
	ProjectID  string
	IssueID    string
	Identifier string
	IssueURL   string
	Events     []BoardActivityEvent
	Verbose    bool
	Limit      int
	HasMore    bool
	Error      string
}

type BoardActivityEvent struct {
	ID            string
	At            time.Time
	Kind          string
	Title         string
	Detail        string
	Reason        string
	Status        string
	Model         string
	AttemptNumber int
	SessionID     int64
	Turns         int64
	TotalTokens   int64
	Verbose       bool
}

type BoardSessionData struct {
	ProjectID         string
	IssueID           string
	Identifier        string
	DetentSessionID   int64
	ProviderSessionID string
	Active            bool
	HistoryAvailable  bool
}

type BoardSessionEvent struct {
	ID        string
	At        time.Time
	Kind      string
	Title     string
	Content   string
	Status    string
	Model     string
	Tokens    int64
	Truncated bool
}

type BoardSessionHistoryData struct {
	Session BoardSessionData
	Events  []BoardSessionEvent
	Offset  int
	Limit   int
	HasMore bool
	Error   string
}

func boardActivityPath(data BoardActivityData, limit int, verbose bool, events bool) string {
	values := url.Values{}
	values.Set("project", strings.TrimSpace(data.ProjectID))
	values.Set("issue", strings.TrimSpace(data.IssueID))
	if data.Identifier != "" {
		values.Set("identifier", strings.TrimSpace(data.Identifier))
	}
	if limit > 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
	if verbose {
		values.Set("verbose", "1")
	}
	path := "/api/v1/board/activity"
	if events {
		path += "/events"
	}
	return path + "?" + values.Encode()
}

func boardSessionPath(data BoardSessionData, events bool) string {
	values := url.Values{}
	values.Set("project", strings.TrimSpace(data.ProjectID))
	values.Set("issue", strings.TrimSpace(data.IssueID))
	if data.Identifier != "" {
		values.Set("identifier", strings.TrimSpace(data.Identifier))
	}
	path := "/api/v1/board/session"
	if events {
		path += "/events"
	}
	return path + "?" + values.Encode()
}

func boardSessionHistoryPath(data BoardSessionData, offset int, limit int) string {
	values := url.Values{}
	values.Set("project", strings.TrimSpace(data.ProjectID))
	values.Set("issue", strings.TrimSpace(data.IssueID))
	if data.Identifier != "" {
		values.Set("identifier", strings.TrimSpace(data.Identifier))
	}
	if offset > 0 {
		values.Set("offset", strconv.Itoa(offset))
	}
	if limit > 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
	return "/api/v1/board/session/history?" + values.Encode()
}

func boardLiveSessionID(data BoardSessionData) string {
	key := strings.Join([]string{
		strings.TrimSpace(data.ProjectID),
		strings.TrimSpace(data.IssueID),
		strings.TrimSpace(data.Identifier),
		strconv.FormatBool(data.Active),
		strconv.FormatInt(data.DetentSessionID, 10),
		strings.TrimSpace(data.ProviderSessionID),
	}, "\x00")
	digest := sha256.Sum256([]byte(key))
	return "board-live-session-" + hex.EncodeToString(digest[:8])
}

func boardLiveSessionTarget(data BoardSessionData) string {
	return "#" + boardLiveSessionID(data)
}

func boardActivityEventID(event BoardActivityEvent) string {
	replacer := strings.NewReplacer(":", "-", "/", "-", " ", "-", "#", "-")
	return "activity-" + replacer.Replace(strings.TrimSpace(event.ID))
}

func boardActivityTime(event BoardActivityEvent) string {
	if event.At.IsZero() {
		return "now"
	}
	return event.At.Local().Format("Jan 2 · 15:04:05")
}

func boardActivityMeta(event BoardActivityEvent) string {
	parts := make([]string, 0, 4)
	if event.AttemptNumber > 0 {
		parts = append(parts, "attempt "+strconv.Itoa(event.AttemptNumber))
	}
	if event.Model != "" {
		parts = append(parts, event.Model)
	}
	if event.Turns > 0 {
		parts = append(parts, strconv.FormatInt(event.Turns, 10)+" turns")
	}
	if event.TotalTokens > 0 {
		parts = append(parts, strconv.FormatInt(event.TotalTokens, 10)+" tokens")
	}
	return strings.Join(parts, " · ")
}

func boardActivityDotClass(event BoardActivityEvent) string {
	status := strings.ToLower(strings.TrimSpace(event.Status + " " + event.Kind + " " + event.Title))
	switch {
	case strings.Contains(status, "fail"), strings.Contains(status, "error"), strings.Contains(status, "breaker"), strings.Contains(status, "blocked"):
		return "bg-err"
	case strings.Contains(status, "skip"), strings.Contains(status, "wait"), strings.Contains(status, "pending"):
		return "bg-warn"
	case strings.Contains(status, "finish"), strings.Contains(status, "complete"), strings.Contains(status, "selected"):
		return "bg-ok"
	default:
		return "bg-accent"
	}
}
