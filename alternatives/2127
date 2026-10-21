package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/labstack/echo/v4"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/digitaldrywood/detent/internal/tracker"
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
	CanEdit   bool           `json:"can_edit"`
	ID        string         `json:"id"`
	OwnerID   string         `json:"owner_id"`
	Name      string         `json:"name"`
	Scope     string         `json:"scope"`
	ProjectID *string        `json:"project_id"`
	Favorite  bool           `json:"favorite"`
	State     savedWorkState `json:"state"`
}

func (s *Service) savedWorkViews(c echo.Context) error {
	ctx := c.Request().Context()
	readCtx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	credential, _, err := s.hostedCredential(ctx, c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	org := s.config.Hosted.OrganizationID
	if c.Param("organization") != org {
		return s.nativeAPIError(c, nativeNotFound())
	}
	user := credential.Hosted.Subject
	id := c.Param("view")
	var current savedWorkView
	if id != "" {
		current, err = s.readSavedWorkView(readCtx, org, user, id)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	if c.Request().Method == http.MethodGet {
		if id != "" {
			return c.JSON(http.StatusOK, current)
		}
		params := c.QueryParams()
		for key, values := range params {
			if key != "cursor" || len(values) != 1 || len(values[0]) > 100 {
				return s.nativeAPIError(c, nativeInvalid("Invalid saved views cursor"))
			}
		}
		ids, err := s.savedWorkViewIDs(readCtx, org, user, params.Get("cursor"))
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		page := savedWorkViewsPage{Views: []savedWorkView{}}
		for _, key := range ids {
			if len(page.Views) == maxSavedWorkViewsPageSize {
				page.NextCursor = page.Views[len(page.Views)-1].ID
				break
			}
			v, err := s.readSavedWorkView(readCtx, org, user, key)
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			candidate := savedWorkViewsPage{Views: append(page.Views, v), NextCursor: key}
			encoded, err := json.Marshal(candidate)
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			if len(encoded)+1 > maxSavedWorkViewsPageBytes {
				if len(page.Views) == 0 {
					return s.nativeAPIError(c, nativeInvalid("Saved view exceeds the page size limit"))
				}
				page.NextCursor = page.Views[len(page.Views)-1].ID
				break
			}
			page.Views = candidate.Views
		}
		encoded, err := json.Marshal(page)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.JSONBlob(http.StatusOK, encoded)
	}
	if strings.HasSuffix(c.Path(), "/favorite") {
		var value struct {
			Favorite bool `json:"favorite"`
		}
		if err := decodeAPIJSON(c, &value); err != nil {
			return invalidAPIRequest(c, err)
		}
		err = s.setSavedWorkFavorite(ctx, s.database.db, id, user, value.Favorite)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		current.Favorite = value.Favorite
		return c.JSON(http.StatusOK, current)
	}
	if id != "" && current.OwnerID != user {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if c.Request().Method == http.MethodDelete {
		_, err = s.database.db.ExecContext(ctx, `DELETE FROM hosted_work_views WHERE id=? AND organization_id=? AND owner_id=?`, id, org, user)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.NoContent(http.StatusNoContent)
	}
	var v savedWorkView
	if err := decodeAPIJSON(c, &v); err != nil {
		return invalidAPIRequest(c, err)
	}
	v.Name = strings.TrimSpace(v.Name)
	if !validSavedWorkView(v) {
		return s.nativeAPIError(c, nativeInvalid("The view name, scope or display state is invalid"))
	}
	if v.ProjectID != nil {
		scope := nativeScope{organization: tracker.OrganizationID(org), project: tracker.ProjectID(*v.ProjectID), credential: credential}
		if err := s.requireHostedProject(readCtx, s.database.reader, scope, false); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	ownerSlot, organizationSlot := 0, 0
	if id == "" {
		ownerSlot, organizationSlot, err = s.savedWorkViewSlots(readCtx, org, user)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		id = newNativeID("view")
	} else {
		err = s.database.reader.QueryRowContext(readCtx, `SELECT owner_slot,organization_slot FROM hosted_work_views WHERE id=? AND organization_id=? AND owner_id=?`, id, org, user).Scan(&ownerSlot, &organizationSlot)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	v.ID = id
	v.OwnerID = user
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO hosted_work_views (id,organization_id,owner_id,name,scope,project_id,layout, tab, search, sort, completed_window, archived, created_by,all_lanes,owner_slot,organization_slot) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,scope=excluded.scope,project_id=excluded.project_id,layout=excluded.layout,tab=excluded.tab,search=excluded.search,sort=excluded.sort,completed_window=excluded.completed_window,archived=excluded.archived,created_by=excluded.created_by,all_lanes=excluded.all_lanes`, id, org, user, v.Name, v.Scope, v.ProjectID, v.State.View, v.State.Tab, v.State.Q, v.State.Sort, v.State.CompletedWindow, v.State.Archived, v.State.CreatedBy, v.State.Lanes == nil, ownerSlot, organizationSlot)
	if err != nil {
		var constraint *sqlite.Error
		if errors.As(err, &constraint) && constraint.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE {
			return s.nativeAPIError(c, nativeConflict(0))
		}
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `DELETE FROM hosted_work_view_values WHERE view_id=?`, id)
	}
	if err == nil {
		for kind, values := range v.State.values() {
			for position, value := range values {
				_, err = tx.ExecContext(ctx, `INSERT INTO hosted_work_view_values(view_id,kind,position,value) VALUES(?,?,?,?)`, id, kind, position, value)
				if err != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
	}
	if err == nil {
		err = s.setSavedWorkFavorite(ctx, tx, id, user, v.Favorite)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	v, err = s.readSavedWorkView(readCtx, org, user, id)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, v)
}

func (s savedWorkState) values() map[string][]string {
	return map[string][]string{"state": s.State, "type": s.Type, "label": s.Label, "assignee": s.Assignee, "priority": s.Priority, "lanes": s.Lanes, "collapsed": s.Collapsed}
}

func validSavedWorkView(v savedWorkView) bool {
	s := v.State
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

func (s *Service) readSavedWorkView(ctx context.Context, org, user, id string) (savedWorkView, error) {
	var v savedWorkView
	var allLanes bool
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return v, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT id,owner_id,name,scope,project_id,layout, tab, search, sort, completed_window, archived, created_by,all_lanes,EXISTS(SELECT 1 FROM hosted_work_view_favorites f WHERE f.view_id=v.id AND f.user_id=?) FROM hosted_work_views v WHERE id=? AND organization_id=? AND (owner_id=? OR scope='organization') AND (project_id IS NULL OR EXISTS(SELECT 1 FROM hosted_project_grants g WHERE g.organization_id=v.organization_id AND g.project_id=v.project_id AND g.user_id=?))`, user, id, org, user, user).Scan(&v.ID, &v.OwnerID, &v.Name, &v.Scope, &v.ProjectID, &v.State.View, &v.State.Tab, &v.State.Q, &v.State.Sort, &v.State.CompletedWindow, &v.State.Archived, &v.State.CreatedBy, &allLanes, &v.Favorite)
	if errors.Is(err, sql.ErrNoRows) {
		return v, nativeNotFound()
	}
	if err != nil {
		return v, err
	}
	v.State.State = []string{}
	v.State.Type = []string{}
	v.State.Label = []string{}
	v.State.Assignee = []string{}
	v.State.Priority = []string{}
	v.State.Collapsed = []string{}
	if !allLanes {
		v.State.Lanes = []string{}
	}
	rows, err := tx.QueryContext(ctx, `SELECT kind,value FROM hosted_work_view_values WHERE view_id=? ORDER BY kind,position`, id)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, value string
		if err := rows.Scan(&kind, &value); err != nil {
			return v, err
		}
		switch kind {
		case "state":
			v.State.State = append(v.State.State, value)
		case "type":
			v.State.Type = append(v.State.Type, value)
		case "label":
			v.State.Label = append(v.State.Label, value)
		case "assignee":
			v.State.Assignee = append(v.State.Assignee, value)
		case "priority":
			v.State.Priority = append(v.State.Priority, value)
		case "lanes":
			v.State.Lanes = append(v.State.Lanes, value)
		case "collapsed":
			v.State.Collapsed = append(v.State.Collapsed, value)
		}
	}
	v.CanEdit = v.OwnerID == user
	return v, rows.Err()
}

func (s *Service) setSavedWorkFavorite(ctx context.Context, db nativeExecer, id, user string, favorite bool) error {
	var err error
	if favorite {
		_, err = db.ExecContext(ctx, `INSERT INTO hosted_work_view_favorites(view_id,user_id) VALUES(?,?) ON CONFLICT(view_id,user_id) DO NOTHING`, id, user)
	} else {
		_, err = db.ExecContext(ctx, `DELETE FROM hosted_work_view_favorites WHERE view_id=? AND user_id=?`, id, user)
	}
	return err
}

func (s *Service) savedWorkViewIDs(ctx context.Context, org, user, cursor string) ([]string, error) {
	rows, err := s.database.reader.QueryContext(ctx, `SELECT id FROM hosted_work_views v WHERE organization_id=? AND id>? AND (owner_id=? OR scope='organization') AND (project_id IS NULL OR EXISTS (SELECT 1 FROM hosted_project_grants g WHERE g.organization_id=v.organization_id AND g.project_id=v.project_id AND g.user_id=?)) ORDER BY id LIMIT ?`, org, cursor, user, user, maxSavedWorkViewsPageSize+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Service) savedWorkViewSlots(ctx context.Context, org, user string) (int, int, error) {
	var ownerSlot, organizationSlot int
	err := s.database.reader.QueryRowContext(ctx, `WITH RECURSIVE slots(slot) AS (SELECT 0 UNION ALL SELECT slot+1 FROM slots WHERE slot+1<?) SELECT slot FROM slots WHERE NOT EXISTS(SELECT 1 FROM hosted_work_views WHERE organization_id=? AND owner_id=? AND owner_slot=slot) ORDER BY slot LIMIT 1`, maxSavedWorkViewsPerOwner, org, user).Scan(&ownerSlot)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nativeInvalid("The saved view limit for this owner has been reached")
	}
	if err != nil {
		return 0, 0, err
	}
	err = s.database.reader.QueryRowContext(ctx, `WITH RECURSIVE slots(slot) AS (SELECT 0 UNION ALL SELECT slot+1 FROM slots WHERE slot+1<?) SELECT slot FROM slots WHERE NOT EXISTS(SELECT 1 FROM hosted_work_views WHERE organization_id=? AND organization_slot=slot) ORDER BY slot LIMIT 1`, maxSavedWorkViewsPerOrganization, org).Scan(&organizationSlot)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nativeInvalid("The saved view limit for this organization has been reached")
	}
	return ownerSlot, organizationSlot, err
}
