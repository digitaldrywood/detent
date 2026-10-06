package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type bindNativeRepositoryRequest struct {
	tracker.Mutation
	ExpectedRevision tracker.Revision `json:"expected_revision,string"`
	Repository       string           `json:"repository"`
	Source           string           `json:"source,omitempty"`
}

func (s *Service) bindNativeRepository(c echo.Context) error {
	var request bindNativeRepositoryRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	result, err := s.bindNativeRepositoryCommand(c.Request().Context(), nativeRequestScope(c), nativeCommandOptions{OperationID: c.Request().Method + " " + c.Request().URL.EscapedPath(), Feature: hostedMutationFeature(c)}, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}
func (s *Service) bindNativeRepositoryCommand(ctx context.Context, scope nativeScope, options nativeCommandOptions, request bindNativeRepositoryRequest) (json.RawMessage, error) {
	if request.Source == "" {
		if replay, found, err := s.nativeCommandReplay(ctx, scope, options.OperationID, request.IdempotencyKey, request); found || err != nil {
			return replay, err
		}
	}

	owner, name, valid := splitRepositoryFullName(request.Repository)
	if !valid {
		return nil, nativeInvalid("Repository must be owner/name")
	}
	if request.Source == "runner_checkout" {
		return s.bindRunnerCheckoutCommand(ctx, scope, options, request.Mutation, request.ExpectedRevision, owner+"/"+name)
	}
	if request.Source != "" {
		return nil, nativeInvalid("Repository source is invalid")
	}
	if s.config.ReconcileBackend == nil {
		return nil, nativeInvalid("GitHub repository transport is not configured")
	}
	current, err := readProjectIntegration(ctx, s.database.db, scope)
	if err != nil {
		return nil, err
	}
	if current.Repository != "" && strings.EqualFold(current.Repository, request.Repository) {
		return json.Marshal(current)
	}
	if current.Revision != request.ExpectedRevision {
		return nil, nativeConflict(current.Revision)
	}
	if current.Profile != "native" || current.RepositoryID != 0 {
		return nil, nativeInvalid("Only an unbound native project can attach a repository; existing bindings are immutable")
	}
	snapshot, err := s.config.ReconcileBackend.Reconcile(ctx, ReconcileRequest{Repository: RepositoryTarget{Owner: owner, Name: name}, Profile: "native", SkipIssues: true, SkipRepository: true})
	if err != nil {
		return nil, err
	}
	if err := validateReconcileSnapshot(ReconcileSnapshot{Repository: snapshot.Repository}); err != nil {
		return nil, err
	}
	return s.executeNativeMutation(ctx, scope, options, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
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
		if err := ensureNativeTriage(ctx, tx, scope, now); err != nil {
			return nil, err
		}
		return readProjectIntegration(ctx, tx, scope)
	})
}
