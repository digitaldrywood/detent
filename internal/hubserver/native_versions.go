package hubserver

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) getNativeVersion(c echo.Context) error {
	revision, err := strconv.ParseInt(c.Param("revision"), 10, 64)
	if err != nil || revision <= 0 {
		return s.nativeAPIError(c, nativeInvalid("Revision must be a positive decimal integer"))
	}
	scope := nativeRequestScope(c)
	recordID := c.Param("item")
	if c.Param("comment") != "" {
		recordID = c.Param("comment")
	}
	var body string
	err = s.database.db.QueryRowContext(c.Request().Context(), `SELECT record_json FROM collaboration_versions
WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND record_id = ? AND revision = ?`, scope.organization, scope.project, c.Param("item"), recordID, revision).Scan(&body)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if recordID == c.Param("item") && (scope.credential.Scope != apiScopeAdmin || scope.credential.NativeOnly) {
		var issue tracker.NativeIssue
		if err := json.Unmarshal([]byte(body), &issue); err != nil {
			return s.nativeAPIError(c, err)
		}
		visible := make(map[tracker.NativeWorkItemID]bool, len(issue.Dependencies))
		for _, id := range issue.Dependencies {
			var count int
			err := s.database.db.QueryRowContext(c.Request().Context(), `SELECT count(*) FROM issues i JOIN token_grants g ON g.organization_id = i.organization_id AND g.project_id = i.project_id
WHERE i.organization_id = ? AND i.native_id = ? AND g.token_id = ?`, scope.organization, id, scope.credential.ID).Scan(&count)
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			visible[id] = count != 0
		}
		issue.Dependencies = slices.DeleteFunc(issue.Dependencies, func(id tracker.NativeWorkItemID) bool { return !visible[id] })
		issue.Blockers = slices.DeleteFunc(issue.Blockers, func(dependency tracker.NativeDependency) bool { return !visible[dependency.ID] })
		return c.JSON(http.StatusOK, issue)
	}
	return c.JSONBlob(http.StatusOK, []byte(body))
}
