package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e hubProjectExecutor) read(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if operatortool.IsModelSelection(call.Name) {
		return e.readModelSelection(ctx, call)
	}
	var r operatortool.ProjectReadRequest
	if operatortool.DecodeProjectArguments(call.Arguments, &r) != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	if r.ProjectID == "" && call.Name != "list_projects" && call.Name != "project_setup" && call.Name != operatortool.GetSpritePool {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	ctx, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: r.ProjectID, OrganizationWide: call.Name == operatortool.GetSpritePool && r.ProjectID == ""})
	if err != nil {
		return operatortool.Result{}, err
	}
	scope, err := e.projectScope(ctx)
	if err != nil {
		return operatortool.Result{}, err
	}
	scope.Project = tracker.ProjectID(r.ProjectID)
	s := e.service
	var value any
	switch call.Name {
	case "list_projects":
		value, err = s.operatorProjectList(ctx, scope, r)
	case "get_native_project":
		var record projectstore.Record
		record, err = readHostedProject(ctx, s, scope, tenantstore.Reader.ProjectRecord)
		if err == nil {
			value, err = s.projectRecordNative(ctx, record)
		}
	case operatortool.GetSpritePool:
		value, err = s.spritePoolResponse(ctx, scope)
	case operatortool.GetProjectSpriteAllocation:
		value, err = s.readProjectSpriteAllocation(ctx, scope)
	case "get_onboarding":
		value, err = s.projectOnboarding(ctx, scope)
	case "get_git_hub_batch":
		var view githubBatchView
		view, err = s.projectGitHubBatch(ctx, scope)
		if err == nil {
			value, err = boundedProjectBatch(view, r)
		}
	case "get_project_integration":
		value, err = s.projectIntegration(ctx, scope)
	case "get_cutover_receipt":
		value, err = s.readCutoverReceipt(ctx, scope)
		if errors.Is(err, corestore.ErrNotFound) {
			err = &operatortool.RequestError{Code: "not_found", Message: "No cutover receipt exists for this project"}
		}
	case "get_git_hub_import":
		if r.ImportID == "" {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		var job GitHubImport
		job, err = s.readGitHubImportRecord(ctx, scope, r.ImportID)
		if job.LastError != "" {
			job.LastError = "Import transport is unavailable"
		}
		value = job
	case "list_git_hub_import_records":
		value, err = s.readProjectImportRecords(ctx, scope, r)
	case "get_project_policy":
		var record projectstore.Metadata
		record, err = readHostedProject(ctx, s, scope, tenantstore.Reader.ProjectMetadata)
		if err == nil && r.RepositoryPolicy && (record.Core.Profile != "native" || record.Settings.CheckoutRepository == "") {
			err = &operatortool.RequestError{Code: "invalid_request", Message: "Project has no linked repository policy; use repository_policy:false for its project policy"}
		}
		if err == nil {
			var approval policy.Approval
			approval, err = s.readTenantProjectPolicy(ctx, scope.Organization, scope.Project)
			if err == nil {
				value, err = s.tenantProjectPolicyRead(ctx, scope.Organization, scope.Project, approval, r.Limit, r.After)
			}
		}
	case "project_secret_metadata":
		var status projectSecretStatus
		status, err = s.readAccessSecretStatus(ctx, scope)
		value = struct {
			Metadata projectSecretStatus `json:"metadata"`
			SetupURL string              `json:"setup_url"`
		}{status, s.hostedPath("/projects/" + r.ProjectID + "/settings")}
	case "repository_freshness":
		value, err = s.projectRepositoryFreshness(ctx, scope)
	case "project_setup":
		if s.config.Hosted == nil {
			return operatortool.Result{}, hubapi.ErrProjectServiceUnavailable
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
		return operatortool.Result{}, hubapi.ErrProjectServiceUnavailable
	}
	if err != nil {
		return operatortool.Result{}, projectToolError(err)
	}
	// Each read carries its application identity and observation time. The value
	// is a concrete application DTO; no HTTP body or database record is proxied.
	page := struct {
		OrganizationID tracker.OrganizationID `json:"organization_id"`
		ProjectID      tracker.ProjectID      `json:"project_id,omitempty"`
		ObservedAt     time.Time              `json:"observed_at"`
		Data           any                    `json:"data"`
	}{scope.Organization, scope.Project, s.config.Clock().UTC(), value}
	if call.Name == "get_project_policy" {
		view, ok := value.(projectPolicyRead)
		if !ok {
			return operatortool.Result{}, hubapi.ErrProjectServiceUnavailable
		}
		page.Data = json.RawMessage(`{}`)
		envelope, err := json.Marshal(page)
		if err != nil {
			return operatortool.Result{}, err
		}
		page.Data, err = boundedProjectPolicyHistory(view, operatortool.MaxResultBytes-len(envelope)+2)
		if err != nil {
			return operatortool.Result{}, err
		}
	}
	return hubProjectResult(page)
}

func boundedProjectPolicyHistory(approval projectPolicyRead, budget int) (projectPolicyRead, error) {
	history, next := approval.History, approval.HistoryNext
	approval.History, approval.HistoryNext = nil, ""
	if len(history) > 0 {
		entry := history[0]
		entry.PreviousDefinition, entry.Definition = nil, nil
		approval.History = []policy.WorkflowApply{entry}
		approval.HistoryNext = next
		if len(history) > 1 {
			approval.HistoryNext = strconv.FormatInt(entry.ID, 10)
		}
	}
	raw, err := json.Marshal(approval)
	if err != nil {
		return approval, err
	}
	if len(raw) > budget && len(approval.ObservedPolicies) > 0 {
		approval.ObservedPolicyDetailsOmitted = true
		observations := make([]policy.ObservedPolicy, len(approval.ObservedPolicies))
		for i, observed := range approval.ObservedPolicies {
			observations[i] = observed
			observations[i].Policy = policy.Descriptor{
				Schema: observed.Policy.Schema, ID: observed.Policy.ID,
				SourceRevision: observed.Policy.SourceRevision, SourceDigest: observed.Policy.SourceDigest, ConfigDigest: observed.Policy.ConfigDigest,
			}
		}
		low, high := 0, len(observations)
		for low < high {
			count := low + (high-low+1)/2
			approval.ObservedPolicies = observations[:count]
			approval.ObservedPoliciesOmitted = len(observations) - count
			raw, err = json.Marshal(approval)
			if err != nil {
				return approval, err
			}
			if len(raw) <= budget {
				low = count
			} else {
				high = count - 1
			}
		}
		approval.ObservedPolicies = observations[:low]
		approval.ObservedPoliciesOmitted = len(observations) - low
		raw, err = json.Marshal(approval)
		if err != nil {
			return approval, err
		}
	}
	if len(raw) > budget {
		return approval, hubapi.ErrProjectServiceUnavailable
	}
	approval.History, approval.HistoryNext = nil, ""
	for i, entry := range history {
		entry.PreviousDefinition, entry.Definition = nil, nil
		previousNext := approval.HistoryNext
		approval.History = append(approval.History, entry)
		approval.HistoryNext = next
		if i+1 < len(history) {
			approval.HistoryNext = strconv.FormatInt(entry.ID, 10)
		}
		raw, err := json.Marshal(approval)
		if err != nil {
			return approval, err
		}
		if len(raw) > budget {
			if i == 0 {
				return approval, hubapi.ErrProjectServiceUnavailable
			}
			approval.History = approval.History[:i]
			approval.HistoryNext = previousNext
			break
		}
	}
	return approval, nil
}

type operatorProjectPage struct {
	Projects   []tracker.NativeProject `json:"projects"`
	NextCursor string                  `json:"next_cursor,omitempty"`
}

type projectBatchPage struct {
	Batch      *projectBatch         `json:"batch"`
	Lanes      []tracker.NativeState `json:"lanes"`
	NextCursor string                `json:"next_cursor,omitempty"`
}

type projectBatch struct {
	tracker.GitHubBatch
	Page projectBatchIssues `json:"page"`
}

type projectBatchIssues struct {
	Issues     []projectBatchIssue `json:"issues"`
	Total      int                 `json:"total"`
	NextCursor string              `json:"next_cursor"`
}

type projectBatchIssue struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	Labels []string `json:"labels"`
	Status string   `json:"status"`
}

