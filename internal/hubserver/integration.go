package hubserver

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type ProjectIntegration struct {
	Profile           string            `json:"profile"`
	Revision          tracker.Revision  `json:"revision,string"`
	Intake            string            `json:"intake"`
	Projection        string            `json:"projection"`
	RepositoryEnabled bool              `json:"repository_enabled"`
	Repository        string            `json:"repository,omitempty"`
	Authority         map[string]string `json:"authority"`
	RepositoryID      int64             `json:"-"`
}

type GitHubRequestCount struct {
	Profile       string    `json:"profile"`
	Operation     string    `json:"operation"`
	Requests      int64     `json:"requests"`
	Errors        int64     `json:"errors"`
	LastRequestAt time.Time `json:"last_request_at"`
}

func (s *Service) githubRequestCounts(c echo.Context) error {
	counts := []GitHubRequestCount{}
	if s.config.GitHubRequestCounts != nil {
		counts = s.config.GitHubRequestCounts()
	}
	return c.JSON(http.StatusOK, counts)
}

func readProjectIntegration(ctx context.Context, query nativeQueryer, scope nativeScope) (ProjectIntegration, error) {
	var result ProjectIntegration
	err := query.QueryRowContext(ctx, `SELECT p.profile, p.integration_revision, p.github_intake, p.github_projection,
p.github_repository_enabled, COALESCE(r.github_owner || '/' || r.github_name, ''), COALESCE(r.id, 0)
FROM projects p LEFT JOIN repositories r ON r.id = p.repository_id WHERE p.organization_id = ? AND p.id = ?`, scope.organization, scope.project).Scan(
		&result.Profile, &result.Revision, &result.Intake, &result.Projection, &result.RepositoryEnabled, &result.Repository, &result.RepositoryID)
	owner := "detent"
	if result.Profile == "github_compatible" {
		owner = "github"
	}
	result.Authority = map[string]string{"title": owner, "body": owner, "discussion": owner, "dependencies": owner, "authors": owner, "source_timestamps": "source", "workflow": owner, "labels": owner, "assignees": owner, "priority": owner, "scheduling": "detent", "progress": "detent", "repository_policy": "trusted_repository_revision", "github_merge": "github_branch_protections_and_fresh_checks", "native_approval": "detent_only"}
	return result, err
}

func (s *Service) getProjectIntegration(c echo.Context) error {
	result, err := readProjectIntegration(c.Request().Context(), s.database.db, nativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) getCutoverReceipt(c echo.Context) error {
	var raw string
	if err := s.database.db.QueryRowContext(c.Request().Context(), "SELECT receipt_json FROM github_cutovers WHERE project_id = ?", nativeRequestScope(c).project).Scan(&raw); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, []byte(raw))
}

func (s *Service) updateProjectIntegration(c echo.Context) error {
	var request struct {
		tracker.Mutation
		ExpectedRevision  tracker.Revision `json:"expected_revision,string"`
		Intake            string           `json:"intake"`
		Projection        string           `json:"projection"`
		RepositoryEnabled bool             `json:"repository_enabled"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		current, err := readProjectIntegration(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		if current.Revision != request.ExpectedRevision {
			return nil, nativeConflict(current.Revision)
		}
		if (request.Intake != "disabled" && request.Intake != "manual") || (request.Projection != "disabled" && request.Projection != "summary") {
			return nil, nativeInvalid("Intake must be disabled or manual; projection must be disabled or summary")
		}
		if current.RepositoryID == 0 && (request.Intake != "disabled" || request.Projection != "disabled" || request.RepositoryEnabled) {
			return nil, nativeInvalid("Attach a GitHub repository before enabling intake, summaries or repository integration")
		}
		if current.Profile == "github_compatible" && request.Projection != "disabled" {
			return nil, nativeInvalid("Summary projection requires native authority; use explicit cutover first")
		}
		if err := requireIntegrationIdle(ctx, tx, scope, now); err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE projects SET integration_revision = integration_revision + 1, github_intake = ?, github_projection = ?, github_repository_enabled = ? WHERE id = ?`, request.Intake, request.Projection, request.RepositoryEnabled, scope.project)
		if err != nil {
			return nil, err
		}
		if err := supersedeDisallowedOutbox(ctx, tx, scope, now); err != nil {
			return nil, err
		}
		return readProjectIntegration(ctx, tx, scope)
	})
}

func requireIntegrationIdle(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) error {
	var active int
	err := tx.QueryRowContext(ctx, `SELECT
(SELECT count(*) FROM leases l JOIN issues i ON i.id = l.issue_id WHERE i.project_id = ? AND l.released_at IS NULL AND julianday(l.expires_at) > julianday(?)) +
(SELECT count(*) FROM github_outbox o JOIN issues i ON i.id = o.issue_id WHERE i.project_id = ? AND o.status = 'processing')`, scope.project, formatHubTime(now), scope.project).Scan(&active)
	if err != nil {
		return err
	}
	if active != 0 {
		return nativeInvalid("Finish active leases and processing GitHub writes before changing integration authority")
	}
	return nil
}

func supersedeDisallowedOutbox(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE github_outbox SET status = 'superseded', completed_at = ?, updated_at = ?
WHERE issue_id IN (SELECT i.id FROM issues i JOIN projects p ON p.id = i.project_id WHERE p.id = ? AND
((p.profile = 'native' AND (mutation_kind = 'workflow_label' OR (mutation_kind = 'workpad' AND (p.github_projection = 'disabled' OR COALESCE(json_extract(desired_json, '$.summary'), 0) = 0)))) OR (mutation_kind = 'merge_pull_request' AND p.github_repository_enabled = 0)))
AND status IN ('pending', 'retrying')`, formatOutboxTime(now), formatOutboxTime(now), scope.project)
	return err
}

func repositoryOwnership(ctx context.Context, query nativeQueryer, id int64) (string, bool, error) {
	var profile string
	var enabled bool
	err := query.QueryRowContext(ctx, "SELECT profile, github_repository_enabled FROM projects WHERE repository_id = ?", id).Scan(&profile, &enabled)
	return profile, enabled, err
}

func (s *Service) registerIntegrationRoutes(e *echo.Echo) {
	e.GET("/api/v2/github/requests", s.githubRequestCounts, s.requireInstanceAdmin())
	read := s.requireNativeScope(apiScopeWorker, apiScopeOperator)
	operator := s.requireNativeScope(apiScopeOperator)
	admin := s.requireNativeScope(apiScopeAdmin)
	e.GET(nativeBase+"/integration", s.getProjectIntegration, read)
	e.PUT(nativeBase+"/integration", s.updateProjectIntegration, admin)
	e.POST(nativeBase+"/integration/repository", s.bindNativeRepository, admin)
	e.POST(nativeBase+"/integration/cutover", s.cutoverProject, admin)
	e.GET(nativeBase+"/integration/cutover", s.getCutoverReceipt, read)
	e.POST(nativeBase+"/imports", s.startGitHubImport, operator)
	e.GET(nativeBase+"/imports/:import", s.getGitHubImport, read)
	e.POST(nativeBase+"/imports/:import/advance", s.advanceGitHubImport, operator)
	e.GET(nativeBase+"/imports/:import/records", s.listGitHubImportRecords, read)
	e.POST(nativeBase+"/work-items/:item/projection", s.projectNativeSummary, operator)
}
