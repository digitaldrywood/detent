package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ProjectIntegration struct {
	ArchiveCompletedAfterDays *int                  `json:"archive_completed_after_days"`
	ArchiveCancelledAfterDays *int                  `json:"archive_cancelled_after_days"`
	WorkflowSource            string                `json:"workflow_source,omitempty"`
	WorkflowSourceRevision    string                `json:"workflow_source_revision,omitempty"`
	WorkflowMarkdown          string                `json:"workflow_markdown,omitempty"`
	States                    []tracker.NativeState `json:"states,omitempty"`
	Profile                   string                `json:"profile"`
	Revision                  tracker.Revision      `json:"revision,string"`
	Intake                    string                `json:"intake"`
	Projection                string                `json:"projection"`
	RepositoryEnabled         bool                  `json:"repository_enabled"`
	GitHubTransportAvailable  *bool                 `json:"github_transport_available,omitempty"`
	GitHubAppSlug             string                `json:"github_app_slug,omitempty"`
	GitHubAppInstallURL       string                `json:"github_app_install_url,omitempty"`
	GitHubAppInstalled        *bool                 `json:"github_app_installed,omitempty"`
	Repository                string                `json:"repository,omitempty"`
	CheckoutRepository        string                `json:"checkout_repository,omitempty"`
	Authority                 map[string]string     `json:"authority"`
	RepositoryID              int64                 `json:"-"`
}