// Stored runner diagnostics are provider text. Keep the application's checkpoint
// and status while returning a bounded page and opaque transport diagnostics.
func boundedProjectBatch(view githubBatchView, request operatortool.ProjectReadRequest) (projectBatchPage, error) {
	page := projectBatchPage{Lanes: view.Lanes}
	after := 0
	if request.After != "" {
		var err error
		after, err = strconv.Atoi(request.After)
		if err != nil || after < 0 || after > 50000 {
			return page, operatortool.ErrInvalidArguments
		}
	}
	if view.Batch == nil {
		return page, nil
	}
	batch := *view.Batch
	batch.Items = slices.Clone(batch.Items)
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
	issues := make([]projectBatchIssue, 0, len(batch.Page.Issues))
	for _, issue := range batch.Page.Issues {
		status := "open"
		if issue.Closed {
			status = "closed"
		}
		issues = append(issues, projectBatchIssue{Number: issue.Number, Title: issue.Title, Labels: issue.Labels, Status: status})
	}
	page.Batch = &projectBatch{GitHubBatch: batch, Page: projectBatchIssues{Issues: issues, Total: batch.Page.Total, NextCursor: batch.Page.NextCursor}}
	return page, nil
}

func (s *Service) operatorProjectList(ctx context.Context, scope hubscope.NativeScope, r operatortool.ProjectReadRequest) (operatorProjectPage, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if s.config.TenantRepository == nil {
		return operatorProjectPage{}, operatortool.ErrServiceUnavailable
	}
	return s.operatorProjectListHosted(ctx, scope, r)
}

