package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
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
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	if _, _, err := readNativeIssue(ctx, s.database.reader, scope, item); err != nil {
		return nil, err
	}
	recordID := item
	if comment != "" {
		recordID = comment
	}
	var body string
	err := s.database.reader.QueryRowContext(ctx, `SELECT record_json FROM collaboration_versions
WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND record_id = ? AND revision = ?`, scope.organization, scope.project, item, recordID, revision).Scan(&body)
	if err != nil {
		return nil, err
	}
	if recordID == item && (scope.credential.Scope != apiScopeAdmin || scope.credential.NativeOnly) {
		var issue tracker.NativeIssue
		if err := json.Unmarshal([]byte(body), &issue); err != nil {
			return nil, err
		}
		for _, links := range []*[]tracker.NativeDependency{&issue.Blockers, &issue.Dependents, &issue.Related} {
			kept := []tracker.NativeDependency{}
			for _, link := range *links {
				_, err := readNativeDependencyReference(ctx, s.database.reader, scope, link.ID)
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				if err != nil {
					return nil, err
				}
				kept = append(kept, link)
			}
			*links = kept
		}
		issue.Dependencies = []tracker.NativeWorkItemID{}
		for _, link := range issue.Blockers {
			issue.Dependencies = append(issue.Dependencies, link.ID)
		}
		issue.BlockedReason.Text = ""
		issue.UnblocksReason.Text = ""
		path, err := readIssueBrowserPath(ctx, s.database.reader, string(issue.WorkItemID))
		if err != nil {
			return nil, err
		}
		issue.WebURL = path
		return json.Marshal(s.nativeIssueResponse(issue))
	}
	if recordID == item {
		var issue tracker.NativeIssue
		if err := json.Unmarshal([]byte(body), &issue); err != nil {
			return nil, err
		}
		issue.BlockedReason.Text, issue.UnblocksReason.Text = "", ""
		encoded, err := json.Marshal(issue)
		if err != nil {
			return nil, err
		}
		return s.nativeIssueJSON(ctx, encoded)
	}
	return json.RawMessage(body), nil
}
