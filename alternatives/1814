package hubserver

import (
	"context"
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
	body, err := s.readVersion(c.Request().Context(), nativeRequestScope(c), c.Param("item"), c.Param("comment"), revision)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, body)
}

func (s *Service) readVersion(ctx context.Context, scope nativeScope, item, comment string, revision int64) (json.RawMessage, error) {
	if _, _, err := readNativeIssue(ctx, s.database.db, scope, item); err != nil {
		return nil, err
	}
	recordID := item
	if comment != "" {
		recordID = comment
	}
	var body string
	err := s.database.db.QueryRowContext(ctx, `SELECT record_json FROM collaboration_versions
WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND record_id = ? AND revision = ?`, scope.organization, scope.project, item, recordID, revision).Scan(&body)
	if err != nil {
		return nil, err
	}
	if recordID == item && (scope.credential.Scope != apiScopeAdmin || scope.credential.NativeOnly) {
		var issue tracker.NativeIssue
		if err := json.Unmarshal([]byte(body), &issue); err != nil {
			return nil, err
		}
		visible := make(map[tracker.NativeWorkItemID]bool, len(issue.Dependencies))
		for _, id := range issue.Dependencies {
			var count int
			condition, grantArgs := scope.credential.projectGrantSQL("i.organization_id", "i.project_id")
			args := append([]any{scope.organization, id}, grantArgs...)
			err := s.database.db.QueryRowContext(ctx, `SELECT count(*) FROM issues i WHERE i.organization_id=? AND i.native_id=? AND (`+condition+`)`, args...).Scan(&count)
			if err != nil {
				return nil, err
			}
			visible[id] = count != 0
		}
		issue.Dependencies = slices.DeleteFunc(issue.Dependencies, func(id tracker.NativeWorkItemID) bool { return !visible[id] })
		issue.Blockers = slices.DeleteFunc(issue.Blockers, func(dependency tracker.NativeDependency) bool { return !visible[dependency.ID] })
		return json.Marshal(s.nativeIssueResponse(issue))
	}
	if recordID == item {
		return s.nativeIssueJSON(json.RawMessage(body))
	}
	return json.RawMessage(body), nil
}