func projectAccessFilter(credential hubapi.APICredential, project tracker.ProjectID, after tracker.ProjectID, limit int) accessstore.ProjectAccessFilter {
	return hubauth.ProjectFilter(hubscope.AuthCredential(credential), project, after, limit)
}

func (s *Service) grantedAccessProjects(ctx context.Context, r accessstore.Reader, organization tracker.OrganizationID, credential hubapi.APICredential, projects []accessstore.ProjectAccess) ([]accessstore.ProjectAccess, error) {
	if credential.EntryKey != nil || credential.HostedKeyKind != "service" {
		return projects, nil
	}
	granted := make([]accessstore.ProjectAccess, 0, len(projects))
	for _, project := range projects {
		err := s.authorizeAccessScope(ctx, r, hubscope.NativeScope{Organization: organization, Project: project.ProjectID, Credential: credential}, false)
		if errors.Is(err, accessstore.ErrDenied) || hubapi.IsNativeNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		granted = append(granted, project)
	}
	return granted, nil
}

func hostedSubject(credential hubapi.APICredential) string {
	if credential.Hosted == nil || credential.EntryKey != nil && credential.EntryKey.Kind == "service" {
		return ""
	}
	return credential.Hosted.Subject
}

func (s *Service) operatorProjectListHosted(ctx context.Context, scope hubscope.NativeScope, request operatortool.ProjectReadRequest) (operatorProjectPage, error) {
	limit := request.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 1000 {
		return operatorProjectPage{}, corestore.ErrInvalidPage
	}
	filter := projectAccessFilter(scope.Credential, "", tracker.ProjectID(request.After), limit)
	if request.ProjectID != "" {
		filter.Only = tracker.ProjectID(request.ProjectID)
	}
	var rows []accessstore.ProjectAccess
	var records []projectstore.Record
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		pageReader, ok := reader.(accessstore.ProjectAccessPageReader)
		if !ok {
			return hubapi.ErrProjectParticipants
		}
		var err error
		rows, err = pageReader.ProjectAccessPage(ctx, filter)
		if err != nil {
			return err
		}
		count := len(rows)
		if count > limit {
			count = limit
		}
		records = make([]projectstore.Record, 0, count)
		for _, row := range rows[:count] {
			record, err := reader.ProjectRecord(ctx, row.ProjectID)
			if err != nil {
				return err
			}
			records = append(records, record)
		}
		return nil
	})
	if err != nil {
		return operatorProjectPage{}, err
	}
	page := operatorProjectPage{Projects: make([]tracker.NativeProject, 0, len(records))}
	for _, record := range records {
		project, err := s.projectRecordNative(ctx, record)
		if err != nil {
			return operatorProjectPage{}, err
		}
		page.Projects = append(page.Projects, project)
	}
	if len(rows) > limit && len(page.Projects) > 0 {
		page.NextCursor = string(page.Projects[len(page.Projects)-1].ID)
	}
	return page, nil
}

