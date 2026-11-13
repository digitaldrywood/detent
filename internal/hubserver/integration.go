package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ProjectIntegration = projectstore.Integration

func (s *Service) githubRequestCounts(c echo.Context) error {
	counts := []GitHubRequestCount{}
	if s.config.GitHubRequestCounts != nil {
		counts = s.config.GitHubRequestCounts()
	}
	return c.JSON(http.StatusOK, counts)
}

func readProjectIntegration(ctx context.Context, query nativeQueryer, scope nativeScope) (ProjectIntegration, error) {
	var result ProjectIntegration
	var states string
	err := query.QueryRowContext(ctx, `SELECT p.name, p.profile, p.integration_revision, p.github_intake, p.github_projection,
p.github_repository_enabled, COALESCE(r.github_owner || '/' || r.github_name, ''), COALESCE(r.id, 0), p.checkout_repository, p.states_json, p.workflow_source, p.workflow_source_revision, p.workflow_markdown, p.archive_completed_after_days, p.archive_cancelled_after_days, p.duplicate_challenge, p.aging_enabled
FROM projects p LEFT JOIN repositories r ON r.id = p.repository_id WHERE p.organization_id = ? AND p.id = ? AND p.deleted_at IS NULL`, scope.organization, scope.project).Scan(
		&result.Name, &result.Profile, &result.Revision, &result.Intake, &result.Projection, &result.RepositoryEnabled, &result.Repository, &result.RepositoryID, &result.CheckoutRepository, &states, &result.WorkflowSource, &result.WorkflowSourceRevision, &result.WorkflowMarkdown, &result.ArchiveCompletedAfterDays, &result.ArchiveCancelledAfterDays, &result.DuplicateChallenge, &result.AgingEnabled)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal([]byte(states), &result.States); err != nil {
		return result, err
	}
	icon, err := readProjectIcon(ctx, query, scope)
	if err != nil {
		return result, err
	}
	result.Icon = projectIconOverride(icon)
	result.ManualImportEnabled = result.Intake == "manual"
	if result.Profile == "native" && (result.Repository != "" || result.CheckoutRepository != "") {
		result.Intake = "automatic"
	}
	owner := "detent"
	if result.Profile == "github_compatible" {
		owner = "github"
	}
	result.Authority = map[string]string{"title": owner, "body": owner, "discussion": owner, "dependencies": owner, "authors": owner, "source_timestamps": "source", "workflow": owner, "labels": owner, "assignees": owner, "priority": owner, "scheduling": "detent", "progress": "detent", "repository_policy": "trusted_repository_revision", "github_merge": "github_branch_protections_and_fresh_checks", "native_approval": "detent_only"}
	if result.WorkflowSource != "" {
		result.Authority["workflow"] = "repository"
	}
	return result, err
}

func (s *Service) getProjectIntegration(c echo.Context) error {
	record, err := s.readProjectRecord(c.Request().Context(), nativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	value := projectIntegrationRecord(record)
	value.WorkflowMarkdown, err = s.projectRecordWorkflow(c.Request().Context(), record)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, value)
}