type GitHubAppInstallation struct {
	Slug       string
	InstallURL string
	Installed  bool
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
	var states string
	err := query.QueryRowContext(ctx, `SELECT p.profile, p.integration_revision, p.github_intake, p.github_projection,
p.github_repository_enabled, COALESCE(r.github_owner || '/' || r.github_name, ''), COALESCE(r.id, 0), p.checkout_repository, p.states_json, p.workflow_source, p.workflow_source_revision, p.workflow_markdown, p.archive_completed_after_days, p.archive_cancelled_after_days
FROM projects p LEFT JOIN repositories r ON r.id = p.repository_id WHERE p.organization_id = ? AND p.id = ?`, scope.organization, scope.project).Scan(
		&result.Profile, &result.Revision, &result.Intake, &result.Projection, &result.RepositoryEnabled, &result.Repository, &result.RepositoryID, &result.CheckoutRepository, &states, &result.WorkflowSource, &result.WorkflowSourceRevision, &result.WorkflowMarkdown, &result.ArchiveCompletedAfterDays, &result.ArchiveCancelledAfterDays)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal([]byte(states), &result.States); err != nil {
		return result, err
	}
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
	result, err := s.projectIntegration(c.Request().Context(), s.database.db, nativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) getCutoverReceipt(c echo.Context) error {
	result, err := s.readCutoverReceipt(c.Request().Context(), nativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) projectIntegration(ctx context.Context, query nativeQueryer, scope nativeScope) (ProjectIntegration, error) {
	result, err := readProjectIntegration(ctx, query, scope)
	if err != nil {
		return result, err
	}
	available := !s.config.GitHubDisabled && s.config.ReconcileBackend != nil
	result.GitHubTransportAvailable = &available
	repository := result.CheckoutRepository
	if repository == "" {
		repository = result.Repository
	}
	if available && result.Profile == "native" && repository != "" && s.config.GitHubAppInstallation != nil {
		app, err := s.config.GitHubAppInstallation(ctx, repository)
		if err != nil {
			return result, err
		}
		result.GitHubAppSlug, result.GitHubAppInstallURL = app.Slug, app.InstallURL
		result.GitHubAppInstalled = &app.Installed
	}
	return result, err
}

type updateProjectIntegrationRequest struct {
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
		return invalidAPIRequest(c, err)
	}
	scope := nativeRequestScope(c)
	scope.requireHostedAdmin = true
	c.Set("native_scope", scope)
	return s.nativeMutation(c, request.Mutation, request, s.updateProjectIntegrationOperation(request))
}

func (s *Service) updateProjectIntegrationOperation(request updateProjectIntegrationRequest) func(context.Context, *sql.Tx, nativeScope, time.Time) (any, error) {
	return func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		current, err := readProjectIntegration(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		if current.Revision != request.ExpectedRevision {
			return nil, nativeConflict(current.Revision)
		}
		if current.Profile == "native" && (current.Repository != "" || current.CheckoutRepository != "") && request.Intake == "automatic" {
			request.Intake = "manual"
		}
		completed, err := archivePeriod(request.ArchiveCompletedAfterDays, current.ArchiveCompletedAfterDays)
		if err != nil {
			return nil, err
		}
		cancelled, err := archivePeriod(request.ArchiveCancelledAfterDays, current.ArchiveCancelledAfterDays)
		if err != nil {
			return nil, err
		}
		currentIntake := current.Intake
		if currentIntake == "automatic" {
			currentIntake = "manual"
		}
		archiveSettingsSupplied := len(request.ArchiveCompletedAfterDays) != 0 || len(request.ArchiveCancelledAfterDays) != 0
		integrationChanged := request.States != nil || request.WorkflowMarkdown != nil || request.Intake != currentIntake || request.Projection != current.Projection || request.RepositoryEnabled != current.RepositoryEnabled
		if archiveSettingsSupplied && !integrationChanged {
			if _, err := tx.ExecContext(ctx, "UPDATE projects SET integration_revision=integration_revision+1, archive_completed_after_days=?, archive_cancelled_after_days=? WHERE organization_id=? AND id=?", completed, cancelled, scope.organization, scope.project); err != nil {
				return nil, err
			}
			return readProjectIntegration(ctx, tx, scope)
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
		if request.States != nil && request.WorkflowMarkdown != nil {
			return nil, nativeWorkflowInvalid("Supply workflow_markdown or states, not both")
		}
		if request.WorkflowMarkdown != nil {
			if current.WorkflowSource != "" {
				return nil, nativeWorkflowInvalid("Workflow is controlled by the repository; edit its definition and approve the new repository policy")
			}
			if len(*request.WorkflowMarkdown) > 128*1024 {
				return nil, nativeWorkflowInvalid("Workflow Markdown is limited to 128 KiB")
			}
			workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{WorkflowPath: "Cloud WORKFLOW.md", Workflow: []byte(*request.WorkflowMarkdown)})
			if err != nil {
				return nil, nativeWorkflowInvalid(err.Error())
			}
			if workflow.Config.Tracker.Kind != workflowconfig.TrackerHubNative {
				return nil, nativeWorkflowInvalid("Cloud workflow must use tracker.kind hub_native")
			}
			if err := workflow.Config.Validate(); err != nil {
				return nil, nativeWorkflowInvalid(err.Error())
			}
			if err := workflow.Config.ValidateNativeWorkflow(); err != nil {
				return nil, nativeWorkflowInvalid(err.Error())
			}
			if err := updateNativeProjectStates(ctx, tx, scope, workflow.Config.NativeWorkflowStates(), now); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE projects SET workflow_markdown=? WHERE organization_id=? AND id=?", *request.WorkflowMarkdown, scope.organization, scope.project); err != nil {
				return nil, err
			}
		}
		if request.States != nil {
			if current.WorkflowMarkdown != "" {
				return nil, nativeWorkflowInvalid("Workflow is authored in Markdown; update workflow_markdown instead of competing state arrays")
			}
			if err := updateNativeProjectStates(ctx, tx, scope, *request.States, now); err != nil {
				return nil, err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE projects SET integration_revision = integration_revision + 1, github_intake = ?, github_projection = ?, github_repository_enabled = ?, archive_completed_after_days = ?, archive_cancelled_after_days = ? WHERE id = ?`, request.Intake, request.Projection, request.RepositoryEnabled, completed, cancelled, scope.project)
		if err != nil {
			return nil, err
		}
		if err := supersedeDisallowedOutbox(ctx, tx, scope, now); err != nil {
			return nil, err
		}
		return readProjectIntegration(ctx, tx, scope)
	}
}

func archivePeriod(raw json.RawMessage, current *int) (*int, error) {
	if len(raw) == 0 {
		return current, nil
	}
	var days *int
	if err := json.Unmarshal(raw, &days); err != nil {
		return nil, nativeInvalid("Archive period must be 7, 14, 30, 60, 90 days or null")
	}
	if days != nil {
		switch *days {
		case 7, 14, 30, 60, 90:
		default:
			return nil, nativeInvalid("Archive period must be 7, 14, 30, 60, 90 days or null")
		}
	}
	return days, nil
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
((p.profile = 'native' AND (mutation_kind = 'workflow_label' OR (mutation_kind = 'workpad' AND ((p.github_projection = 'disabled' AND COALESCE(json_extract(desired_json, '$.issue_number'), 0) = 0) OR COALESCE(json_extract(desired_json, '$.summary'), 0) = 0)))) OR (mutation_kind = 'merge_pull_request' AND p.github_repository_enabled = 0)))
AND status IN ('pending', 'retrying')`, formatOutboxTime(now), formatOutboxTime(now), scope.project)
	return err
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
	e.GET("/api/v2/github/requests", s.githubRequestCounts, s.requireInstanceAdmin())
	e.GET(nativeBase+"/model-selection", s.getCloudModelSelection, s.requireNativeScope(apiScopeWorker, apiScopeOperator))
	e.PUT(nativeBase+"/model-selection", s.updateCloudModelSelection, s.requireNativeScope(apiScopeOperator, apiScopeAdmin))
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