func readHostedProject[T any](ctx context.Context, s *Service, scope hubscope.NativeScope, read func(tenantstore.Reader, context.Context, tracker.ProjectID) (T, error)) (T, error) {
	var record T
	if s.config.TenantRepository == nil {
		return record, operatortool.ErrServiceUnavailable
	}
	filter := projectAccessFilter(scope.Credential, scope.Project, "", 1)
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		pageReader, ok := reader.(accessstore.ProjectAccessPageReader)
		if !ok {
			return hubapi.ErrProjectParticipants
		}
		rows, err := pageReader.ProjectAccessPage(ctx, filter)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return corestore.ErrNotFound
		}
		record, err = read(reader, ctx, scope.Project)
		return err
	})
	return record, hubapi.ProjectRecordError(err)
}

func (s *Service) readCutoverReceipt(ctx context.Context, scope hubscope.NativeScope) (CutoverReceipt, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if s.config.TenantRepository == nil {
		return CutoverReceipt{}, operatortool.ErrServiceUnavailable
	}
	var row postgres.CutoverReceipt
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		if _, err := r.ProjectVersion(ctx, scope.Project); err != nil {
			return hubapi.ProjectRecordError(err)
		}
		reader, ok := r.(postgres.CutoverReader)
		if !ok {
			return operatortool.ErrServiceUnavailable
		}
		var err error
		row, err = reader.LatestProjectCutoverReceipt(ctx, postgres.LatestProjectCutoverReceiptInput{OrganizationID: scope.Organization, ProjectID: scope.Project})
		return err
	})
	if err != nil {
		return CutoverReceipt{}, err
	}
	raw, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, PayloadObjectKey: row.ReceiptObjectKey, PayloadSHA256: row.ReceiptSha256, PayloadBytes: row.ReceiptBytes, PayloadContentBytes: row.ReceiptContentBytes})
	if err != nil {
		return CutoverReceipt{}, err
	}
	var result CutoverReceipt
	if err := json.Unmarshal(raw, &result); err != nil {
		return CutoverReceipt{}, err
	}
	if row.OrganizationID != scope.Organization || row.ProjectID != scope.Project || result.Checkpoint != row.Checkpoint {
		return CutoverReceipt{}, artifact.ErrIntegrity
	}
	return result, nil
}

type projectImportRecordPage struct {
	Records    []GitHubImportRecord `json:"records"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

func (s *Service) readProjectImportRecords(ctx context.Context, scope hubscope.NativeScope, r operatortool.ProjectReadRequest) (projectImportRecordPage, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	result := projectImportRecordPage{Records: []GitHubImportRecord{}}
	if r.ImportID == "" {
		return result, operatortool.ErrInvalidArguments
	}
	if _, err := s.readGitHubImportRecord(ctx, scope, r.ImportID); err != nil {
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
	rows, err := s.readGitHubImportRecords(ctx, scope, r.ImportID, after, limit)
	if err != nil {
		return result, err
	}
	var last int64
	for index, row := range rows {
		if index == limit {
			result.NextCursor = strconv.FormatInt(last, 10)
			break
		}
		record, err := s.decodeGitHubImportRecord(ctx, row)
		if err != nil {
			return result, err
		}
		result.Records = append(result.Records, record)
		last = row.Sequence
	}
	return result, nil
}
func (s *Service) projectRepositoryFreshness(ctx context.Context, scope hubscope.NativeScope) (repositoryFreshnessResponse, error) {
	record, err := s.readProjectMetadata(ctx, scope)
	if err != nil {
		return repositoryFreshnessResponse{}, err
	}
	if record.Settings.Repository == "" && record.Settings.CheckoutRepository == "" {
		return repositoryFreshnessResponse{Repositories: []RepositoryFreshness{}}, nil
	}
	return repositoryFreshnessResponse{}, operatortool.ErrServiceUnavailable
}