func (s *Service) getCutoverReceipt(c echo.Context) error {
	result, err := s.readCutoverReceipt(c.Request().Context(), nativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) projectIntegration(ctx context.Context, scope nativeScope) (ProjectIntegration, error) {
	record, err := s.readProjectRecord(ctx, scope)
	if err != nil {
		return ProjectIntegration{}, err
	}
	result := projectIntegrationRecord(record)
	result.WorkflowMarkdown, err = s.projectRecordWorkflow(ctx, record)
	if err != nil {
		return result, err
	}
	return result, s.applyProjectIntegrationLive(ctx, scope, &result)
}

func (s *Service) applyProjectIntegrationLive(ctx context.Context, scope nativeScope, result *ProjectIntegration) error {
	if result.CheckoutRepository != "" {
		verified, err := s.verifiedProjectCheckout(ctx, nil, scope, result.CheckoutRepository, s.config.now())
		if err != nil {
			return err
		}
		result.CheckoutVerified = verified
	}
	available := s.config.ReconcileBackend != nil
	result.GitHubTransportAvailable = &available
	repository := result.CheckoutRepository
	if repository == "" {
		repository = result.Repository
	}
	if available && result.Profile == "native" && repository != "" && s.config.GitHubAppInstallation != nil {
		app, err := s.config.GitHubAppInstallation(ctx, repository)
		if err != nil {
			return err
		}
		result.GitHubAppSlug, result.GitHubAppInstallURL = app.Slug, app.InstallURL
		result.GitHubAppInstalled = &app.Installed
	}
	return nil
}

type updateProjectIntegrationRequest struct {
	AgingEnabled              *bool           `json:"aging_enabled,omitempty"`
	DuplicateChallenge        *bool           `json:"duplicate_challenge,omitempty"`
	ArchiveCompletedAfterDays json.RawMessage `json:"archive_completed_after_days,omitempty"`
	ArchiveCancelledAfterDays json.RawMessage `json:"archive_cancelled_after_days,omitempty"`
	tracker.Mutation
	States            *[]tracker.NativeState `json:"states,omitempty"`
	WorkflowMarkdown  *string                `json:"workflow_markdown,omitempty"`
	ExpectedRevision  tracker.Revision       `json:"expected_revision,string"`
	Intake            string                 `json:"intake"`
	Projection        string                 `json:"projection"`
	RepositoryEnabled bool                   `json:"repository_enabled"`
}

func (s *Service) updateProjectIntegration(c echo.Context) error {
	var request updateProjectIntegrationRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope := nativeRequestScope(c)
	scope.requireHostedAdmin = true
	c.Set("native_scope", scope)
	return s.nativeMutation(c, request.Mutation, request, nil)
}

func archivePeriod(raw json.RawMessage, current *int) (*int, error) {
	if len(raw) == 0 {
		return current, nil
	}
	var days *int
	if err := json.Unmarshal(raw, &days); err != nil {
		return nil, nativePublicInvalid("Archive period must be 7, 14, 30, 60, 90 days or null")
	}
	if days != nil {
		switch *days {
		case 7, 14, 30, 60, 90:
		default:
			return nil, nativePublicInvalid("Archive period must be 7, 14, 30, 60, 90 days or null")
		}
	}
	return days, nil
}

func supersedeDisallowedOutbox(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT o.id,o.desired_json,o.desired_issue_number,o.desired_summary,p.github_projection FROM github_outbox o JOIN issues i ON i.id=o.issue_id JOIN projects p ON p.id=i.project_id WHERE p.id=? AND p.profile='native' AND o.mutation_kind='workpad' AND o.status IN ('pending','retrying') AND (o.desired_issue_number IS NULL OR o.desired_summary IS NULL)`, scope.project)
	if err != nil {
		return err
	}
	defer rows.Close()
	var legacy []int64
	for rows.Next() {
		var id int64
		var raw, projection string
		var number sql.NullInt64
		var summary sql.NullBool
		if err := rows.Scan(&id, &raw, &number, &summary, &projection); err != nil {
			return err
		}
		fields, err := evaluatedHostedImportFields(ctx, tx, "github_outbox", raw)
		if err != nil {
			return err
		}
		value := fields.outbox
		if number.Valid {
			value.IssueNumber = int(number.Int64)
		}
		if summary.Valid {
			value.Summary = summary.Bool
		}
		if (projection == "disabled" && value.IssueNumber == 0) || !value.Summary {
			legacy = append(legacy, id)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE github_outbox SET status='superseded',completed_at=?,updated_at=?
WHERE issue_id IN (SELECT i.id FROM issues i JOIN projects p ON p.id=i.project_id WHERE p.id=? AND
((p.profile='native' AND (mutation_kind='workflow_label' OR (mutation_kind='workpad' AND ((p.github_projection='disabled' AND desired_issue_number=0) OR desired_summary=0)))) OR (mutation_kind='merge_pull_request' AND p.github_repository_enabled=0)))
AND status IN ('pending','retrying')`, formatOutboxTime(now), formatOutboxTime(now), scope.project)
	if err != nil {
		return err
	}
	for _, id := range legacy {
		if _, err := tx.ExecContext(ctx, `UPDATE github_outbox SET status='superseded',completed_at=?,updated_at=? WHERE id=? AND status IN ('pending','retrying')`, formatOutboxTime(now), formatOutboxTime(now), id); err != nil {
			return err
		}
	}
	return nil
}

func repositoryOwnership(ctx context.Context, query nativeQueryer, id int64) (string, bool, error) {
	var profile string
	var enabled bool
	err := query.QueryRowContext(ctx, "SELECT profile, github_repository_enabled FROM projects WHERE repository_id = ?", id).Scan(&profile, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return profile, enabled, err
}

func (s *Service) registerIntegrationRoutes(e *echo.Echo) {
	if s.config.Hosted == nil {
		operator := s.requireAPIScope(apiScopeOperator)
		e.GET(hostedOrganizationBase+"/model-selection", s.getOrganizationModelSelection, operator)
		e.PUT(hostedOrganizationBase+"/model-selection", s.updateOrganizationModelSelection, operator)
	}
	e.GET("/api/v2/github/requests", s.githubRequestCounts, s.requireInstanceAdmin())
	e.GET(nativeBase+"/model-selection", s.getCloudModelSelection, s.requireNativeScope(apiScopeWorker, apiScopeOperator))
	e.PUT(nativeBase+"/model-selection", s.updateCloudModelSelection, s.requireNativeScope(apiScopeOperator, apiScopeAdmin))
	e.GET(nativeBase+"/content-settings", s.getCloudContentSettings, s.requireNativeScope(apiScopeWorker, apiScopeOperator))
	e.PUT(nativeBase+"/content-settings", s.updateCloudContentSettings, s.requireNativeScope(apiScopeOperator, apiScopeAdmin))
	read := s.requireNativeScope(apiScopeWorker, apiScopeOperator)
	operator := s.requireNativeScope(apiScopeOperator)
	admin := s.requireNativeScope(apiScopeAdmin)
	e.GET(nativeBase+"/integration", s.getProjectIntegrationPage, s.requireAPIScope(apiScopeWorker, apiScopeOperator))
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
