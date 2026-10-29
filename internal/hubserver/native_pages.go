package hubserver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	Version    int                    `json:"v"`
	Scope      string                 `json:"scope"`
	After      string                 `json:"after"`
	Expires    int64                  `json:"expires"`
	Issues     *nativeIssuePageCursor `json:"issues,omitempty"`
	Compaction int64                  `json:"compaction,omitempty"`
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
		return nil, nativePublicInvalid("Query is invalid")
	}
	return params, nil
}

func (s *Service) readNativePage(ctx context.Context, scope nativeScope, path string, params url.Values) (int, nativeCursor, []byte, error) {
	return s.readNativePageLimit(ctx, scope, path, params, maxAPIPageLimit, defaultAPIPageLimit)
}

func (s *Service) readNativePageLimit(ctx context.Context, scope nativeScope, path string, params url.Values, maximum, defaultLimit int) (int, nativeCursor, []byte, error) {
	ctx = s.runDetailContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	limit := defaultLimit
	var err error
	if value := strings.TrimSpace(params.Get("limit")); value != "" {
		limit, err = strconv.Atoi(value)
	}
	if limit < 1 || limit > maximum {
		err = nativePublicInvalid("Page limit is invalid")
	}
	if err != nil {
		return 0, nativeCursor{}, nil, nativePublicInvalid("Page limit is invalid")
	}
	requestedCursor := params.Get("cursor")
	cloned := url.Values{}
	for k, v := range params {
		cloned[k] = v
	}
	params = cloned
	params.Del("cursor")
	params.Del("limit")
	params.Del("archive_url")

	fingerprint := scope.credential.ID + " " + path + "?" + params.Encode()
	if scope.credential.HostedRole != "" {
		fingerprint += " " + scope.credential.HostedRole + " " + scope.actor().PrincipalID
	}
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
		return 0, cursor, nil, nativePublicInvalid("Cursor is invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return 0, cursor, nil, nativePublicInvalid("Cursor is invalid")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, cursor, nil, nativePublicInvalid("Cursor is invalid")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return 0, cursor, nil, nativePublicInvalid("Cursor signature is invalid")
	}
	if err := json.Unmarshal(payload, &cursor); err != nil || cursor.Version != 2 || cursor.Scope != fingerprint || cursor.Expires <= s.config.now().Unix() {
		return 0, cursor, nil, nativePublicInvalid("Cursor scope changed or expired; restart the query")
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
	fields = append(fields, "archive_url")
	if value, present := params["archive_url"]; present && (len(value) != 1 || value[0] != "true" && value[0] != "false") {
		return nativePublicInvalid("archive_url supports true or false")
	}
	allowed := map[string]bool{"limit": true, "cursor": true}
	for _, field := range fields {
		allowed[field] = true
	}
	for key, values := range params {
		if !allowed[key] || len(values) != 1 || len(values[0]) > 4096 {
			return nativePublicInvalid("Query contains an unsupported field or value")
		}
	}
	return nil
}

func nativePageOrder(params url.Values, column, after string) (string, string, error) {
	switch params.Get("direction") {
	case "", "oldest":
		return column + " > CAST(? AS INTEGER) ORDER BY " + column, after, nil
	case "newest":
		if after == "" {
			after = "9223372036854775807"
		}
		return column + " < CAST(? AS INTEGER) ORDER BY " + column + " DESC", after, nil
	default:
		return "", "", nativePublicInvalid("direction supports oldest or newest")
	}
}

func nativeCreatedBy(scope nativeScope, params url.Values) (string, error) {
	value := strings.TrimSpace(params.Get("created_by"))
	if len(value) > 256 {
		return "", nativePublicInvalid("created_by is invalid")
	}
	if value != "me" {
		return value, nil
	}
	actor := scope.actor()
	if actor.Kind != "human" || actor.PrincipalID == "" {
		return "", nativePublicInvalid("created_by=me requires a human identity")
	}
	return actor.PrincipalID, nil
}

func validateNativeIssueQuery(params url.Values) error {
	single := url.Values{}
	for key, values := range params {
		switch key {
		case "type":
			if len(values) > 2 {
				return nativePublicInvalid("type must be bug or change_request")
			}
			for _, value := range values {
				if err := validateNativeIssueType(value, true); err != nil {
					return err
				}
			}
		case "verification":
			if len(values) > 2 {
				return nativeVerificationFilterInvalid()
			}
			for _, value := range values {
				if value != tracker.VerificationPending && value != tracker.VerificationVerified {
					return nativeVerificationFilterInvalid()
				}
			}
		case "state", "label", "assignee", "priority":
			if len(values) > 32 || len(strings.Join(values, "\x00")) > 4096 {
				return nativePublicInvalid("Query contains an unsupported field or value")
			}
		default:
			single[key] = values
		}
	}
	return validateNativeQuery(single, "include", "archived", "q", "open", "fingerprint", "completed_window", "completed_from", "completed_to", "sort", "created_after", "created_before", "updated_after", "created_by", "fields")
}

// parseNativeIssueIncludes reads the include query and reports whether it asks
// for workspace items. Unknown members are refused rather than ignored so a
// client typo is visible.
func parseNativeIssueIncludes(value string) (bool, error) {
	if value == "" {
		return false, nil
	}
	for name := range strings.SplitSeq(value, ",") {
		if name = strings.TrimSpace(name); name != "workspace" && name != "work" && name != "summary" && name != "board" {
			return false, nativePublicInvalid("include supports workspace,work,summary,board")
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
	if params.Get("fields") == nativeIssuePickerFields {
		return c.JSON(http.StatusOK, nativeIssuePickerResponse(page))
	}
	return c.JSON(http.StatusOK, page)
}

func (s *Service) readIssues(ctx context.Context, scope nativeScope, params url.Values) (tracker.NativeIssuePage, error) {
	return s.readIssuesProjection(ctx, scope, params, false)
}

func (s *Service) readIssuesProjection(ctx context.Context, scope nativeScope, params url.Values, compact bool) (tracker.NativeIssuePage, error) {
	ctx = s.runDetailContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	picker, err := nativeIssuePickerRequested(params)
	if err != nil {
		return tracker.NativeIssuePage{}, err
	}
	if slices.Contains(strings.Split(params.Get("include"), ","), "board") {
		if params.Has("created_after") || params.Has("created_before") || params.Has("updated_after") {
			return tracker.NativeIssuePage{}, nativePublicInvalid("Date filters require an issue list projection")
		}
		return s.readBoardIssues(ctx, scope, params)
	}
	path := "/api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(string(scope.project)) + "/work-items"

	if err := validateNativeIssueQuery(params); err != nil {
		return tracker.NativeIssuePage{}, err
	}
	if _, err := nativeShippedWindow(params.Get("completed_window")); err != nil {
		return tracker.NativeIssuePage{}, err
	}
	if sort := params.Get("sort"); sort != "" && sort != "closed" {
		return tracker.NativeIssuePage{}, nativePublicInvalid("sort supports closed")
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

	query, args, err := nativeIssueSelection(scope, params, includeWorkspace)
	if err != nil {
		return tracker.NativeIssuePage{}, err
	}
	searchQuery := strings.TrimSpace(params.Get("q"))
	workIncluded := slices.ContainsFunc(strings.Split(params.Get("include"), ","), func(name string) bool { return strings.TrimSpace(name) == "work" })
	if summary && workIncluded {
		return tracker.NativeIssuePage{}, nativePublicInvalid("summary cannot include work")
	}

	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return tracker.NativeIssuePage{}, err
	}
	defer tx.Rollback()
	watermark, _, err := readDashboardWatermark(ctx, tx, scope)
	if err != nil {
		return tracker.NativeIssuePage{}, err
	}
	var work *tracker.NativeWorkSummary
	if workIncluded {
		work, err = readNativeWorkSummary(ctx, tx, scope, query, args, limit, s.config.now(), params.Get("completed_window"), searchQuery)
		if err != nil {
			return tracker.NativeIssuePage{}, err
		}
	}
	items, frozen, total, err := s.readIssuePageItems(ctx, tx, query, args, limit, &cursor, params.Get("sort"), searchQuery, scope)
	if err != nil {
		return tracker.NativeIssuePage{}, err
	}
	page := tracker.NativeIssuePage{Page: tracker.Page[tracker.NativeIssue]{Items: []tracker.NativeIssue{}}}
	page.Total = total
	page.ChangeSequence = watermark
	operational := map[string]tracker.NativeIssue{}
	if work != nil {
		for i, issue := range work.Items {
			work.Items[i] = s.nativeIssueResponse(issue)
			operational[string(issue.WorkItemID)] = work.Items[i]
		}
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	if picker {
		page.Items, err = readNativeIssuePickerRows(ctx, tx, scope, items)
		if err != nil {
			return tracker.NativeIssuePage{}, err
		}
		if len(items) > 0 {
			cursor.Issues = &items[len(items)-1].Position
		}
		items = nil
	}
	for _, item := range items {
		id := item.ID
		issue, loaded := operational[id]
		if !loaded {
			issue, _, err = readNativeIssueProjection(ctx, tx, scope, id, workIncluded || summary)
			if err != nil {
				return tracker.NativeIssuePage{}, err
			}
		}
		issue = s.nativeIssueResponse(issue)
		issue.ReportContext = nil
		issue.Snippet = item.Snippet
		issue.SearchScore = item.Position.Score
		next := cursor
		next.Issues = &item.Position
		if summary {
			issue = operatortool.NativeListIssue(issue)
		}
		if compact {
			transition, err := latestNativeRuntimeEvent(ctx, tx, scope, id, "workflow.transitioned", "")
			if err != nil {
				return tracker.NativeIssuePage{}, err
			}
			issue.LatestTransitionReason = transition.Data.ReasonDetail
			if issue.LatestTransitionReason == "" {
				issue.LatestTransitionReason = transition.Data.Reason
			}
		}
		if summary || workIncluded {
			candidate := tracker.Page[tracker.NativeIssue]{Items: append(page.Items, issue)}
			candidate.NextCursor, err = encodeNativeCursor(next, key)
			if err != nil {
				return tracker.NativeIssuePage{}, err
			}
			var projection any = operatortool.NativeItemPage(string(scope.project), candidate)
			if compact {
				projection = operatortool.NativeSummaryPageView(tracker.NativeIssuePage{Page: candidate, Total: total})
			}
			raw, encodeErr := json.Marshal(projection)
			if encodeErr != nil {
				return tracker.NativeIssuePage{}, encodeErr
			}
			if len(raw) > operatortool.WorkListPageBytes {
				if len(page.Items) > 0 {
					hasMore = true
					break
				}
				issue, err = operatortool.BoundNativeListIssue(issue)
				if err != nil {
					return tracker.NativeIssuePage{}, err
				}
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
	ctx = s.runDetailContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	path := "/api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(string(scope.project)) + "/work-items/" + url.PathEscape(item) + "/comments"

	if err := validateNativeQuery(params, "direction"); err != nil {
		return tracker.Page[tracker.NativeComment]{}, err
	}
	limit, cursor, key, err := s.readNativePage(ctx, scope, path, params)
	if err != nil {
		return tracker.Page[tracker.NativeComment]{}, err
	}

	if _, _, err := readNativeIssue(ctx, s.database.reader, scope, item); err != nil {
		return tracker.Page[tracker.NativeComment]{}, err
	}
	order, after, err := nativePageOrder(params, "sequence", cursor.After)
	if err != nil {
		return tracker.Page[tracker.NativeComment]{}, err
	}
	ids, err := nativePageIDs(ctx, s.database.reader, "SELECT id FROM native_comments WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND "+order+" LIMIT ?", scope.organization, scope.project, item, after, limit+1)
	if err != nil {
		return tracker.Page[tracker.NativeComment]{}, err
	}
	page := tracker.Page[tracker.NativeComment]{Items: []tracker.NativeComment{}}
	hasMore := len(ids) > limit
	if hasMore {
		ids = ids[:limit]
	}
	size := 8192
	for _, id := range ids {
		comment, err := readNativeComment(ctx, s.database.reader, scope, item, id)
		if err != nil {
			return tracker.Page[tracker.NativeComment]{}, err
		}
		encoded, err := json.Marshal(comment)
		if err != nil {
			return tracker.Page[tracker.NativeComment]{}, err
		}
		if size+len(encoded)+1 > operatortool.MaxResultBytes {
			if len(page.Items) == 0 {
				return tracker.Page[tracker.NativeComment]{}, nativePublicInvalid("A single comment exceeds the 256 KiB page budget after JSON encoding; use work_bulk_export to read the complete stored comment")
			}
			hasMore = true
			break
		}
		size += len(encoded) + 1
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
	return nativeArchiveJSON(c, http.StatusOK, page)
}

func (s *Service) readHistory(ctx context.Context, scope nativeScope, item string, params url.Values) (tracker.Page[tracker.CollaborationEvent], error) {
	ctx = s.runDetailContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	path := "/api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(string(scope.project)) + "/work-items/" + url.PathEscape(item) + "/history"

	if err := validateNativeQuery(params, "view", "direction"); err != nil {
		return tracker.Page[tracker.CollaborationEvent]{}, err
	}
	view := params.Get("view")
	if view != "" && view != "blockers" && view != "timeline" {
		return tracker.Page[tracker.CollaborationEvent]{}, nativePublicInvalid("view supports blockers or timeline")
	}
	if params.Get("direction") == "newest" && view == "" {
		return tracker.Page[tracker.CollaborationEvent]{}, nativePublicInvalid("Newest history requires the timeline or blockers view")
	}
	dataColumn := "data_json"
	switch view {
	case "timeline":
		dataColumn = "CASE WHEN type IN ('run.started','run.checkpointed','run.finished') THEN '{}' ELSE data_json END"
	case "blockers":
		dataColumn = "data_json,page_projection_ready,event_revision,from_state,to_state,reason,run_present,run_attempt_id,run_fencing_token"
	}
	limit, cursor, key, err := s.readNativePage(ctx, scope, path, params)
	if err != nil {
		return tracker.Page[tracker.CollaborationEvent]{}, err
	}

	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return tracker.Page[tracker.CollaborationEvent]{}, err
	}
	defer tx.Rollback()
	if _, _, err := readNativeIssueProjection(ctx, tx, scope, item, true); err != nil {
		return tracker.Page[tracker.CollaborationEvent]{}, err
	}
	summary, err := readEventCompaction(ctx, tx, scope.organization, scope.project, "work_item", item)
	if err != nil {
		return tracker.Page[tracker.CollaborationEvent]{}, err
	}
	var compaction *tracker.EventCompaction
	if summary.RemovedEvents > 0 {
		compaction = &summary
	}
	generation := summary.RemovedEvents
	if params.Get("cursor") != "" && cursor.Compaction != generation {
		return tracker.Page[tracker.CollaborationEvent]{}, nativePublicInvalid("History was compacted; restart the query")
	}
	cursor.Compaction = generation
	var after int64
	if cursor.After != "" {
		after, err = strconv.ParseInt(cursor.After, 10, 64)
		if err != nil {
			return tracker.Page[tracker.CollaborationEvent]{}, nativePublicInvalid("History cursor is invalid")
		}
	}
	var query nativeQueryer = tx
	if view == "" {
		query, err = archiveQuery(ctx, tx, item, "history", item, after+1)
		if err != nil {
			return tracker.Page[tracker.CollaborationEvent]{}, err
		}
	}
	order, boundary, err := nativePageOrder(params, "sequence", cursor.After)
	if err != nil {
		return tracker.Page[tracker.CollaborationEvent]{}, err
	}
	rows, err := query.QueryContext(ctx, `SELECT id, sequence, type, schema_version, actor_json, `+dataColumn+`, recorded_at FROM collaboration_events
WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND `+order+` LIMIT ?`, scope.organization, scope.project, item, boundary, limit+1)
	if err != nil {
		return tracker.Page[tracker.CollaborationEvent]{}, err
	}
	defer rows.Close()
	page := tracker.Page[tracker.CollaborationEvent]{Items: []tracker.CollaborationEvent{}, Compaction: compaction}
	for rows.Next() {
		event := tracker.CollaborationEvent{OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "work_item", AggregateID: tracker.NativeWorkItemID(item)}
		var actor, data, recorded string
		var ready, revision, run, fence sql.NullInt64
		var from, to, reason, attempt sql.NullString
		targets := []any{&event.ID, &event.AggregateSequence, &event.Type, &event.SchemaVersion, &actor, &data}
		if view == "blockers" {
			targets = append(targets, &ready, &revision, &from, &to, &reason, &run, &attempt, &fence)
		}
		targets = append(targets, &recorded)
		if err := rows.Scan(targets...); err != nil {
			return tracker.Page[tracker.CollaborationEvent]{}, err
		}
		if err := json.Unmarshal([]byte(actor), &event.Actor); err != nil {
			return tracker.Page[tracker.CollaborationEvent]{}, err
		}
		if view == "blockers" {
			if ready.Valid && ready.Int64 == 1 {
				event.Data = tracker.CollaborationData{Revision: tracker.Revision(revision.Int64), FromState: from.String, ToState: to.String, Reason: reason.String}
				if run.Int64 == 1 {
					event.Data.Run = &tracker.NativeRunData{AttemptID: attempt.String, FencingToken: tracker.FencingToken(fence.Int64)}
				}
			} else {
				var legacy struct {
					Revision tracker.Revision `json:"revision,string"`
					From     string           `json:"from_state"`
					To       string           `json:"to_state"`
					Reason   string           `json:"reason"`
					Run      *struct {
						Attempt string               `json:"attempt_id"`
						Fence   tracker.FencingToken `json:"fencing_token,string"`
					} `json:"run"`
				}
				if strings.HasPrefix(strings.TrimSpace(data), "{") {
					if err := json.Unmarshal([]byte(data), &legacy); err != nil {
						return tracker.Page[tracker.CollaborationEvent]{}, err
					}
				}
				event.Data = tracker.CollaborationData{Revision: legacy.Revision, FromState: legacy.From, ToState: legacy.To, Reason: legacy.Reason}
				if legacy.Run != nil {
					event.Data.Run = &tracker.NativeRunData{AttemptID: legacy.Run.Attempt, FencingToken: legacy.Run.Fence}
				}
			}
		} else {
			resolved, err := resolveStorageJSON(ctx, []byte(data))
			if err != nil {
				return tracker.Page[tracker.CollaborationEvent]{}, err
			}
			data = string(resolved)
			if err := decodeRunHistory(ctx, tx, data, &event.Data); err != nil {
				return tracker.Page[tracker.CollaborationEvent]{}, err
			}
			if event.Data.Run != nil {
				var stored struct {
					Run storedRun `json:"run"`
				}
				if err := json.Unmarshal([]byte(data), &stored); err != nil {
					return tracker.Page[tracker.CollaborationEvent]{}, err
				}
				if stored.Run.Detail != nil {
					data, err = marshalNative(event.Data)
					if err != nil {
						return tracker.Page[tracker.CollaborationEvent]{}, err
					}
				}
				event.Data.Run.Runtime = event.Data.Run.Runtime.WithoutActivitySpans()
			}
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
		if event.Data.RelatedWorkItemID == "" {
			continue
		}
		link, err := readNativeDependencyReference(ctx, tx, scope, event.Data.RelatedWorkItemID)
		if errors.Is(err, sql.ErrNoRows) {
			event.Data.RelatedWorkItemID = ""
			event.Data.RelatedIssue = nil
			continue
		}
		if err != nil {
			return tracker.Page[tracker.CollaborationEvent]{}, err
		}
		event.Data.RelatedIssue = link
	}
	hasMore := len(page.Items) > limit
	if state, ok := ctx.Value(nativeArchiveContextKey{}).(*nativeArchiveRead); ok && state.section != nil && state.last > 0 {
		through := state.last
		if len(page.Items) > 0 {
			through = max(through, page.Items[len(page.Items)-1].AggregateSequence)
		}
		var subsequent bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>?)
OR EXISTS(SELECT 1 FROM native_archive_sections WHERE organization_id=? AND project_id=? AND work_item_id=? AND kind='history' AND first_sequence>?)`, scope.organization, scope.project, item, through, scope.organization, scope.project, item, through).Scan(&subsequent)
		if err != nil {
			return tracker.Page[tracker.CollaborationEvent]{}, err
		}
		hasMore = hasMore || subsequent
	}
	items := page.Items[:min(len(page.Items), limit)]
	page.Items = []tracker.CollaborationEvent{}
	size := 8192
	for _, event := range items {
		next := cursor
		next.After = strconv.FormatInt(event.AggregateSequence, 10)
		encoded, err := json.Marshal(event)
		if err != nil {
			return tracker.Page[tracker.CollaborationEvent]{}, err
		}
		if size+len(encoded)+1 > operatortool.WorkHistoryPageBytes {
			if len(page.Items) == 0 {
				return tracker.Page[tracker.CollaborationEvent]{}, operatortool.ErrReadUnavailable
			}
			hasMore = true
			break
		}
		size += len(encoded) + 1
		page.Items = append(page.Items, event)
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

func nativeVerificationFilterInvalid() error {
	return &nativeError{Code: "invalid_request", Message: "verification must be pending or verified", status: http.StatusUnprocessableEntity, publicMessage: true}
}

func nativeIssueSelection(scope nativeScope, params url.Values, includeWorkspace bool) (string, []any, error) {
	query := `SELECT i.native_id FROM issues i LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id
WHERE i.organization_id = ? AND i.project_id = ? `
	args := []any{scope.organization, scope.project}
	searchQuery := strings.TrimSpace(params.Get("q"))
	if searchQuery != "" {
		expressions := nativeSearchExpressions(searchQuery)
		join, searchArgs := nativeSearchJoin(scope, params.Get("archived"), expressions, searchQuery, slices.ContainsFunc(strings.Split(params.Get("include"), ","), func(name string) bool { return strings.TrimSpace(name) == "work" }))
		number, project := nativeSearchNumber(searchQuery)
		query = `SELECT i.native_id FROM ` + join + ` CROSS JOIN issues i LEFT JOIN workflow_states ws ON ws.id=i.workflow_state_id ` + nativeNumberSearchJoin + `
 WHERE i.id=search.issue_id AND i.organization_id=? AND i.project_id=? `
		args = append(append(searchArgs, number, project, -float64(len(expressions))-1), args...)
	}
	var dateClauses strings.Builder
	for _, filter := range []struct{ name, column, operator string }{
		{"created_after", "native_created_at", ">="}, {"created_before", "native_created_at", "<"}, {"updated_after", "native_updated_at", ">="},
	} {
		if value := params.Get(filter.name); value != "" {
			date, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return "", nil, nativePublicInvalid("Date filters must be RFC3339 timestamps")
			}
			dateClauses.WriteString(" AND rtrim(i." + filter.column + ", 'Z') " + filter.operator + " ?")
			args = append(args, strings.TrimSuffix(formatHubTime(date), "Z"))
		}
	}
	query += dateClauses.String()
	visibility, visibilityArgs := scope.issueVisibilitySQL("i")
	query += " AND (" + visibility + ")"
	args = append(args, visibilityArgs...)
	createdBy, err := nativeCreatedBy(scope, params)
	if err != nil {
		return "", nil, err
	}
	if createdBy != "" {
		query += " AND " + nativeIssuePrincipal("i") + " = ?"
		args = append(args, createdBy)
	}
	switch params.Get("archived") {
	case "", "false":
		query += " AND i.archived = 0"
	case "true":
		query += " AND i.archived = 1"
	case "all":
	default:
		return "", nil, nativePublicInvalid("archived must be true, false or all")
	}
	switch params.Get("open") {
	case "":
	case "true":
		query += " AND ws.terminal = 0"
	case "false":
		query += " AND ws.terminal = 1"
	default:
		return "", nil, nativePublicInvalid("open must be true or false")
	}
	if value := strings.TrimSpace(params.Get("fingerprint")); value != "" {
		query += " AND (instr(i.body, ?) > 0 OR EXISTS (SELECT 1 FROM native_comments c WHERE c.work_item_id = i.native_id AND instr(c.body, ?) > 0))"
		marker := "\nfingerprint: " + value + "\n"
		args = append(args, marker, marker)
	}
	if !includeWorkspace {
		// Workspace items hold a worktree open for a person's surfaces, not
		// project work: the board would otherwise
		// fill with one card per opened Files panel.
		query += " AND " + notWorkspaceItemClause
	}
	var filterClauses strings.Builder
	for _, filter := range []struct{ name, clause string }{
		{"type", "i.issue_type = ?"},
		{"verification", "i.verification_state = ?"},
		{"state", "ws.detent_state = ?"},
		{"label", "EXISTS (SELECT 1 FROM native_issue_page_filter_values WHERE issue_id=i.id AND kind='label' AND value = ?)"},
		{"assignee", "EXISTS (SELECT 1 FROM native_issue_page_filter_values WHERE issue_id=i.id AND kind='assignee' AND value = ?)"},
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

	return query, args, nil
}
