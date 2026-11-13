package hubserver

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	defaultAPIPageLimit = 50
	maxAPIPageLimit     = 200
)

type workItemListResponse struct {
	Items      []tracker.WorkItem `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

type workItemListQuery struct {
	Repositories  map[string]struct{}
	WorkflowState map[string]struct{}
	Readiness     map[string]struct{}
	Priorities    map[string]struct{}
	Labels        map[string]struct{}
	Assignees     map[string]struct{}
	Machines      map[string]struct{}
	Leases        map[string]struct{}
	PullRequests  map[string]struct{}
	SyncHealth    map[string]struct{}
	Sort          string
	Order         string
	Limit         int
	Cursor        workItemCursor
}

type workItemCursor struct {
	Version int      `json:"v"`
	Sort    string   `json:"sort"`
	Order   string   `json:"order"`
	Values  []string `json:"values"`
}

type workpadProjection struct {
	Phase     string    `json:"phase"`
	Body      string    `json:"body"`
	SyncState string    `json:"sync_state"`
	UpdatedAt time.Time `json:"updated_at"`
}

type timelineEvent struct {
	ID int64 `json:"id"`
	tracker.WorkEvent
	RecordedAt time.Time `json:"recorded_at"`
}

type workItemDetailResponse struct {
	tracker.WorkItem
	Body               string             `json:"body"`
	Workpad            *workpadProjection `json:"workpad,omitempty"`
	Timeline           []timelineEvent    `json:"timeline"`
	TimelineNextCursor string             `json:"timeline_next_cursor,omitempty"`
}

type timelineCursor struct {
	Version int   `json:"v"`
	ID      int64 `json:"id"`
}

func parsePageLimit(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultAPIPageLimit, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > maxAPIPageLimit {
		return 0, errors.New("page limit is invalid")
	}
	return limit, nil
}

func encodeTimelineCursor(cursor timelineCursor) (string, error) {
	value, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func decodeTimelineCursor(value string) (timelineCursor, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return timelineCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return timelineCursor{}, err
	}
	var cursor timelineCursor
	if err := json.Unmarshal(raw, &cursor); err != nil || cursor.Version != 1 || cursor.ID <= 0 {
		return timelineCursor{}, errors.New("invalid timeline cursor")
	}
	return cursor, nil
}
