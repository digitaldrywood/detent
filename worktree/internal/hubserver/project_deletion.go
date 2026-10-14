package hubserver

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type deleteCloudProjectRequest struct {
	tracker.Mutation
	ConfirmName string `json:"confirm_name"`
}

func (s *Service) deleteCloudProject(c echo.Context) error {
	scope := nativeRequestScope(c)
	if s.config.Hosted == nil || scope.credential.Hosted == nil {
		return s.nativeAPIError(c, nativeNotFound())
	}
	scope.requireHostedAdmin = true
	c.Set("native_scope", scope)
	var request deleteCloudProjectRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	return s.nativeMutationStatus(c, http.StatusOK, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		project, err := readNativeProject(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		if request.ConfirmName != project.Name {
			return nil, &nativeError{Code: "invalid_request", Message: "Confirm the current project name before deleting it", status: http.StatusUnprocessableEntity, publicMessage: true}
		}
		if err := requireProjectDeletionIdle(ctx, tx, scope, now); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_secret_audit (organization_id,project_id,actor,kind,key_version,event,recorded_at)
SELECT organization_id,project_id,?,kind,master_key_version,'remove',? FROM project_secrets WHERE organization_id=? AND project_id=?`, scope.credential.ID, formatHubTime(now), scope.organization, scope.project); err != nil {
			return nil, err
		}
		for _, query := range []string{
			"DELETE FROM project_secrets WHERE organization_id=? AND project_id=?",
			"DELETE FROM hosted_project_grants WHERE organization_id=? AND project_id=?",
			"DELETE FROM token_grants WHERE organization_id=? AND project_id=?",
			"DELETE FROM runner_enrollment_projects WHERE organization_id=? AND project_id=?",
			"DELETE FROM artifact_grants WHERE organization_id=? AND project_id=?",
		} {
			if _, err := tx.ExecContext(ctx, query, scope.organization, scope.project); err != nil {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM runner_checkout_repositories WHERE project_id=?", scope.project); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE projects SET deleted_at=?, github_repository_enabled=0, github_intake='disabled' WHERE organization_id=? AND id=? AND deleted_at IS NULL", formatHubTime(now), scope.organization, scope.project); err != nil {
			return nil, err
		}
		return struct {
			ProjectID tracker.ProjectID `json:"project_id"`
		}{scope.project}, nil
	})
}

func requireProjectDeletionIdle(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) error {
	queries := []struct {
		query string
		args  []any
	}{
		{`SELECT EXISTS(SELECT 1 FROM leases l JOIN issues i ON i.id=l.issue_id WHERE i.organization_id=? AND i.project_id=? AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?))`, []any{scope.organization, scope.project, formatHubTime(now)}},
		{`SELECT EXISTS(SELECT 1 FROM native_attempts WHERE organization_id=? AND project_id=? AND status='running')`, nil},
		{`SELECT EXISTS(SELECT 1 FROM workspace_sessions WHERE organization_id=? AND project_id=? AND state NOT IN ('closed','failed'))`, nil},
		{`SELECT EXISTS(SELECT 1 FROM project_action_runs WHERE organization_id=? AND project_id=? AND status IN ('queued','running'))`, nil},
		{`SELECT EXISTS(SELECT 1 FROM conversations WHERE organization_id=? AND project_id=? AND json_extract(execution_json,'$.status') IN ('waiting_for_runner','starting','running','waiting_input','interrupting','unknown'))`, nil},
		{`SELECT EXISTS(SELECT 1 FROM conversation_messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.organization_id=? AND c.project_id=? AND m.delivery IN ('queued','sending','sent','delivered','responding','unknown'))`, nil},
		{`SELECT EXISTS(SELECT 1 FROM github_imports g JOIN projects p ON p.id=g.project_id WHERE p.organization_id=? AND p.id=? AND (g.status IN ('pending','running','partial') OR g.intake_pending=1))`, nil},
		{`SELECT EXISTS(SELECT 1 FROM github_outbox o JOIN issues i ON i.id=o.issue_id WHERE i.organization_id=? AND i.project_id=? AND o.status IN ('pending','retrying','processing'))`, nil},
		{`SELECT EXISTS(SELECT 1 FROM organization_sprite_members WHERE organization_id=? AND token_project_id=? AND state<>'deleted')`, nil},
	}
	for _, check := range queries {
		args := check.args
		if args == nil {
			args = []any{scope.organization, scope.project}
		}
		var active bool
		if err := tx.QueryRowContext(ctx, check.query, args...).Scan(&active); err != nil {
			return err
		}
		if active {
			return &nativeError{Code: "invalid_request", Message: "Finish or cancel active work, close workspaces, finish imports and GitHub writes, and remove Sprite pool members using this project's token before deleting this project", status: http.StatusUnprocessableEntity, publicMessage: true}
		}
	}
	return nil
}
