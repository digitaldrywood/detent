package hubserver

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

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
	return s.readBoardProjection(ctx, scope, params, false, compact, false)
}

func (s *Service) readIssueLanes(ctx context.Context, scope nativeScope, params url.Values) (tracker.NativeIssuePage, error) {
	return s.readBoardProjection(ctx, scope, params, false, false, true)
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
