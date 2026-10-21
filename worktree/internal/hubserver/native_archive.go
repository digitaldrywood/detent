package hubserver

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type archiveIssueRequest struct {
	tracker.Mutation
	ExpectedRevision tracker.Revision `json:"expected_revision,string"`
}

func (s *Service) archiveNativeIssue(c echo.Context) error { return s.setNativeArchive(c, true) }
func (s *Service) restoreNativeIssue(c echo.Context) error { return s.setNativeArchive(c, false) }

func (s *Service) setNativeArchive(c echo.Context, archived bool) error {
	var request archiveIssueRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	result, err := s.setNativeArchiveCommand(c.Request().Context(), nativeRequestScope(c), c.Param("item"), request, archived)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}

func readNativeArchiveIssue(ctx context.Context, query nativeQueryer, scope nativeScope, item string, expected tracker.Revision, archived bool, now time.Time) (tracker.NativeIssue, error) {
	issue, id, err := readNativeIssue(ctx, query, scope, item)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	if err := requireNativeEdit(issue, expected); err != nil {
		return tracker.NativeIssue{}, err
	}
	if scope.credential.Scope == apiScopeWorker {
		return tracker.NativeIssue{}, nativeInvalid("Archive and restore require an operator")
	}
	if archived && !issue.Archived {
		lease, found, err := readUnreleasedLease(ctx, query, id)
		if err != nil {
			return tracker.NativeIssue{}, err
		}
		if found && lease.session.ExpiresAt.After(now) {
			return tracker.NativeIssue{}, fmt.Errorf("%w: finish or stop the active work before archiving", tracker.ErrLeaseConflict)
		}
	}
	return issue, nil
}

func setNativeArchiveTx(ctx context.Context, tx *sql.Tx, scope nativeScope, item string, expected tracker.Revision, archived bool, now time.Time) (tracker.NativeIssue, error) {
	issue, err := readNativeArchiveIssue(ctx, tx, scope, item, expected, archived, now)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	if issue.Archived == archived {
		return issue, nil
	}
	issue.Archived = archived
	operation := "restore"
	if archived {
		operation = "archive"
	}
	return persistNativeIssue(ctx, tx, scope, issue, "issue.edited", tracker.CollaborationData{Fields: []string{"archived"}, Operation: operation}, now)
}
