package hubserver

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) nativePage(c echo.Context) (int, nativeCursor, []byte, error) {
	params, err := nativeReadQuery(c)
	if err != nil {
		return 0, nativeCursor{}, nil, err
	}
	return s.readNativePage(c.Request().Context(), hubscope.NativeRequestScope(c), c.Request().URL.EscapedPath(), params)
}

func nativeReadQuery(c echo.Context) (url.Values, error) {
	params, err := url.ParseQuery(c.Request().URL.RawQuery)
	if err != nil {
		return nil, hubapi.NativePublicInvalid("Query is invalid")
	}
	return params, nil
}

func (s *Service) readNativePage(ctx context.Context, scope hubscope.NativeScope, path string, params url.Values) (int, nativeCursor, []byte, error) {
	return s.readNativePageLimit(ctx, scope, path, params, hubapi.MaxAPIPageLimit, hubapi.DefaultAPIPageLimit)
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
		return "", "", hubapi.NativePublicInvalid("direction supports oldest or newest")
	}
}

func nativeCreatedBy(scope hubscope.NativeScope, params url.Values) (string, error) {
	value := strings.TrimSpace(params.Get("created_by"))
	if len(value) > 256 {
		return "", hubapi.NativePublicInvalid("created_by is invalid")
	}
	if value != "me" {
		return value, nil
	}
	actor := scope.Actor()
	if actor.Kind != "human" || actor.PrincipalID == "" {
		return "", hubapi.NativePublicInvalid("created_by=me requires a human identity")
	}
	return actor.PrincipalID, nil
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
			return false, hubapi.NativePublicInvalid("include supports workspace,work,summary,board")
		}
	}
	return slices.ContainsFunc(strings.Split(value, ","), func(name string) bool { return strings.TrimSpace(name) == "workspace" }), nil
}

func (s *Service) listNativeIssues(c echo.Context) error {
	ctx := c.Request().Context()
	scope := hubscope.NativeRequestScope(c)
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

func (s *Service) readIssues(ctx context.Context, scope hubscope.NativeScope, params url.Values) (tracker.NativeIssuePage, error) {
	return s.readBoardProjection(ctx, scope, params, false, false, true)
}

func (s *Service) readIssuesProjection(ctx context.Context, scope hubscope.NativeScope, params url.Values, compact bool) (tracker.NativeIssuePage, error) {
	return s.readBoardProjection(ctx, scope, params, false, compact, false)
}

func (s *Service) listNativeComments(c echo.Context) error {
	ctx := c.Request().Context()
	scope := hubscope.NativeRequestScope(c)
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

func (s *Service) readComments(ctx context.Context, scope hubscope.NativeScope, item string, params url.Values) (tracker.Page[tracker.NativeComment], error) {
	return s.readCommentRecords(ctx, scope, item, params)
}

func (s *Service) listNativeHistory(c echo.Context) error {
	ctx := c.Request().Context()
	scope := hubscope.NativeRequestScope(c)
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

func (s *Service) readHistory(ctx context.Context, scope hubscope.NativeScope, item string, params url.Values) (tracker.Page[tracker.CollaborationEvent], error) {
	return s.readHistoryTenant(ctx, scope, item, params)
}
