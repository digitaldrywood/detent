package hubserver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeCursor struct {
	Version int    `json:"v"`
	Scope   string `json:"scope"`
	After   string `json:"after"`
	Expires int64  `json:"expires"`
}

func (s *Service) nativePage(c echo.Context) (int, nativeCursor, []byte, error) {
	params, err := nativeReadQuery(c)
	if err != nil {
		return 0, nativeCursor{}, nil, err
	}
	return s.readNativePage(c.Request().Context(), nativeRequestScope(c), c.Request().URL.EscapedPath(), params)
}

func nativeReadQuery(c echo.Context) (url.Values, error) {
	params, err := url.ParseQuery(c.Request().URL.RawQuery)
	if err != nil {
		return nil, nativeInvalid("Query is invalid")
	}
	return params, nil
}

func (s *Service) readNativePage(ctx context.Context, scope nativeScope, path string, params url.Values) (int, nativeCursor, []byte, error) {
	limit, err := parsePageLimit(params.Get("limit"))
	if err != nil {
		return 0, nativeCursor{}, nil, nativeInvalid("Page limit is invalid")
	}
	requestedCursor := params.Get("cursor")
	cloned := url.Values{}
	for k, v := range params {
		cloned[k] = v
	}
	params = cloned
	params.Del("cursor")
	params.Del("limit")

	fingerprint := scope.credential.ID + " " + path + "?" + params.Encode()
	if scope.credential.Hosted != nil {
		fingerprint += " " + scope.credential.Hosted.SessionID
	}
	cursor := nativeCursor{Version: 2, Scope: fingerprint, Expires: s.config.now().Add(time.Hour).Unix()}
	var key []byte
	if err := s.database.db.QueryRowContext(ctx, "SELECT cursor_key FROM hub_identity").Scan(&key); err != nil {
		return 0, cursor, nil, err
	}
	value := requestedCursor
	if value == "" {
		return limit, cursor, key, nil
	}
	parts := strings.Split(value, ".")
	if len(parts) != 2 || len(value) > 4096 {
		return 0, cursor, nil, nativeInvalid("Cursor is invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return 0, cursor, nil, nativeInvalid("Cursor is invalid")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, cursor, nil, nativeInvalid("Cursor is invalid")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return 0, cursor, nil, nativeInvalid("Cursor signature is invalid")
	}
	if err := json.Unmarshal(payload, &cursor); err != nil || cursor.Version != 2 || cursor.Scope != fingerprint || cursor.Expires <= s.config.now().Unix() {
		return 0, cursor, nil, nativeInvalid("Cursor scope changed or expired; restart the query")
	}
	return limit, cursor, key, nil
}

func encodeNativeCursor(cursor nativeCursor, key []byte) (string, error) {
	payload, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func validateNativeQuery(params url.Values, fields ...string) error {
	allowed := map[string]bool{"limit": true, "cursor": true}
	for _, field := range fields {
		allowed[field] = true
	}
	for key, values := range params {
		if !allowed[key] || len(values) != 1 || len(values[0]) > 4096 {
			return nativeInvalid("Query contains an unsupported field or value")
		}
	}
	return nil
}

// parseNativeIssueIncludes reads the include query and reports whether it asks
// for workspace items. Unknown members are refused rather than ignored so a
// client typo is visible.
func parseNativeIssueIncludes(value string) (bool, error) {
	if value == "" {
		return false, nil
	}
	for name := range strings.SplitSeq(value, ",") {
		if strings.TrimSpace(name) != "workspace" {
			return false, nativeInvalid("include supports workspace")
		}
	}
	return true, nil
}

func (s *Service) listNativeIssues(c echo.Context) error {
	ctx := c.Request().Context()
	scope := nativeRequestScope(c)
	params, err := nativeReadQuery(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	page, err := s.readIssues(ctx, scope, params)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, page)
}

func (s *Service) readIssues(ctx context.Context, scope nativeScope, params url.Values) (tracker.Page[tracker.NativeIssue], error) {
	path := "/api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(string(scope.project)) + "/work-items"

	if err := validateNativeQuery(params, "state", "label", "assignee", "priority", "include", "archived", "q"); err != nil {
		return tracker.Page[tracker.NativeIssue]{}, err
	}
	includeWorkspace, err := parseNativeIssueIncludes(params.Get("include"))
	if err != nil {
		return tracker.Page[tracker.NativeIssue]{}, err
	}
	limit, cursor, key, err := s.readNativePage(ctx, scope, path, params)
	if err != nil {
		return tracker.Page[tracker.NativeIssue]{}, err
	}

	query := `SELECT i.native_id FROM issues i LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id
WHERE i.organization_id = ? AND i.project_id = ? AND i.number > CAST(? AS INTEGER)`
	args := []any{scope.organization, scope.project, cursor.After}
	switch params.Get("archived") {
	case "", "false":
		query += " AND i.archived = 0"
	case "true":
		query += " AND i.archived = 1"
	case "all":
	default:
		return tracker.Page[tracker.NativeIssue]{}, nativeInvalid("archived must be true, false or all")
	}
	if !includeWorkspace {
		// Workspace items hold a worktree open for a person's surfaces, not
		// project work (decisions section 18.1): the board would otherwise
		// fill with one card per opened Files panel.
		query += " AND " + notWorkspaceItemClause
	}
	var clauses []string
	for _, filter := range []struct{ name, clause string }{
		{"state", "ws.detent_state = ?"},
		{"label", "EXISTS (SELECT 1 FROM json_each(i.labels_json) WHERE value = ?)"},
		{"assignee", "EXISTS (SELECT 1 FROM json_each(i.assignees_json) WHERE value = ?)"},
		{"priority", "EXISTS (SELECT 1 FROM queue_entries q WHERE q.issue_id = i.id AND q.priority_override = ?)"},
	} {
		if value := params.Get(filter.name); value != "" {
			clauses = append(clauses, filter.clause)
			args = append(args, value)
		}
	}
	if len(clauses) > 0 {
		query += " AND " + strings.Join(clauses, " AND ")
	}
	if value := params.Get("q"); value != "" {
		query += " AND instr(lower(i.title || ' ' || i.body), lower(?)) > 0"
		args = append(args, value)
	}
	query += " ORDER BY i.number LIMIT ?"
	args = append(args, limit+1)
	ids, err := nativePageIDs(ctx, s.database.db, query, args...)
	if err != nil {
		return tracker.Page[tracker.NativeIssue]{}, err
	}
	page := tracker.Page[tracker.NativeIssue]{Items: []tracker.NativeIssue{}}
	hasMore := len(ids) > limit
	if hasMore {
		ids = ids[:limit]
	}
	for _, id := range ids {
		issue, _, err := readNativeIssue(ctx, s.database.db, scope, id)
		if err != nil {
			return tracker.Page[tracker.NativeIssue]{}, err
		}
		page.Items = append(page.Items, s.nativeIssueResponse(issue))
		cursor.After = strconv.Itoa(issue.Number)
	}
	if hasMore {
		page.NextCursor, err = encodeNativeCursor(cursor, key)
		if err != nil {
			return tracker.Page[tracker.NativeIssue]{}, err
		}
	}
	return page, nil
}

func nativePageIDs(ctx context.Context, query nativeQueryer, statement string, args ...any) ([]string, error) {
	rows, err := query.QueryContext(ctx, statement, args...)
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

func (s *Service) listNativeComments(c echo.Context) error {
	ctx := c.Request().Context()
	scope := nativeRequestScope(c)
	params, err := nativeReadQuery(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	page, err := s.readComments(ctx, scope, c.Param("item"), params)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, page)
}

func (s *Service) readComments(ctx context.Context, scope nativeScope, item string, params url.Values) (tracker.Page[tracker.NativeComment], error) {
	path := "/api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(string(scope.project)) + "/work-items/" + url.PathEscape(item) + "/comments"

	if err := validateNativeQuery(params); err != nil {
		return tracker.Page[tracker.NativeComment]{}, err
	}
	limit, cursor, key, err := s.readNativePage(ctx, scope, path, params)
	if err != nil {
		return tracker.Page[tracker.NativeComment]{}, err
	}

	if _, _, err := readNativeIssue(ctx, s.database.db, scope, item); err != nil {
		return tracker.Page[tracker.NativeComment]{}, err
	}
	ids, err := nativePageIDs(ctx, s.database.db, "SELECT id FROM native_comments WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND sequence > CAST(? AS INTEGER) ORDER BY sequence LIMIT ?", scope.organization, scope.project, item, cursor.After, limit+1)
	if err != nil {
		return tracker.Page[tracker.NativeComment]{}, err
	}
	page := tracker.Page[tracker.NativeComment]{Items: []tracker.NativeComment{}}
	hasMore := len(ids) > limit
	if hasMore {
		ids = ids[:limit]
	}
	for _, id := range ids {
		comment, err := readNativeComment(ctx, s.database.db, scope, item, id)
		if err != nil {
			return tracker.Page[tracker.NativeComment]{}, err
		}
		page.Items = append(page.Items, comment)
		cursor.After = strconv.FormatInt(comment.Sequence, 10)
	}
	if hasMore {
		page.NextCursor, err = encodeNativeCursor(cursor, key)
		if err != nil {
			return tracker.Page[tracker.NativeComment]{}, err
		}
	}
	return page, nil
}

func (s *Service) listNativeHistory(c echo.Context) error {
	ctx := c.Request().Context()
	scope := nativeRequestScope(c)
	params, err := nativeReadQuery(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	page, err := s.readHistory(ctx, scope, c.Param("item"), params)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, page)
}

func (s *Service) readHistory(ctx context.Context, scope nativeScope, item string, params url.Values) (tracker.Page[tracker.CollaborationEvent], error) {
	path := "/api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(string(scope.project)) + "/work-items/" + url.PathEscape(item) + "/history"

	if err := validateNativeQuery(params); err != nil {
		return tracker.Page[tracker.CollaborationEvent]{}, err
	}
	limit, cursor, key, err := s.readNativePage(ctx, scope, path, params)
	if err != nil {
		return tracker.Page[tracker.CollaborationEvent]{}, err
	}

	if _, _, err := readNativeIssue(ctx, s.database.db, scope, item); err != nil {
		return tracker.Page[tracker.CollaborationEvent]{}, err
	}
	var after int64
	if cursor.After != "" {
		after, err = strconv.ParseInt(cursor.After, 10, 64)
		if err != nil {
			return tracker.Page[tracker.CollaborationEvent]{}, nativeInvalid("History cursor is invalid")
		}
	}
	rows, err := s.database.db.QueryContext(ctx, `SELECT id, sequence, type, schema_version, actor_json, data_json, recorded_at FROM collaboration_events
WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND sequence > ? ORDER BY sequence LIMIT ?`, scope.organization, scope.project, item, after, limit+1)
	if err != nil {
		return tracker.Page[tracker.CollaborationEvent]{}, err
	}
	defer rows.Close()
	page := tracker.Page[tracker.CollaborationEvent]{Items: []tracker.CollaborationEvent{}}
	for rows.Next() {
		event := tracker.CollaborationEvent{OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "work_item", AggregateID: tracker.NativeWorkItemID(item)}
		var actor, data, recorded string
		if err := rows.Scan(&event.ID, &event.AggregateSequence, &event.Type, &event.SchemaVersion, &actor, &data, &recorded); err != nil {
			return tracker.Page[tracker.CollaborationEvent]{}, err
		}
		if err := json.Unmarshal([]byte(actor), &event.Actor); err != nil {
			return tracker.Page[tracker.CollaborationEvent]{}, err
		}
		if err := json.Unmarshal([]byte(data), &event.Data); err != nil {
			return tracker.Page[tracker.CollaborationEvent]{}, err
		}
		if event.Data.Run != nil {
			event.Data.Run.Runtime = event.Data.Run.Runtime.WithoutActivitySpans()
		}
		if event.RecordedAt, err = parseTimeValue(recorded); err != nil {
			return tracker.Page[tracker.CollaborationEvent]{}, err
		}
		page.Items = append(page.Items, event)
	}
	if err := rows.Err(); err != nil {
		return tracker.Page[tracker.CollaborationEvent]{}, err
	}
	if err := rows.Close(); err != nil {
		return tracker.Page[tracker.CollaborationEvent]{}, err
	}
	for index := range page.Items {
		event := &page.Items[index]
		if event.Data.RelatedWorkItemID == "" || scope.credential.Scope == apiScopeAdmin && !scope.credential.NativeOnly {
			continue
		}
		var count int
		err := s.database.db.QueryRowContext(ctx, `SELECT count(*) FROM issues i JOIN token_grants g ON g.organization_id = i.organization_id AND g.project_id = i.project_id
WHERE i.organization_id = ? AND i.native_id = ? AND g.token_id = ?`, scope.organization, event.Data.RelatedWorkItemID, scope.credential.ID).Scan(&count)
		if err != nil {
			return tracker.Page[tracker.CollaborationEvent]{}, err
		}
		if count == 0 {
			event.Data.RelatedWorkItemID = ""
		}
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		cursor.After = strconv.FormatInt(page.Items[len(page.Items)-1].AggregateSequence, 10)
		page.NextCursor, err = encodeNativeCursor(cursor, key)
		if err != nil {
			return tracker.Page[tracker.CollaborationEvent]{}, err
		}
	}
	return page, nil
}
