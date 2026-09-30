package hubserver

import (
	"context"
	"database/sql"
	"fmt"
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
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		issue, id, err := readNativeIssue(ctx, tx, scope, c.Param("item"))
		if err != nil {
			return nil, err
		}
		if err := requireNativeEdit(issue, request.ExpectedRevision); err != nil {
			return nil, err
		}
		if scope.credential.Scope == apiScopeWorker {
			return nil, nativeInvalid("Archive and restore require an operator")
		}
		if issue.Archived == archived {
			return issue, nil
		}
		if archived {
			lease, found, err := readUnreleasedLease(ctx, tx, id)
			if err != nil {
				return nil, err
			}
			if found && lease.session.ExpiresAt.After(now) {
				return nil, fmt.Errorf("%w: finish or stop the active work before archiving", tracker.ErrLeaseConflict)
			}
		}
		issue.Archived = archived
		operation := "restore"
		if archived {
			operation = "archive"
		}
		return persistNativeIssue(ctx, tx, scope, issue, "issue.edited", tracker.CollaborationData{Fields: []string{"archived"}, Operation: operation}, now)
	})
}
