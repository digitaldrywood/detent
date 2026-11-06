package hubserver

import (
	"encoding/json"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"
)

const (
	maxSavedWorkViewBytes            = 16 << 10
	maxSavedWorkViewsPerOwner        = 50
	maxSavedWorkViewsPerOrganization = 200
	maxSavedWorkViewsPageSize        = 25
	maxSavedWorkViewsPageBytes       = 256 << 10
)

type savedWorkViewsPage struct {
	Views      []savedWorkView `json:"views"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

type savedWorkState struct {
	View            string   `json:"view"`
	Tab             string   `json:"tab"`
	Q               string   `json:"q"`
	Sort            string   `json:"sort"`
	CompletedWindow string   `json:"completedWindow"`
	CompletedFrom   string   `json:"completedFrom,omitempty"`
	CompletedTo     string   `json:"completedTo,omitempty"`
	Archived        bool     `json:"archived"`
	CreatedBy       string   `json:"createdBy,omitempty"`
	State           []string `json:"state"`
	Type            []string `json:"type"`
	Label           []string `json:"label"`
	Assignee        []string `json:"assignee"`
	Priority        []string `json:"priority"`
	Lanes           []string `json:"lanes"`
	Collapsed       []string `json:"collapsed"`
}

type savedWorkView struct {
	CanEdit    bool           `json:"can_edit"`
	ID         string         `json:"id"`
	OwnerID    string         `json:"owner_id"`
	Name       string         `json:"name"`
	Scope      string         `json:"scope"`
	ProjectID  *string        `json:"project_id"`
	ProjectIDs []string       `json:"project_ids"`
	Favorite   bool           `json:"favorite"`
	State      savedWorkState `json:"state"`
}

func (s *Service) savedWorkViews(c echo.Context) error {
	return s.savedWorkViewsRecord(c)
}

func (s savedWorkState) values() map[string][]string {
	return map[string][]string{"state": s.State, "type": s.Type, "label": s.Label, "assignee": s.Assignee, "priority": s.Priority, "lanes": s.Lanes, "collapsed": s.Collapsed}
}

func validSavedWorkView(v savedWorkView) bool {
	s := v.State
	for _, bound := range []string{s.CompletedFrom, s.CompletedTo} {
		if bound != "" {
			if _, err := time.Parse(time.RFC3339Nano, bound); err != nil {
				return false
			}
		}
	}
	if v.ProjectIDs == nil || len(v.ProjectIDs) > 100 || (v.ProjectID != nil && !slices.Contains(v.ProjectIDs, *v.ProjectID)) {
		return false
	}
	seen := make(map[string]bool, len(v.ProjectIDs))
	for _, id := range v.ProjectIDs {
		if id == "" || len(id) > 512 || seen[id] {
			return false
		}
		seen[id] = true
	}
	if len(v.Name) == 0 || utf8.RuneCountInString(v.Name) > 120 || !slices.Contains([]string{"personal", "organization"}, v.Scope) || (v.ProjectID != nil && *v.ProjectID == "") {
		return false
	}
	if !slices.Contains([]string{"board", "list"}, s.View) || !slices.Contains([]string{"active", "backlog", "closed", "all"}, s.Tab) || !slices.Contains([]string{"default", "priority", "updated", "created", "title", "closed"}, s.Sort) || !slices.Contains([]string{"1h", "24h", "7d"}, s.CompletedWindow) || !slices.Contains([]string{"", "me"}, s.CreatedBy) || len(s.Q) > 8192 {
		return false
	}
	total := 0
	for _, values := range s.values() {
		total += len(values)
		for _, value := range values {
			if len(value) == 0 || len(value) > 512 {
				return false
			}
		}
	}
	if total > 256 {
		return false
	}
	encoded, err := json.Marshal(v)
	return err == nil && len(encoded) <= maxSavedWorkViewBytes
}
