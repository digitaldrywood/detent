package hubserver

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e hubProjectExecutor) read(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	if operatortool.IsModelSelection(call.Name) {
		return e.readModelSelection(ctx, call)
	}
	var r operatortool.ProjectReadRequest
	if operatortool.DecodeProjectArguments(call.Arguments, &r) != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	if r.ProjectID == "" && call.Name != "list_projects" && call.Name != "project_setup" {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	ctx, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: r.ProjectID})
	if err != nil {
		return operatortool.Result{}, err
	}
	scope, err := e.projectScope(ctx)
	if err != nil {
		return operatortool.Result{}, err
	}
	scope.project = tracker.ProjectID(r.ProjectID)
	s := e.service
	var value any
	switch call.Name {
	case "list_projects":
		value, err = s.operatorProjectList(ctx, scope, r)
	case "get_native_project":
		value, err = readNativeProject(ctx, s.database.db, scope)
	case "get_onboarding":
		value, err = s.projectOnboarding(ctx, scope)
	case "get_git_hub_batch":
		var view githubBatchView
		view, err = s.projectGitHubBatch(ctx, scope)
		if err == nil {
			value, err = boundedProjectBatch(view, r)
		}
	case "get_project_integration":
		value, err = s.projectIntegration(ctx, s.database.db, scope)
	case "get_cutover_receipt":
		value, err = s.readCutoverReceipt(ctx, scope)
	case "get_git_hub_import":
		if r.ImportID == "" {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		var job GitHubImport
		job, err = readGitHubImport(ctx, s.database.db, scope, r.ImportID)
		if job.LastError != "" {
			job.LastError = "Import transport is unavailable"
		}
		value = job
	case "list_git_hub_import_records":
		value, err = s.readProjectImportRecords(ctx, scope, r)
	case "get_project_policy":
		var policyScope string
		policyScope, err = operatorPolicyScope(ctx, s.database.db, scope, r.RepositoryPolicy)
		if err == nil {
			value, err = readProjectPolicyWithHistory(ctx, s.database.db, policyScope, r.Limit, r.After)
		}
	case "project_secret_metadata":
		var status projectSecretStatus
		status, err = readSecretStatus(ctx, s.database.db, scope)
		value = struct {
			Metadata projectSecretStatus `json:"metadata"`
			SetupURL string              `json:"setup_url"`
		}{status, s.hostedPath("/projects/" + r.ProjectID + "/settings")}
	case "repository_freshness":
		value, err = s.projectRepositoryFreshness(ctx, scope)
	case "project_setup":
		if s.config.Hosted == nil {
			return operatortool.Result{}, errProjectServiceUnavailable
		}
		url := s.hostedPath("/settings/projects")
		steps := []string{"Create or select a project", "Configure repository, provider credentials and runner in the authenticated browser", "Inspect onboarding and approve its exact policy"}
		if r.ProjectID != "" {
			url = s.hostedPath("/projects/" + r.ProjectID + "/setup")
		}
		value = struct {
			URL         string   `json:"setup_url"`
			Steps       []string `json:"required_steps"`
			Credentials string   `json:"credentials"`
		}{url, steps, "Configure credentials in the existing browser setup form; tools never accept or return secret values"}
	default:
		return operatortool.Result{}, errProjectServiceUnavailable
	}
	if err != nil {
		return operatortool.Result{}, projectToolError(err)
	}
	// Each read carries its application identity and observation time. The value
	// is a concrete application DTO; no HTTP body or database record is proxied.
	return hubProjectResult(struct {
		OrganizationID tracker.OrganizationID `json:"organization_id"`
		ProjectID      tracker.ProjectID      `json:"project_id,omitempty"`
		ObservedAt     time.Time              `json:"observed_at"`
		Data           any                    `json:"data"`
	}{scope.organization, scope.project, s.config.now().UTC(), value})
}

type operatorProjectPage struct {
	Projects   []tracker.NativeProject `json:"projects"`
	NextCursor string                  `json:"next_cursor,omitempty"`
}

type projectBatchPage struct {
	githubBatchView
	NextCursor string `json:"next_cursor,omitempty"`
}

// Stored runner diagnostics are provider text. Keep the application's checkpoint
// and status while returning a bounded page and opaque transport diagnostics.
func boundedProjectBatch(view githubBatchView, request operatortool.ProjectReadRequest) (projectBatchPage, error) {
	page := projectBatchPage{githubBatchView: view}
	after := 0
	if request.After != "" {
		var err error
		after, err = strconv.Atoi(request.After)
		if err != nil || after < 0 || after > 1000 {
			return page, operatortool.ErrInvalidArguments
		}
	}
	if view.Batch == nil {
		return page, nil
	}
	batch := view.Batch
	if batch.Error != "" {
		batch.Error = "Import transport is unavailable"
	}
	for i := range batch.Items {
		if batch.Items[i].Error != "" {
			batch.Items[i].Error = "Import transport is unavailable"
		}
	}
	limit := request.Limit
	if limit == 0 {
		limit = 100
	}
	if max(len(batch.Page.Issues), len(batch.Items)) > after+limit {
		page.NextCursor = strconv.Itoa(after + limit)
	}
	batch.Page.Issues = batch.Page.Issues[min(after, len(batch.Page.Issues)):min(after+limit, len(batch.Page.Issues))]
	batch.Items = batch.Items[min(after, len(batch.Items)):min(after+limit, len(batch.Items))]
	return page, nil
}

