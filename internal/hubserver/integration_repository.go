package hubserver

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) bindNativeRepository(c echo.Context) error {
	var request struct {
		tracker.Mutation
		ExpectedRevision tracker.Revision `json:"expected_revision,string"`
		Repository       string           `json:"repository"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	owner, name, valid := splitRepositoryFullName(request.Repository)
	if !valid {
		return s.nativeAPIError(c, nativeInvalid("Repository must be owner/name"))
	}
	if s.config.ReconcileBackend == nil {
		return s.nativeAPIError(c, nativeInvalid("GitHub repository transport is not configured"))
	}
	ctx, scope := c.Request().Context(), nativeRequestScope(c)
	current, err := readProjectIntegration(ctx, s.database.db, scope)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if current.Repository != "" && strings.EqualFold(current.Repository, request.Repository) {
		return c.JSON(http.StatusOK, current)
	}
	if current.Revision != request.ExpectedRevision {
		return s.nativeAPIError(c, nativeConflict(current.Revision))
	}
	if current.Profile != "native" || current.RepositoryID != 0 {
		return s.nativeAPIError(c, nativeInvalid("Only an unbound native project can attach a repository; existing bindings are immutable"))
	}
	snapshot, err := s.config.ReconcileBackend.Reconcile(ctx, ReconcileRequest{Repository: RepositoryTarget{Owner: owner, Name: name}, Profile: "native", SkipIssues: true, SkipRepository: true})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := validateReconcileSnapshot(ReconcileSnapshot{Repository: snapshot.Repository}); err != nil {
		return s.nativeAPIError(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		current, err := readProjectIntegration(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		if current.Revision != request.ExpectedRevision {
			return nil, nativeConflict(current.Revision)
		}
		if err := requireIntegrationIdle(ctx, tx, scope, now); err != nil {
			return nil, err
		}
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM repositories WHERE github_node_id = ? OR (lower(github_owner) = lower(?) AND lower(github_name) = lower(?))", snapshot.Repository.NodeID, snapshot.Repository.Owner, snapshot.Repository.Name).Scan(&exists); err != nil {
			return nil, err
		}
		if exists != 0 {
			return nil, nativeInvalid("Repository already belongs to another project; use that project's explicit cutover")
		}
		repository := normalizedRepository{NodeID: snapshot.Repository.NodeID, DatabaseID: snapshot.Repository.DatabaseID, Owner: snapshot.Repository.Owner, Name: snapshot.Repository.Name}
		stamp, err := newSourceStamp(snapshot.Repository.UpdatedAt, repository)
		if err != nil {
			return nil, err
		}
		id, _, err := applyRepositoryProjection(ctx, tx, repository, stamp, now, false, true)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM projects WHERE repository_id = ?", id); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE projects SET repository_id = ?, integration_revision = integration_revision + 1 WHERE id = ?", id, scope.project); err != nil {
			return nil, err
		}
		return readProjectIntegration(ctx, tx, scope)
	})
}
