package hubserver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
	key, err := s.readNativeCursorKey(ctx, scope.organization)
	if err != nil {
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

func (s *Service) readNativeCursorKey(ctx context.Context, organization tracker.OrganizationID) ([]byte, error) {
	if s.config.AccessRepository == nil {
		return nil, nativeRunEventStorageUnavailable()
	}
	var secret accessstore.Secret
	if err := s.accessRead(ctx, organization, func(r tenantstore.Reader) error {
		var err error
		secret, err = r.AccessSecret(ctx, "", accessstore.NativeCursorKeyKind)
		return err
	}); err != nil {
		return nil, err
	}
	key, err := accessstore.OpenSecret(s.config.SecretKeys, secret)
	if err != nil {
		return nil, err
	}
	if len(key) != sha256.Size {
		return nil, errors.New("native cursor key has invalid length")
	}
	return key, nil
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
	return s.readBoardRecords(ctx, scope, params)
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
	return s.readCommentRecords(ctx, scope, item, params)
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
	return s.readHistoryTenant(ctx, scope, item, params)
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