func (s *Service) operatorProjectList(ctx context.Context, scope nativeScope, r operatortool.ProjectReadRequest) (operatorProjectPage, error) {
	ids := []string{}
	if s.config.Hosted != nil {
		projects, err := s.hostedReadableProjects(ctx, scope.credential)
		if err != nil {
			return operatorProjectPage{}, err
		}
		for _, project := range projects {
			ids = append(ids, project.ID)
		}
		slices.Sort(ids)
	} else {
		rows, err := s.database.db.QueryContext(ctx, "SELECT id FROM projects WHERE organization_id=? ORDER BY id", scope.organization)
		if err != nil {
			return operatorProjectPage{}, err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return operatorProjectPage{}, err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		if closeErr := rows.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return operatorProjectPage{}, err
		}
	}
	limit := r.Limit
	if limit == 0 {
		limit = 100
	}
	page := operatorProjectPage{Projects: []tracker.NativeProject{}}
	for _, id := range ids {
		if r.ProjectID != "" && id != r.ProjectID || id <= r.After {
			continue
		}
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: id}); err != nil {
			continue
		}
		if len(page.Projects) == limit {
			page.NextCursor = string(page.Projects[len(page.Projects)-1].ID)
			break
		}
		scope.project = tracker.ProjectID(id)
		p, err := readNativeProject(ctx, s.database.db, scope)
		if err != nil {
			return page, err
		}
		page.Projects = append(page.Projects, p)
	}
	return page, nil
}
func (s *Service) readCutoverReceipt(ctx context.Context, scope nativeScope) (CutoverReceipt, error) {
	var raw string
	var result CutoverReceipt
	err := s.database.db.QueryRowContext(ctx, "SELECT c.receipt_json FROM github_cutovers c JOIN projects p ON p.id=c.project_id WHERE p.organization_id=? AND p.id=?", scope.organization, scope.project).Scan(&raw)
	if err != nil {
		return result, err
	}
	err = json.Unmarshal([]byte(raw), &result)
	return result, err
}

type projectImportRecordPage struct {
	Records    []GitHubImportRecord `json:"records"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

func (s *Service) readProjectImportRecords(ctx context.Context, scope nativeScope, r operatortool.ProjectReadRequest) (projectImportRecordPage, error) {
	result := projectImportRecordPage{Records: []GitHubImportRecord{}}
	if r.ImportID == "" {
		return result, operatortool.ErrInvalidArguments
	}
	if _, err := readGitHubImport(ctx, s.database.db, scope, r.ImportID); err != nil {
		return result, err
	}
	after := int64(0)
	var err error
	if r.After != "" {
		after, err = strconv.ParseInt(r.After, 10, 64)
	}
	if err != nil || after < 0 {
		return result, operatortool.ErrInvalidArguments
	}
	limit := r.Limit
	if limit == 0 {
		limit = 100
	}
	rows, err := s.database.db.QueryContext(ctx, "SELECT sequence,record_json,current_dependency FROM github_import_records WHERE import_id=? AND sequence>? ORDER BY sequence LIMIT ?", r.ImportID, after, limit+1)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	var last int64
	for rows.Next() {
		var sequence int64
		var raw string
		var current bool
		if err := rows.Scan(&sequence, &raw, &current); err != nil {
			return result, err
		}
		if len(result.Records) == limit {
			result.NextCursor = strconv.FormatInt(last, 10)
			break
		}
		var record GitHubImportRecord
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			return result, err
		}
		if record.Kind == "dependency" {
			record.CurrentDependency = &current
		}
		result.Records = append(result.Records, record)
		last = sequence
	}
	return result, rows.Err()
}
func (s *Service) projectRepositoryFreshness(ctx context.Context, scope nativeScope) (repositoryFreshnessResponse, error) {
	integration, err := readProjectIntegration(ctx, s.database.db, scope)
	if err != nil {
		return repositoryFreshnessResponse{}, err
	}
	result, err := s.database.repositoryFreshness(ctx, s.config.now(), s.config.ReconcileInterval)
	if err != nil {
		return result, err
	}
	result.Repositories = slices.DeleteFunc(result.Repositories, func(r RepositoryFreshness) bool { return r.ID != integration.RepositoryID })
	result.Summary = RepositoryHealthSummary{Total: len(result.Repositories)}
	for i := range result.Repositories {
		r := &result.Repositories[i]
		switch r.Status {
		case "fresh":
			result.Summary.Fresh++
		case "stale":
			result.Summary.Stale++
		default:
			result.Summary.Error++
		}
		if r.LastWebhookError != nil {
			r.LastWebhookError.Message = "Repository sync is unavailable"
		}
		if r.LastReconcileError != nil {
			r.LastReconcileError.Message = "Repository sync is unavailable"
		}
	}
	return result, nil
}

// Resolve legacy policy ownership through the authorized project binding, never
// through a caller-selected owner/repository pair in the compatibility API.
func operatorPolicyScope(ctx context.Context, query nativeQueryer, scope nativeScope, repository bool) (string, error) {
	if !repository {
		return string(scope.organization) + "/" + string(scope.project), nil
	}
	integration, err := readProjectIntegration(ctx, query, scope)
	if err != nil {
		return "", err
	}
	if integration.RepositoryID == 0 || integration.Repository == "" {
		return "", errProjectServiceUnavailable
	}
	return "repository:" + strings.ToLower(integration.Repository), nil
}
