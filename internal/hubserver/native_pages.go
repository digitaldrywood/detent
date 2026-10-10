package hubserver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeCursor struct {
	Version int                    `json:"v"`
	Scope   string                 `json:"scope"`
	After   string                 `json:"after"`
	Expires int64                  `json:"expires"`
	Issues  *nativeIssuePageCursor `json:"issues,omitempty"`
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
	if err := s.database.reader.QueryRowContext(ctx, "SELECT cursor_key FROM hub_identity").Scan(&key); err != nil {
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

func validateNativeIssueQuery(params url.Values) error {
	single := url.Values{}
	for key, values := range params {
		switch key {
		case "state", "label", "assignee", "priority":
			if len(values) > 32 || len(strings.Join(values, "\x00")) > 4096 {
				return nativeInvalid("Query contains an unsupported field or value")
			}
		default:
			single[key] = values
		}
	}
	return validateNativeQuery(single, "include", "archived", "q", "open", "fingerprint", "completed_window", "sort")
}

// parseNativeIssueIncludes reads the include query and reports whether it asks
// for workspace items. Unknown members are refused rather than ignored so a
// client typo is visible.
func parseNativeIssueIncludes(value string) (bool, error) {
	if value == "" {
		return false, nil
	}
	for name := range strings.SplitSeq(value, ",") {
		if name = strings.TrimSpace(name); name != "workspace" && name != "work" && name != "summary" {
			return false, nativeInvalid("include supports workspace,work,summary")
		}
	}
	return slices.ContainsFunc(strings.Split(value, ","), func(name string) bool { return strings.TrimSpace(name) == "workspace" }), nil
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

func (s *Service) readIssues(ctx context.Context, scope nativeScope, params url.Values) (tracker.NativeIssuePage, error) {
	path := "/api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(string(scope.project)) + "/work-items"

	if err := validateNativeIssueQuery(params); err != nil {
		return tracker.NativeIssuePage{}, err
	}
	if _, err := nativeCompletedWindow(params.Get("completed_window")); err != nil {
		return tracker.NativeIssuePage{}, err
	}
	if sort := params.Get("sort"); sort != "" && sort != "closed" {
		return tracker.NativeIssuePage{}, nativeInvalid("sort supports closed")
	}
	includeWorkspace, err := parseNativeIssueIncludes(params.Get("include"))
	if err != nil {
		return tracker.NativeIssuePage{}, err
	}
	summary := slices.ContainsFunc(strings.Split(params.Get("include"), ","), func(name string) bool { return strings.TrimSpace(name) == "summary" })
	cursorParams := params
	if params.Has("include") {
		cursorParams = url.Values{}
		for name, values := range params {
			cursorParams[name] = values
		}
		includes := slices.DeleteFunc(strings.Split(params.Get("include"), ","), func(name string) bool { return slices.Contains([]string{"summary", "work"}, strings.TrimSpace(name)) })
		cursorParams.Del("include")
		if len(includes) > 0 {
			cursorParams.Set("include", strings.Join(includes, ","))
		}
	}
	limit, cursor, key, err := s.readNativePage(ctx, scope, path, cursorParams)
	if err != nil {
		return tracker.NativeIssuePage{}, err
	}

	query := `SELECT i.native_id FROM issues i LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id
WHERE i.organization_id = ? AND i.project_id = ? `
	args := []any{scope.organization, scope.project}
	switch params.Get("archived") {
	case "", "false":
		query += " AND i.archived = 0"
	case "true":
		query += " AND i.archived = 1"
	case "all":
	default:
		return tracker.NativeIssuePage{}, nativeInvalid("archived must be true, false or all")
	}
	switch params.Get("open") {
	case "":
	case "true":
		query += " AND ws.terminal = 0"
	case "false":
		query += " AND ws.terminal = 1"
	default:
		return tracker.NativeIssuePage{}, nativeInvalid("open must be true or false")
	}
	if value := strings.TrimSpace(params.Get("fingerprint")); value != "" {
		query += " AND (instr(i.body, ?) > 0 OR EXISTS (SELECT 1 FROM native_comments c WHERE c.work_item_id = i.native_id AND instr(c.body, ?) > 0))"
		marker := "\nfingerprint: " + value + "\n"
		args = append(args, marker, marker)
	}
	if !includeWorkspace {
		// Workspace items hold a worktree open for a person's surfaces, not
		// project work (decisions section 18.1): the board would otherwise
		// fill with one card per opened Files panel.
		query += " AND " + notWorkspaceItemClause
	}
	var filterClauses strings.Builder
	for _, filter := range []struct{ name, clause string }{
		{"state", "ws.detent_state = ?"},
		{"label", "EXISTS (SELECT 1 FROM json_each(i.labels_json) WHERE value = ?)"},
		{"assignee", "EXISTS (SELECT 1 FROM json_each(i.assignees_json) WHERE value = ?)"},
		{"priority", "EXISTS (SELECT 1 FROM queue_entries q WHERE q.issue_id = i.id AND q.priority_override = ?)"},
	} {
		var alternatives []string
		for _, value := range params[filter.name] {
			if value == "" {
				continue
			}
			alternatives = append(alternatives, filter.clause)
			args = append(args, value)
		}
		if len(alternatives) > 0 {
			filterClauses.WriteString(" AND (" + strings.Join(alternatives, " OR ") + ")")
		}
	}
	query += filterClauses.String()
	workIncluded := slices.ContainsFunc(strings.Split(params.Get("include"), ","), func(name string) bool { return strings.TrimSpace(name) == "work" })
	if summary && workIncluded {
		return tracker.NativeIssuePage{}, nativeInvalid("summary cannot include work")
	}
	if value := strings.TrimSpace(params.Get("q")); value != "" {
		text := "i.title"
		if !workIncluded {
			text += " || ' ' || i.body"
		}
		query += " AND (instr(lower(" + text + "), lower(?)) > 0" +
			" OR instr(lower((SELECT name FROM projects WHERE id = i.project_id) || '#' || i.number), lower(?)) > 0" +
			" OR instr(lower(i.project_id || '#' || i.number), lower(?)) > 0" +
			" OR EXISTS (SELECT 1 FROM json_each(i.labels_json) WHERE instr(lower(value), lower(?)) > 0))"
		args = append(args, value, value, value, value)
	}

	var work *tracker.NativeWorkSummary
	if workIncluded {
		work, err = readNativeWorkSummary(ctx, s.database.reader, scope, query, args, limit, s.config.now(), params.Get("completed_window"))
		if err != nil {
			return tracker.NativeIssuePage{}, err
		}
	}
	items, frozen, total, err := s.readIssuePageItems(ctx, query, args, limit, &cursor, params.Get("sort"))
	if err != nil {
		return tracker.NativeIssuePage{}, err
	}
	page := tracker.NativeIssuePage{Page: tracker.Page[tracker.NativeIssue]{Items: []tracker.NativeIssue{}}}
	page.Total = total
	operational := map[string]tracker.NativeIssue{}
	if work != nil {
		for _, issue := range work.Items {
			operational[string(issue.WorkItemID)] = issue
		}
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	for _, item := range items {
		id := item.ID
		issue, loaded := operational[id]
		if !loaded {
			issue, _, err = readNativeIssueProjection(ctx, s.database.reader, scope, id, workIncluded || summary)
			if err != nil {
				return tracker.NativeIssuePage{}, err
			}
		}
		issue = s.nativeIssueResponse(issue)
		next := cursor
		next.Issues = &item.Position
		if summary {
			issue = operatortool.NativeListIssue(issue)
			candidate := tracker.Page[tracker.NativeIssue]{Items: append(page.Items, issue)}
			candidate.NextCursor, err = encodeNativeCursor(next, key)
			if err != nil {
				return tracker.NativeIssuePage{}, err
			}
			raw, encodeErr := json.Marshal(operatortool.NativeItemPage(string(scope.project), candidate))
			if encodeErr != nil {
				return tracker.NativeIssuePage{}, encodeErr
			}
			if len(raw) > operatortool.WorkListPageBytes {
				if len(page.Items) == 0 {
					return tracker.NativeIssuePage{}, operatortool.ErrReadUnavailable
				}
				hasMore = true
				break
			}
		}
		page.Items = append(page.Items, issue)
		cursor = next
	}
	if hasMore {
		page.NextCursor, err = encodeNativeCursor(cursor, key)
		if err != nil {
			return tracker.NativeIssuePage{}, err
		}
		if frozen != nil {
			s.issuePages.store(cursor.Issues.Snapshot, cursor.Scope, cursor.Expires, frozen, s.config.now())
		}
	}
	page.Work = work
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

	if _, _, err := readNativeIssue(ctx, s.database.reader, scope, item); err != nil {
		return tracker.Page[tracker.NativeComment]{}, err
	}
	ids, err := nativePageIDs(ctx, s.database.reader, "SELECT id FROM native_comments WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND sequence > CAST(? AS INTEGER) ORDER BY sequence LIMIT ?", scope.organization, scope.project, item, cursor.After, limit+1)
	if err != nil {
		return tracker.Page[tracker.NativeComment]{}, err
	}
	page := tracker.Page[tracker.NativeComment]{Items: []tracker.NativeComment{}}
	hasMore := len(ids) > limit
	if hasMore {
		ids = ids[:limit]
	}
	for _, id := range ids {
		comment, err := readNativeComment(ctx, s.database.reader, scope, item, id)
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

	if err := validateNativeQuery(params, "view"); err != nil {
		return tracker.Page[tracker.CollaborationEvent]{}, err
	}
	view := params.Get("view")
	if view != "" && view != "blockers" {
		return tracker.Page[tracker.CollaborationEvent]{}, nativeInvalid("view supports blockers")
	}
	dataColumn := "data_json"
	if view == "blockers" {
		dataColumn = `json_object('revision', json_extract(data_json, '$.revision'),
 'from_state', json_extract(data_json, '$.from_state'), 'to_state', json_extract(data_json, '$.to_state'),
 'reason', json_extract(data_json, '$.reason'), 'run', CASE WHEN json_type(data_json, '$.run') = 'object'
 THEN json_object('attempt_id', json_extract(data_json, '$.run.attempt_id'),
 'fencing_token', json_extract(data_json, '$.run.fencing_token')) ELSE NULL END)`
	}
	limit, cursor, key, err := s.readNativePage(ctx, scope, path, params)
	if err != nil {
		return tracker.Page[tracker.CollaborationEvent]{}, err
	}

	if _, _, err := readNativeIssueProjection(ctx, s.database.db, scope, item, true); err != nil {
		return tracker.Page[tracker.CollaborationEvent]{}, err
	}
	var after int64
	if cursor.After != "" {
		after, err = strconv.ParseInt(cursor.After, 10, 64)
		if err != nil {
			return tracker.Page[tracker.CollaborationEvent]{}, nativeInvalid("History cursor is invalid")
		}
	}
	rows, err := s.database.db.QueryContext(ctx, `SELECT id, sequence, type, schema_version, actor_json, `+dataColumn+`, recorded_at FROM collaboration_events
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
		encoded, err := json.Marshal(event)
		if err != nil {
			return tracker.Page[tracker.CollaborationEvent]{}, err
		}
		if len(encoded) > operatortool.WorkHistoryPageBytes-8192 {
			digest := sha256.Sum256([]byte(data))
			event.Data = tracker.CollaborationData{}
			event.DataOmission = &tracker.HistoryDataOmission{Bytes: len(data), SHA256: hex.EncodeToString(digest[:])}
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
		condition, grantArgs := scope.credential.projectGrantSQL("i.organization_id", "i.project_id")
		args := append([]any{scope.organization, event.Data.RelatedWorkItemID}, grantArgs...)
		err := s.database.db.QueryRowContext(ctx, `SELECT count(*) FROM issues i WHERE i.organization_id=? AND i.native_id=? AND (`+condition+`)`, args...).Scan(&count)
		if err != nil {
			return tracker.Page[tracker.CollaborationEvent]{}, err
		}
		if count == 0 {
			event.Data.RelatedWorkItemID = ""
		}
	}
	hasMore := len(page.Items) > limit
	items := page.Items[:min(len(page.Items), limit)]
	page.Items = []tracker.CollaborationEvent{}
	for _, event := range items {
		next := cursor
		next.After = strconv.FormatInt(event.AggregateSequence, 10)
		candidate := tracker.Page[tracker.CollaborationEvent]{Items: append(page.Items, event)}
		candidate.NextCursor, err = encodeNativeCursor(next, key)
		if err != nil {
			return tracker.Page[tracker.CollaborationEvent]{}, err
		}
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return tracker.Page[tracker.CollaborationEvent]{}, err
		}
		if len(encoded) > operatortool.WorkHistoryPageBytes {
			if len(page.Items) == 0 {
				return tracker.Page[tracker.CollaborationEvent]{}, operatortool.ErrReadUnavailable
			}
			hasMore = true
			break
		}
		page.Items = candidate.Items
		cursor = next
	}
	if hasMore {
		page.NextCursor, err = encodeNativeCursor(cursor, key)
		if err != nil {
			return tracker.Page[tracker.CollaborationEvent]{}, err
		}
	}
	return page, nil
}
