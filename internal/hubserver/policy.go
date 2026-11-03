package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/changerequest"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var errNoApprovedRepositoryPolicy = &nativeError{Code: "policy_not_approved", Message: "No approved repository policy; resolve the customer definition and ask an administrator to approve its descriptor", status: http.StatusConflict}

const projectPolicyObservationLimit = 512

func policyMismatch(message string) error {
	return &nativeError{Code: "policy_mismatch", Message: message, status: http.StatusConflict}
}

func (s *Service) policyScope(c echo.Context) (string, error) {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	if c.Param("organization") != "" {
		scope := nativeScope{organization: tracker.OrganizationID(c.Param("organization")), project: tracker.ProjectID(c.Param("project"))}
		if s.config.TenantRepository != nil {
			err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
				_, err := reader.ProjectRecord(ctx, scope.project)
				return err
			})
			if errors.Is(err, corestore.ErrNotFound) {
				return "", nativeNotFound()
			}
			if err != nil {
				return "", err
			}
			return string(scope.organization) + "/" + string(scope.project), nil
		}
		var count int
		if err := s.database.reader.QueryRowContext(ctx, "SELECT count(*) FROM projects WHERE organization_id = ? AND id = ?", scope.organization, scope.project).Scan(&count); err != nil {
			return "", err
		}
		if count != 1 {
			return "", nativeNotFound()
		}
		return string(scope.organization) + "/" + string(scope.project), nil
	}
	repository := c.Param("owner") + "/" + c.Param("repo")
	var count int
	if err := s.database.reader.QueryRowContext(ctx, "SELECT count(*) FROM repositories WHERE github_owner || '/' || github_name = ? COLLATE NOCASE", repository).Scan(&count); err != nil {
		return "", err
	}
	if count != 1 {
		return "", nativeNotFound()
	}
	return "repository:" + strings.ToLower(repository), nil
}

func (s *Service) getProjectPolicy(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	version := c.QueryParam("execution_configuration")
	if version != "" && version != "1" {
		return s.nativeAPIError(c, nativeInvalid("unsupported execution configuration version"))
	}
	scope, err := s.policyScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	limit, err := parsePageLimit(c.QueryParam("limit"))
	if err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	var approval policy.Approval
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if s.config.TenantRepository != nil && c.Param("organization") != "" {
		approval, err = s.readTenantProjectPolicy(ctx, tracker.OrganizationID(c.Param("organization")), tracker.ProjectID(c.Param("project")))
	} else if ok && credential.Runner.Valid() {
		approval, _, err = readProjectPolicy(ctx, s.database.reader, scope)
	} else {
		approval, err = readProjectPolicyWithHistory(ctx, s.database.reader, scope, limit, c.QueryParam("after"))
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if version != "" && approval.Policy.ID != "" {
		workflow, err := projectPolicyWorkflow(ctx, scope, approval.Policy)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		selection := workflowconfig.ResolveCloudModelSelection(workflowconfig.ModelSelection{}, workflowconfig.ModelSelection{})
		if c.Param("organization") != "" {
			projectScope := nativeScope{organization: tracker.OrganizationID(c.Param("organization")), project: tracker.ProjectID(c.Param("project"))}
			if s.config.TenantRepository != nil {
				record, err := s.readProjectRecord(ctx, projectScope)
				if err != nil {
					return s.nativeAPIError(c, err)
				}
				resolved, err := s.projectRecordModel(ctx, record)
				if err != nil {
					return s.nativeAPIError(c, err)
				}
				selection = resolved.Effective
			} else {
				resolved, err := readCloudModelSelection(ctx, s.database.reader, projectScope)
				if err != nil {
					return s.nativeAPIError(c, err)
				}
				selection = resolved.Effective
			}
		}
		approval.ExecutionConfiguration, err = workflowconfig.NewExecutionConfiguration(workflow, approval.Policy, selection)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	var view projectPolicyRead
	if s.config.TenantRepository != nil && c.Param("organization") != "" {
		view = projectPolicyView(approval)
		if !ok || !credential.Runner.Valid() {
			view, err = s.tenantProjectPolicyRead(ctx, tracker.OrganizationID(c.Param("organization")), tracker.ProjectID(c.Param("project")), approval, limit, c.QueryParam("after"))
			if err != nil {
				return s.nativeAPIError(c, err)
			}
		}
	} else {
		view, err = s.projectPolicyRead(ctx, s.database.reader, scope, approval, !ok || !credential.Runner.Valid())
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	return c.JSON(http.StatusOK, view)
}

func (s *Service) tenantProjectPolicyRead(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, approval policy.Approval, limit int, after string) (projectPolicyRead, error) {
	result := projectPolicyView(approval)
	var err error
	result.History, result.HistoryNext, err = s.tenantProjectWorkflowHistory(ctx, organization, project, limit, after)
	if err != nil {
		return projectPolicyRead{}, err
	}
	var rows []policystore.ObservedRevision
	var policyCount int64
	err = s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		rows, policyCount, err = reader.ProjectPolicyObservations(ctx, project, approval.Policy.ID, "", s.config.now(), projectPolicyObservationLimit)
		return err
	})
	if err != nil {
		return projectPolicyRead{}, err
	}
	indexes := make(map[string]int, len(rows))
	for _, row := range rows {
		if _, found := indexes[row.PolicyID]; found {
			continue
		}
		raw, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, eventstore.Receipt{
			OrganizationID:      organization,
			PayloadObjectKey:    row.Revision.PayloadObjectKey,
			PayloadSHA256:       row.Revision.PayloadSHA256,
			PayloadBytes:        row.Revision.PayloadBytes,
			PayloadContentBytes: row.Revision.PayloadContentBytes,
		})
		if err != nil {
			return projectPolicyRead{}, err
		}
		observed, err := decodeProjectPolicy(ctx, string(organization)+"/"+string(project), row.PolicyID, string(raw), policy.Approval{})
		if err != nil {
			return projectPolicyRead{}, err
		}
		if row.PreviouslyApproved && ((row.RepositoryApproved && approval.Policy.Workflow != nil) || observed.Policy.SameAuthoredInputs(approval.Policy) && observed.Policy.Authored != nil && approval.Policy.Authored != nil && observed.Policy.Authored.Version < approval.Policy.Authored.Version) {
			indexes[row.PolicyID] = -1
			continue
		}
		indexes[row.PolicyID] = len(result.ObservedPolicies)
		result.ObservedPolicies = append(result.ObservedPolicies, policy.ObservedPolicy{
			Policy:             observed.Policy,
			RunnerID:           row.RunnerID,
			RunnerIDs:          []string{row.RunnerID},
			ObservedAt:         formatHubTime(row.ObservedAt),
			Source:             row.Source,
			Conflict:           row.DistinctPolicyCount > 1 || row.PreviouslyApproved,
			PreviouslyApproved: row.PreviouslyApproved,
		})
	}
	for _, row := range rows {
		index, found := indexes[row.PolicyID]
		if !found || index < 0 {
			continue
		}
		observed := &result.ObservedPolicies[index]
		if row.RunnerID != observed.RunnerID {
			observed.RunnerIDs = append(observed.RunnerIDs, row.RunnerID)
		}
		if row.RunnerCount > int64(len(observed.RunnerIDs)) {
			observed.RunnerIDsOmitted = int(row.RunnerCount - int64(len(observed.RunnerIDs)))
			result.ObservedPolicyDetailsOmitted = true
		}
	}
	if policyCount > int64(len(indexes)) {
		result.ObservedPoliciesOmitted = int(policyCount - int64(len(indexes)))
		result.ObservedPolicyDetailsOmitted = true
	}
	return result, nil
}

func (s *Service) tenantProjectWorkflowHistory(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, limit int, after string) ([]policy.WorkflowApply, string, error) {
	if limit == 0 {
		limit = defaultAPIPageLimit
	}
	if limit < 1 || limit > maxAPIPageLimit {
		return nil, "", nativePublicInvalid("History page limit is invalid")
	}
	var before int64
	if after != "" {
		parsed, err := strconv.ParseInt(after, 10, 64)
		if err != nil || parsed < 1 {
			return nil, "", nativePublicInvalid("History cursor is invalid")
		}
		before = parsed
	}
	var rows []policystore.WorkflowApply
	var more bool
	var definitions map[string]policystore.Revision
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		rows, err = reader.ProjectWorkflowApplies(ctx, project, before, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			more = true
			rows = rows[:limit]
		}
		definitions = make(map[string]policystore.Revision, len(rows)*2)
		for _, row := range rows {
			for _, id := range []string{row.PreviousPolicyID, row.PolicyID} {
				if id == "" {
					continue
				}
				if _, found := definitions[id]; found {
					continue
				}
				revision, err := reader.ProjectPolicyRevision(ctx, project, id)
				if err != nil {
					return err
				}
				definitions[id] = revision
			}
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	var next string
	if more {
		next = strconv.FormatInt(rows[len(rows)-1].ID, 10)
	}
	content := eventstore.NewContent(s.runDetailStore())
	result := make([]policy.WorkflowApply, 0, len(rows))
	for _, row := range rows {
		entry := policy.WorkflowApply{
			ID: row.ID, Repository: row.Repository, Commit: row.SourceCommit,
			PreviousDefinitionDigest: row.PreviousDefinitionDigest, DefinitionDigest: row.DefinitionDigest,
			RunnerID: row.RunnerID, AppliedBy: row.AppliedBy, AppliedAt: formatHubTime(row.AppliedAt),
			ChangeID: row.ChangeID, PullRequestURL: row.PullRequestURL,
		}
		if previous, found := definitions[row.PreviousPolicyID]; found {
			descriptor, err := s.projectPolicyDescriptor(ctx, content, organization, project, previous)
			if err != nil {
				return nil, "", err
			}
			entry.PreviousDefinition = &descriptor
		}
		if current, found := definitions[row.PolicyID]; found {
			descriptor, err := s.projectPolicyDescriptor(ctx, content, organization, project, current)
			if err != nil {
				return nil, "", err
			}
			entry.Definition = &descriptor
		}
		result = append(result, entry)
	}
	return result, next, nil
}

func (s *Service) projectPolicyDescriptor(ctx context.Context, content *eventstore.Content, organization tracker.OrganizationID, project tracker.ProjectID, revision policystore.Revision) (policy.Descriptor, error) {
	raw, err := content.Receipt(ctx, eventstore.Receipt{
		OrganizationID: organization, PayloadObjectKey: revision.PayloadObjectKey, PayloadSHA256: revision.PayloadSHA256,
		PayloadBytes: revision.PayloadBytes, PayloadContentBytes: revision.PayloadContentBytes,
	})
	if err != nil {
		return policy.Descriptor{}, err
	}
	approval, err := decodeProjectPolicy(ctx, string(organization)+"/"+string(project), revision.PolicyID, string(raw), policy.Approval{})
	return approval.Policy, err
}

func projectPolicyView(approval policy.Approval) projectPolicyRead {
	view := projectPolicyRead{Approval: approval}
	if approval.Policy.ID != "" {
		view.Policy = &view.Approval.Policy
	}
	return view
}

func (s *Service) readTenantProjectPolicy(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID) (policy.Approval, error) {
	approval, _, err := s.readTenantProjectPolicyRevision(ctx, organization, project)
	return approval, err
}

func (s *Service) readTenantProjectPolicyRevision(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID) (policy.Approval, policystore.Revision, error) {
	var revision policystore.Revision
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		revision, err = reader.ProjectPolicy(ctx, project)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return policy.Approval{}, policystore.Revision{}, nil
	}
	if err != nil {
		return policy.Approval{}, policystore.Revision{}, err
	}
	raw, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, eventstore.Receipt{OrganizationID: organization, PayloadObjectKey: revision.PayloadObjectKey, PayloadSHA256: revision.PayloadSHA256, PayloadBytes: revision.PayloadBytes, PayloadContentBytes: revision.PayloadContentBytes})
	if err != nil {
		return policy.Approval{}, policystore.Revision{}, err
	}
	approval, err := decodeProjectPolicy(ctx, string(organization)+"/"+string(project), revision.PolicyID, string(raw), policy.Approval{ApprovedBy: revision.ApprovedBy, ApprovedAt: formatHubTime(revision.ApprovedAt)})
	return approval, revision, err
}

func (s *Service) approveProjectPolicy(c echo.Context) error {
	var change policy.Change
	if err := decodeAPIJSON(c, &change); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if change.ObservedPolicyID == "" {
		if err := change.Policy.Validate(); err != nil {
			return s.nativeAPIError(c, nativeInvalid(err.Error()))
		}
	}
	scope, err := s.policyScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok || credential.ID == "" {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if s.config.TenantRepository != nil && c.Param("organization") != "" {
		scope := nativeScope{organization: tracker.OrganizationID(c.Param("organization")), project: tracker.ProjectID(c.Param("project")), credential: credential, requireHostedAdmin: true}
		approval, err := s.approveTenantProjectPolicy(c.Request().Context(), scope, credential.ID, change)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.JSON(http.StatusOK, approval)
	}
	approval, err := s.database.approvePolicy(c.Request().Context(), scope, credential.ID, change)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, approval)
}

func (s *Service) approveTenantProjectPolicy(ctx context.Context, scope nativeScope, actor string, change policy.Change) (policy.Approval, error) {
	return s.approveTenantProjectPolicyWithReceipt(ctx, scope, actor, change, nil)
}

type tenantPolicyCommandReceipt struct {
	Operation string
	Key       string
	Hash      string
}

func (s *Service) approveTenantProjectPolicyWithReceipt(ctx context.Context, scope nativeScope, actor string, change policy.Change, command *tenantPolicyCommandReceipt) (policy.Approval, error) {
	if s.config.AccessRepository == nil {
		return policy.Approval{}, nativeNotFound()
	}
	previous, previousRevision, err := s.readTenantProjectPolicyRevision(ctx, scope.organization, scope.project)
	if err != nil {
		return policy.Approval{}, err
	}
	if change.ObservedPolicyID != "" {
		if !reflect.ValueOf(change.Policy).IsZero() {
			return policy.Approval{}, nativeInvalid("policy must be omitted when observed_policy_id is supplied")
		}
		if previous.Policy.ID == change.ObservedPolicyID {
			if command != nil {
				return s.recordTenantPolicyNoopReceipt(ctx, scope, previous, previousRevision, command)
			}
			return previous, nil
		}
		observed, err := s.readTenantObservedPolicy(ctx, scope.organization, scope.project, previous.Policy.ID, change.ObservedPolicyID)
		if err != nil {
			return policy.Approval{}, err
		}
		change.Policy = observed
	}
	resolved, err := workflowconfig.ResolveSharedPolicy(change.Policy)
	if err != nil {
		return policy.Approval{}, nativeInvalid(err.Error())
	}
	if previous.Policy.ID != change.ExpectedID && previous.Policy.ID != resolved.ID {
		return policy.Approval{}, policyMismatch("Approved policy changed; inspect the current policy and supply its expected_policy_id")
	}
	if previous.Policy.ID == resolved.ID {
		if command != nil {
			return s.recordTenantPolicyNoopReceipt(ctx, scope, previous, previousRevision, command)
		}
		return previous, nil
	}
	content := eventstore.NewContent(s.runDetailStore())
	revision, revisionFound, err := s.tenantProjectPolicyRevisionForDescriptor(ctx, scope.organization, scope.project, resolved, content)
	if err != nil {
		return policy.Approval{}, err
	}
	now := s.config.now().UTC()
	if !revisionFound {
		raw, err := json.Marshal(resolved)
		if err != nil {
			return policy.Approval{}, err
		}
		reference, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization}, raw)
		if err != nil {
			return policy.Approval{}, err
		}
		gate := resolved.Gates.GitHubPullRequest
		revision = policystore.Revision{OrganizationID: scope.organization, ProjectID: scope.project, PolicyID: resolved.ID, GitHubPullRequest: &gate, PayloadObjectKey: reference.PayloadObjectKey, PayloadSHA256: reference.PayloadSHA256, PayloadBytes: reference.PayloadBytes, PayloadContentBytes: reference.PayloadContentBytes}
	}
	revision.ApprovedBy = actor
	revision.ApprovedAt = now
	result := policy.Approval{Policy: resolved, ApprovedBy: actor, ApprovedAt: formatHubTime(now)}
	var receipt eventstore.Receipt
	if command != nil {
		response, err := json.Marshal(result)
		if err != nil {
			return policy.Approval{}, err
		}
		receipt, err = content.PrepareReceipt(ctx, eventstore.Receipt{
			OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: command.Operation, CommandKey: command.Key,
			RequestHash: command.Hash, CreatedAt: now,
		}, response)
		if err != nil {
			return policy.Approval{}, err
		}
	}
	replayed := false
	receiptReplayed := false
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, writer, scope, true); err != nil {
			return err
		}
		if command != nil {
			prior, err := writer.Receipt(ctx, scope.credential.ID, command.Operation, command.Key)
			if err == nil {
				if prior.RequestHash != command.Hash {
					return nativeConflict(0)
				}
				receiptReplayed = true
				return nil
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
		}
		if err := writer.LockWorkItemProject(ctx, scope.project); err != nil {
			return err
		}
		project, err := writer.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if change.ObservedPolicyID != "" {
			current, err := writer.CurrentProjectPolicyObservation(ctx, scope.project, change.ObservedPolicyID, s.config.now())
			if err != nil {
				return err
			}
			if !current {
				return policyMismatch("Observed repository policy is no longer reported by an authorized runner")
			}
		}
		current, err := writer.ProjectPolicy(ctx, scope.project)
		if errors.Is(err, corestore.ErrNotFound) {
			current = policystore.Revision{}
		} else if err != nil {
			return err
		}
		currentID := current.PolicyID
		if currentID != change.ExpectedID && currentID != resolved.ID {
			return policyMismatch("Approved policy changed; inspect the current policy and supply its expected_policy_id")
		}
		if currentID == resolved.ID {
			if command != nil {
				return corestore.ErrRevisionConflict
			}
			replayed = true
			result = previous
			if command != nil {
				_, err := writer.PutReceipt(ctx, receipt)
				return err
			}
			return nil
		}
		storedRevision, err := writer.ProjectPolicyRevision(ctx, scope.project, resolved.ID)
		if err == nil {
			if !sameTenantProjectPolicyRevisionContent(storedRevision, revision) {
				return corestore.ErrUniqueViolation
			}
			revision = storedRevision
			revision.ApprovedBy = actor
			revision.ApprovedAt = now
		} else if errors.Is(err, corestore.ErrNotFound) {
			if revisionFound {
				return corestore.ErrRevisionConflict
			}
		} else {
			return err
		}
		if project.Core.Profile == "native" {
			if resolved.Workflow == nil && project.Core.WorkflowSource != "" {
				return policyMismatch("Repository-controlled workflow requires a resolved workflow in the policy descriptor; inspect the repository definition with an updated runner")
			}
			if resolved.Workflow != nil {
				project.States = resolved.Workflow.States
				project.Core.WorkflowSource = resolved.Workflow.Source
				project.Core.WorkflowSourceRevision = resolved.Workflow.Revision
				if project.Core.WorkflowSourceRevision == "" {
					project.Core.WorkflowSourceRevision = resolved.SourceRevision
				}
				if _, err := writer.UpdateProjectRecord(ctx, project); err != nil {
					return err
				}
			}
		}
		if err := writer.ApproveProjectPolicy(ctx, revision, change.ExpectedID); err != nil {
			return err
		}
		if currentID != resolved.ID && (resolved.Workflow != nil || previous.Policy.SameAuthoredInputs(resolved)) {
			if err := s.createTenantWorkflowApply(ctx, writer, scope, project, currentID, previous.Policy, resolved, actor, "", now, eventstore.PolicyLanding{}); err != nil {
				return err
			}
		}
		if command != nil {
			_, err := writer.PutReceipt(ctx, receipt)
			return err
		}
		return nil
	})
	if err != nil {
		return policy.Approval{}, err
	}
	if receiptReplayed {
		raw, found, err := s.changeRecordReceipt(ctx, scope, command.Operation, command.Key, command.Hash)
		if err != nil {
			return policy.Approval{}, err
		}
		if !found {
			return policy.Approval{}, corestore.ErrRevisionConflict
		}
		var approval policy.Approval
		if err := json.Unmarshal(raw, &approval); err != nil {
			return policy.Approval{}, err
		}
		return approval, nil
	}
	if replayed {
		return previous, nil
	}
	return result, nil
}

func (s *Service) recordTenantPolicyNoopReceipt(ctx context.Context, scope nativeScope, approval policy.Approval, planned policystore.Revision, command *tenantPolicyCommandReceipt) (policy.Approval, error) {
	raw, err := json.Marshal(approval)
	if err != nil {
		return policy.Approval{}, err
	}
	receipt, err := eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{
		OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: command.Operation,
		CommandKey: command.Key, RequestHash: command.Hash, CreatedAt: s.config.now().UTC(),
	}, raw)
	if err != nil {
		return policy.Approval{}, err
	}
	replayed := false
	var replayReceipt eventstore.Receipt
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, writer, scope, true); err != nil {
			return err
		}
		if err := writer.LockWorkItemProject(ctx, scope.project); err != nil {
			return err
		}
		prior, err := writer.Receipt(ctx, scope.credential.ID, command.Operation, command.Key)
		if err == nil {
			if prior.RequestHash != command.Hash {
				return nativeConflict(0)
			}
			replayed = true
			replayReceipt = prior
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		current, err := writer.ProjectPolicy(ctx, scope.project)
		if err != nil {
			return err
		}
		if !sameNativeClaimProjectPolicy(current, planned) {
			return corestore.ErrRevisionConflict
		}
		_, err = writer.PutReceipt(ctx, receipt)
		return err
	})
	if err != nil {
		return policy.Approval{}, err
	}
	if replayed {
		stored, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, replayReceipt)
		if err != nil {
			return policy.Approval{}, err
		}
		if err := json.Unmarshal(stored, &approval); err != nil {
			return policy.Approval{}, err
		}
	}
	return approval, nil
}

func (s *Service) readTenantObservedPolicy(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, approvedPolicyID, policyID string) (policy.Descriptor, error) {
	var rows []policystore.ObservedRevision
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		rows, _, err = reader.ProjectPolicyObservations(ctx, project, approvedPolicyID, policyID, s.config.now(), 1)
		return err
	})
	if err != nil {
		return policy.Descriptor{}, err
	}
	if len(rows) == 0 {
		return policy.Descriptor{}, policyMismatch("Observed repository policy is no longer reported by an authorized runner")
	}
	row := rows[0]
	raw, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, eventstore.Receipt{
		OrganizationID:      organization,
		PayloadObjectKey:    row.Revision.PayloadObjectKey,
		PayloadSHA256:       row.Revision.PayloadSHA256,
		PayloadBytes:        row.Revision.PayloadBytes,
		PayloadContentBytes: row.Revision.PayloadContentBytes,
	})
	if err != nil {
		return policy.Descriptor{}, err
	}
	approval, err := decodeProjectPolicy(ctx, string(organization)+"/"+string(project), policyID, string(raw), policy.Approval{})
	if err != nil {
		return policy.Descriptor{}, err
	}
	return approval.Policy, nil
}

func (s *Service) observeProjectPolicy(c echo.Context) error {
	var observation policy.Observation
	if err := decodeAPIJSON(c, &observation); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	descriptor, err := workflowconfig.ResolveSharedPolicy(observation.Descriptor)
	if err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	observation.Descriptor = descriptor
	scope, err := s.policyScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok || credential.ID == "" {
		return s.nativeAPIError(c, nativeNotFound())
	}
	reporter := credential.Runner.RunnerID
	if reporter == "" {
		reporter = credential.ID
	}
	if s.config.TenantRepository != nil && c.Param("organization") != "" {
		if err := s.observeTenantProjectPolicy(c.Request().Context(), nativeRequestScope(c), credential, reporter, observation); err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.NoContent(http.StatusNoContent)
	}
	if err := storeObservedPolicy(c.Request().Context(), s.database.db, scope, reporter, observation, s.config.now()); err != nil {
		return s.nativeAPIError(c, err)
	}
	if credential.Runner.RunnerID != "" {
		if err := s.database.applyObservedDefaultBranchPolicy(c.Request().Context(), scope, observation); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) observeTenantProjectPolicy(ctx context.Context, scope nativeScope, credential apiCredential, reporter string, observation policy.Observation) error {
	if s.config.AccessRepository == nil || credential.Runner.RunnerID == "" || credential.Runner.OrganizationID != scope.organization {
		return nativeNotFound()
	}
	previous, previousRevision, err := s.readTenantProjectPolicyRevision(ctx, scope.organization, scope.project)
	if err != nil {
		return err
	}
	content := eventstore.NewContent(s.runDetailStore())
	revision, found, err := s.tenantProjectPolicyRevisionForDescriptor(ctx, scope.organization, scope.project, observation.Descriptor, content)
	if err != nil {
		return err
	}
	if !found {
		raw, err := json.Marshal(observation.Descriptor)
		if err != nil {
			return err
		}
		receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization}, raw)
		if err != nil {
			return err
		}
		gate := observation.Gates.GitHubPullRequest
		revision = policystore.Revision{
			OrganizationID:      scope.organization,
			ProjectID:           scope.project,
			PolicyID:            observation.ID,
			GitHubPullRequest:   &gate,
			PayloadObjectKey:    receipt.PayloadObjectKey,
			PayloadSHA256:       receipt.PayloadSHA256,
			PayloadBytes:        receipt.PayloadBytes,
			PayloadContentBytes: receipt.PayloadContentBytes,
		}
	}
	now := s.config.now().UTC()
	record := policystore.Observation{
		OrganizationID: scope.organization,
		ProjectID:      scope.project,
		RunnerID:       reporter,
		PolicyID:       observation.ID,
		ObservedAt:     now,
		Source:         observation.Source,
		Revision:       revision,
	}
	return s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, writer, scope, true); err != nil {
			return err
		}
		identity, err := writer.RunnerIdentityByCredential(ctx, credential.ID)
		if err != nil {
			return err
		}
		if identity.RunnerID != credential.Runner.RunnerID || identity.OrganizationID != scope.organization || identity.RemovedAt != nil || !identity.ExpiresAt.After(now) {
			return nativeNotFound()
		}
		allowed, err := writer.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.project, now)
		if err != nil {
			return err
		}
		if !allowed {
			return nativeNotFound()
		}
		if err := writer.LockWorkItemProject(ctx, scope.project); err != nil {
			return err
		}
		project, err := writer.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		storedRevision, err := writer.ProjectPolicyRevision(ctx, scope.project, observation.ID)
		if err == nil {
			if !sameTenantProjectPolicyRevisionContent(storedRevision, revision) {
				return corestore.ErrUniqueViolation
			}
			record.Revision = storedRevision
		} else if errors.Is(err, corestore.ErrNotFound) {
			if found {
				return corestore.ErrRevisionConflict
			}
		} else {
			return err
		}
		accepted, err := writer.RecordProjectPolicyObservation(ctx, record)
		if err != nil {
			return err
		}
		if !accepted {
			return nil
		}
		return s.applyObservedTenantDefaultBranchPolicy(ctx, writer, scope, project, reporter, observation, previous, previousRevision, record.Revision, now)
	})
}

func (s *Service) tenantProjectPolicyRevisionForDescriptor(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, descriptor policy.Descriptor, content *eventstore.Content) (policystore.Revision, bool, error) {
	var revision policystore.Revision
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		revision, err = reader.ProjectPolicyRevision(ctx, project, descriptor.ID)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		return policystore.Revision{}, false, err
	}
	if revision.PolicyID == "" {
		return policystore.Revision{}, false, nil
	}
	if err := sameTenantProjectPolicyDescriptor(ctx, content, revision, descriptor); err != nil {
		return policystore.Revision{}, false, err
	}
	return revision, true, nil
}

func sameTenantProjectPolicyDescriptor(ctx context.Context, content *eventstore.Content, revision policystore.Revision, descriptor policy.Descriptor) error {
	if revision.GitHubPullRequest == nil || *revision.GitHubPullRequest != descriptor.Gates.GitHubPullRequest {
		return corestore.ErrUniqueViolation
	}
	stored, err := content.ProjectPolicy(ctx, revision)
	if err != nil {
		return err
	}
	resolved, err := workflowconfig.ResolveSharedPolicy(stored)
	if err != nil {
		return err
	}
	if !sameTenantProjectPolicyDescriptorContent(resolved, descriptor) {
		return corestore.ErrUniqueViolation
	}
	return nil
}

func sameTenantProjectPolicyDescriptorContent(stored, incoming policy.Descriptor) bool {
	if stored.Validate() != nil || incoming.Validate() != nil {
		return false
	}
	if reflect.DeepEqual(stored, incoming) {
		return true
	}
	if stored.ID != incoming.ID || !stored.SameAuthoredInputs(incoming) || stored.Match(incoming) != nil || stored.Workflow == nil || incoming.Workflow == nil {
		return false
	}
	storedWorkflow := *stored.Workflow
	incomingWorkflow := *incoming.Workflow
	storedWorkflow.Revision = ""
	incomingWorkflow.Revision = ""
	stored.Workflow = &storedWorkflow
	incoming.Workflow = &incomingWorkflow
	return reflect.DeepEqual(stored, incoming)
}

func sameTenantProjectPolicyRevisionContent(left, right policystore.Revision) bool {
	if left.GitHubPullRequest == nil || right.GitHubPullRequest == nil || *left.GitHubPullRequest != *right.GitHubPullRequest {
		return false
	}
	return left.OrganizationID == right.OrganizationID && left.ProjectID == right.ProjectID && left.PolicyID == right.PolicyID && left.PayloadObjectKey == right.PayloadObjectKey && left.PayloadSHA256 == right.PayloadSHA256 && left.PayloadBytes == right.PayloadBytes && left.PayloadContentBytes == right.PayloadContentBytes
}

func (s *Service) applyObservedTenantDefaultBranchPolicy(ctx context.Context, writer tenantstore.Writer, scope nativeScope, project projectstore.Record, reporter string, observation policy.Observation, previous policy.Approval, previousRevision, observedRevision policystore.Revision, now time.Time) error {
	descriptor := observation.Descriptor
	current, err := writer.ProjectPolicy(ctx, scope.project)
	if errors.Is(err, corestore.ErrNotFound) {
		current = policystore.Revision{}
	} else if err != nil {
		return err
	}
	if !sameProjectPolicySnapshot(current, previousRevision) {
		return nil
	}
	if current.PolicyID != "" && current.PolicyID != descriptor.ID {
		candidate, err := writer.ProjectPolicyRevision(ctx, scope.project, descriptor.ID)
		if err == nil && candidate.ApprovedBy != "runner-report" {
			return nil
		}
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
	}
	carryover := descriptor.SameAuthoredInputs(previous.Policy)
	landing := eventstore.PolicyLanding{}
	if descriptor.Workflow != nil {
		repository := project.Settings.CheckoutRepository
		if repository == "" {
			repository = project.Settings.Repository
		}
		source := observation.Source
		sourceUnavailable := source == nil || !validCommitID(source.Commit) || !validCommitID(source.DefaultBranchHead) || source.DefaultBranch == "" || source.Commit != descriptor.Workflow.Revision || repository == "" || !strings.EqualFold(repository, source.Repository)
		if !sourceUnavailable {
			landing, err = writer.ProjectPolicyLanding(ctx, scope.project, repository, source.Commit, source.DefaultBranch)
			if errors.Is(err, corestore.ErrNotFound) {
				landing, err = eventstore.PolicyLanding{}, nil
			}
			if err != nil {
				return err
			}
		}
		if !carryover && (sourceUnavailable || !source.DefaultBranchReachable && landing.ChangeID == "" && landing.PullRequestURL == "") {
			return nil
		}
	}
	if current.PolicyID == descriptor.ID {
		return nil
	}
	actor := "repository-default-branch"
	if carryover && previous.ApprovedBy != "" {
		actor = previous.ApprovedBy
	} else if landing.Actor != "" {
		actor = landing.Actor
	}
	if project.Core.Profile == "native" {
		if descriptor.Workflow == nil && project.Core.WorkflowSource != "" {
			return policyMismatch("Repository-controlled workflow requires a resolved workflow in the policy descriptor; inspect the repository definition with an updated runner")
		}
		if descriptor.Workflow != nil {
			project.States = descriptor.Workflow.States
			project.Core.WorkflowSource = descriptor.Workflow.Source
			project.Core.WorkflowSourceRevision = descriptor.Workflow.Revision
			if project.Core.WorkflowSourceRevision == "" {
				project.Core.WorkflowSourceRevision = descriptor.SourceRevision
			}
			if _, err := writer.UpdateProjectRecord(ctx, project); err != nil {
				return err
			}
		}
	}
	approved := observedRevision
	approved.ApprovedBy = actor
	approved.ApprovedAt = now
	if err := writer.ApproveProjectPolicy(ctx, approved, current.PolicyID); err != nil {
		return err
	}
	if descriptor.Workflow == nil && !carryover {
		return nil
	}
	return s.createTenantWorkflowApply(ctx, writer, scope, project, current.PolicyID, previous.Policy, descriptor, actor, reporter, now, landing)
}

func sameProjectPolicySnapshot(current, planned policystore.Revision) bool {
	if current.PolicyID == "" || planned.PolicyID == "" {
		return current.PolicyID == planned.PolicyID
	}
	return sameNativeClaimProjectPolicy(current, planned)
}

func (s *Service) createTenantWorkflowApply(ctx context.Context, writer tenantstore.Writer, scope nativeScope, project projectstore.Record, previousID string, previous policy.Descriptor, descriptor policy.Descriptor, actor, runner string, appliedAt time.Time, landing eventstore.PolicyLanding) error {
	repository := project.Settings.CheckoutRepository
	if repository == "" {
		repository = project.Settings.Repository
	}
	if runner == "" && strings.HasPrefix(actor, "runner_") {
		runner = actor
	}
	previousDigest := previous.SourceDigest
	commit := ""
	if descriptor.Workflow != nil {
		commit = descriptor.Workflow.Revision
	}
	_, err := writer.CreateProjectWorkflowApply(ctx, policystore.WorkflowApply{
		OrganizationID: scope.organization, ProjectID: scope.project, PolicyID: descriptor.ID, PreviousPolicyID: previousID,
		Repository: repository, SourceCommit: commit, PreviousDefinitionDigest: previousDigest,
		DefinitionDigest: descriptor.SourceDigest, RunnerID: runner, AppliedBy: actor, AppliedAt: appliedAt,
		ChangeID: landing.ChangeID, PullRequestURL: landing.PullRequestURL,
	})
	return err
}

func storeObservedPolicy(ctx context.Context, exec hostedExecer, scope, reporter string, observation policy.Observation, now time.Time) error {
	encoded, err := json.Marshal(observation.Descriptor)
	if err != nil {
		return err
	}
	source, err := json.Marshal(observation.Source)
	if err != nil {
		return err
	}
	return storeEncodedObservedPolicy(ctx, exec, scope, reporter, observation.Descriptor, string(encoded), string(source), now)
}

func storeEncodedObservedPolicy(ctx context.Context, exec hostedExecer, scope, reporter string, descriptor policy.Descriptor, encoded, source string, now time.Time) error {
	_, err := exec.ExecContext(ctx, `INSERT INTO project_observed_policies (scope, policy_id, descriptor_json, runner_id, observed_at, source_json) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(scope, runner_id) DO UPDATE SET policy_id = excluded.policy_id, descriptor_json = excluded.descriptor_json, observed_at = excluded.observed_at,
source_json = CASE WHEN excluded.source_json = 'null' AND project_observed_policies.policy_id = excluded.policy_id THEN project_observed_policies.source_json ELSE excluded.source_json END`, scope, descriptor.ID, encoded, reporter, formatHubTime(now), source)
	if err != nil {
		return err
	}
	_, err = exec.ExecContext(ctx, "INSERT INTO policy_revisions (scope, policy_id, metadata_json, approved_by, approved_at, source_digest, workflow_revision,gate_github_pull_request) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING", scope, descriptor.ID, encoded, "runner-report", formatHubTime(now), descriptor.SourceDigest, policyWorkflowRevision(descriptor), descriptor.Gates.GitHubPullRequest)
	return err
}

func readObservedPolicies(ctx context.Context, query nativeQueryer, scope string, approved policy.Descriptor, now time.Time) ([]policy.ObservedPolicy, error) {
	rows, err := query.QueryContext(ctx, `WITH current_reports AS (
SELECT o.* FROM project_observed_policies o
LEFT JOIN runner_identities i ON i.id = o.runner_id
JOIN api_tokens t ON t.id = COALESCE(i.token_id, o.runner_id) AND t.revoked_at IS NULL
AND (t.expires_at IS NULL OR julianday(t.expires_at) > julianday(?))
JOIN token_grants g ON g.token_id = t.id AND g.organization_id || '/' || g.project_id = o.scope
WHERE i.removed_at IS NULL
)
SELECT o.descriptor_json, o.source_json, o.runner_id, o.observed_at, o.policy_id,
EXISTS (SELECT 1 FROM policy_revisions r WHERE r.scope = o.scope AND r.policy_id = o.policy_id AND r.approved_by <> 'runner-report'),
(SELECT count(DISTINCT policy_id) FROM current_reports WHERE scope = o.scope),
EXISTS (SELECT 1 FROM policy_revisions r WHERE r.scope = o.scope AND r.policy_id = ? AND (r.approved_by = 'repository-default-branch' OR EXISTS (SELECT 1 FROM project_workflow_applies a WHERE a.scope = r.scope AND a.definition_digest = r.source_digest AND a.source_commit = r.workflow_revision AND (a.change_id <> '' OR a.pull_request_url <> ''))))
FROM current_reports o WHERE o.scope = ? AND o.policy_id <> ? ORDER BY o.observed_at DESC, o.runner_id`, formatHubTime(now), approved.ID, scope, approved.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []policy.ObservedPolicy{}
	seen := map[string]int{}
	for rows.Next() {
		var raw, source, id string
		var observed policy.ObservedPolicy
		var distinct int
		var repositoryApproved bool
		if err := rows.Scan(&raw, &source, &observed.RunnerID, &observed.ObservedAt, &id, &observed.PreviouslyApproved, &distinct, &repositoryApproved); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if index, ok := seen[id]; ok {
			result[index].RunnerIDs = append(result[index].RunnerIDs, observed.RunnerID)
			continue
		}
		seen[id] = len(result)
		if err := json.Unmarshal([]byte(raw), &observed.Policy); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if observed.PreviouslyApproved && (repositoryApproved && approved.Workflow != nil || observed.Policy.SameAuthoredInputs(approved) && observed.Policy.Authored.Version < approved.Authored.Version) {
			delete(seen, id)
			continue
		}
		if err := json.Unmarshal([]byte(source), &observed.Source); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		observed.Conflict = distinct > 1 || observed.PreviouslyApproved
		observed.RunnerIDs = []string{observed.RunnerID}
		result = append(result, observed)
	}
	return result, errors.Join(rows.Err(), rows.Close())
}

func (s *Service) revokeProjectPolicy(c echo.Context) error {
	var request struct {
		ExpectedID string `json:"expected_policy_id"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope, err := s.policyScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok || credential.ID == "" {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if s.config.TenantRepository != nil && c.Param("organization") != "" {
		return s.nativeAPIError(c, &nativeError{Code: "operation_retired", Message: "Repository configuration does not require execution approval; pause the runner or remove project access to stop work", status: http.StatusGone, publicMessage: true})
	}
	_, err = s.database.revokePolicy(c.Request().Context(), scope, credential.ID, request.ExpectedID)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (d *database) approvePolicy(ctx context.Context, scope, actor string, change policy.Change) (result policy.Approval, resultErr error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, tx.Rollback())
		}
	}()
	result, err = d.approvePolicyInTx(ctx, tx, scope, actor, change, policyLanding{})
	if err != nil {
		return result, err
	}
	return result, d.commit(ctx, tx)
}
func (d *database) approvePolicyInTx(ctx context.Context, tx *sql.Tx, scope, actor string, change policy.Change, landing policyLanding) (result policy.Approval, resultErr error) {
	if change.ObservedPolicyID != "" {
		if !reflect.ValueOf(change.Policy).IsZero() {
			return result, nativePublicInvalid("Supply either observed_policy_id or policy, not both")
		}
		now, err := d.currentTime()
		if err != nil {
			return result, err
		}
		observed, err := readObservedPolicies(ctx, tx, scope, policy.Descriptor{}, now)
		if err != nil {
			return result, err
		}
		for _, candidate := range observed {
			if candidate.Policy.ID == change.ObservedPolicyID {
				change.Policy = candidate.Policy
				break
			}
		}
		if change.Policy.ID == "" {
			return result, nativePublicInvalid("observed_policy_id does not identify a current authorized observation in this project")
		}
	}
	resolved, err := workflowconfig.ResolveSharedPolicy(change.Policy)
	if err != nil {
		return result, nativeInvalid(err.Error())
	}
	change.Policy = resolved
	if err := validateWorkflowPolicy(ctx, tx, scope, change.Policy); err != nil {
		return result, err
	}
	var current string
	err = tx.QueryRowContext(ctx, "SELECT policy_id FROM project_policies WHERE scope = ?", scope).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if current != change.ExpectedID && current != change.Policy.ID {
		return result, policyMismatch("Approved policy changed; inspect the current policy and supply its expected_policy_id")
	}
	now, err := d.currentTime()
	if err != nil {
		return result, err
	}
	now = payloadTimestamp(ctx, now)
	carryover := false
	if current != "" && current != change.Policy.ID {
		previous, err := requireProjectPolicy(ctx, tx, scope)
		if err != nil {
			return result, err
		}
		carryover = change.Policy.SameAuthoredInputs(previous.Policy)
	}
	if current != change.Policy.ID && !carryover {
		organization, project, native := strings.Cut(scope, "/")
		if native && !strings.HasPrefix(scope, "repository:") {
			nativeScope := nativeScope{organization: tracker.OrganizationID(organization), project: tracker.ProjectID(project)}
			var profile, source string
			if err := tx.QueryRowContext(ctx, "SELECT profile, workflow_source FROM projects WHERE organization_id=? AND id=?", organization, project).Scan(&profile, &source); err != nil {
				return result, err
			}
			if profile == "native" {
				if change.Policy.Workflow == nil && source != "" {
					return result, policyMismatch("Repository-controlled workflow requires a resolved workflow in the policy descriptor; inspect the repository definition with an updated runner")
				}
				if change.Policy.Workflow != nil {
					if err := applyNativeProjectStates(ctx, tx, nativeScope, change.Policy.Workflow.States, now); err != nil {
						return result, err
					}
					revision := change.Policy.Workflow.Revision
					if revision == "" {
						revision = change.Policy.SourceRevision
					}
					if _, err := tx.ExecContext(ctx, "UPDATE projects SET workflow_source=?, workflow_source_revision=?, workflow_markdown='', integration_revision=integration_revision+1 WHERE organization_id=? AND id=?", change.Policy.Workflow.Source, revision, organization, project); err != nil {
						return result, err
					}
				}
			}
		}
	}
	if current != change.Policy.ID && (change.Policy.Workflow != nil || carryover) {
		if err := recordWorkflowApply(ctx, tx, scope, actor, current, change.Policy, now, landing); err != nil {
			return result, err
		}
	}
	raw, err := json.Marshal(change.Policy)
	if err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO policy_revisions (scope, policy_id, metadata_json, approved_by, approved_at, source_digest, workflow_revision,gate_github_pull_request) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (scope, policy_id) DO UPDATE SET approved_by = excluded.approved_by, approved_at = excluded.approved_at,gate_github_pull_request=excluded.gate_github_pull_request WHERE policy_revisions.approved_by = 'runner-report' OR ?", scope, change.Policy.ID, string(raw), actor, formatHubTime(now), change.Policy.SourceDigest, policyWorkflowRevision(change.Policy), change.Policy.Gates.GitHubPullRequest, current != change.Policy.ID); err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO project_policies (scope, policy_id) VALUES (?, ?) ON CONFLICT (scope) DO UPDATE SET policy_id = excluded.policy_id WHERE project_policies.policy_id IS NOT excluded.policy_id", scope, change.Policy.ID); err != nil {
		return result, err
	}
	reviewChanged, err := followDefaultChangeReviewPolicy(ctx, tx, scope, change.Policy)
	if err != nil {
		return result, err
	}
	if current != change.Policy.ID || reviewChanged {
		if err := resetNativeBoardPolicy(ctx, tx, scope); err != nil {
			return result, err
		}
	}
	result, err = requireProjectPolicy(ctx, tx, scope)
	if err != nil {
		return result, err
	}
	return result, nil
}

func defaultChangeReviewPolicy(descriptor policy.Descriptor) tracker.ChangeReviewPolicy {
	rules := tracker.ChangeReviewPolicy{PolicyID: descriptor.ID, RequireReview: descriptor.Gates.HumanReview, RequiredChecks: []tracker.ChangeCheckSpec{}}
	rules.ID = changerequest.PolicyID(rules)
	return rules
}

// followsRepositoryGate reports a review policy that pins no CI check. Its
// review requirement is not a separate decision: it follows the repository
// gate, the way the default does, so a descriptor approval may rewrite it.
func followsRepositoryGate(rules tracker.ChangeReviewPolicy) bool {
	return rules.ID != "" && len(rules.RequiredChecks) == 0
}

// followDefaultChangeReviewPolicy keeps a native project's review policy usable
// across a repository policy approval. A project with no review policy, or
// one that pins no checks, gets the default under the approved descriptor. A
// review policy an administrator shaped by pinning checks is never
// rewritten: it goes stale, and publishing says so until the administrator
// approves it again against the new descriptor. A descriptor whose gates the
// default cannot satisfy leaves the project as it is.
func followDefaultChangeReviewPolicy(ctx context.Context, tx *sql.Tx, scope string, descriptor policy.Descriptor) (bool, error) {
	organization, project, ok := strings.Cut(scope, "/")
	if !ok || strings.HasPrefix(scope, "repository:") {
		return false, nil
	}
	native := nativeScope{organization: tracker.OrganizationID(organization), project: tracker.ProjectID(project)}
	current, err := readChangePolicy(ctx, tx, native)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	rules := defaultChangeReviewPolicy(descriptor)
	if err == nil && (current.ID == rules.ID || !followsRepositoryGate(current)) {
		return false, nil
	}
	if changerequest.ValidatePolicy(rules, descriptor) != nil {
		return false, nil
	}
	raw, err := json.Marshal(rules)
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO change_review_policies (organization_id, project_id, policy_json) VALUES (?, ?, ?)
ON CONFLICT (organization_id, project_id) DO UPDATE SET policy_json = excluded.policy_json`, native.organization, native.project, string(raw))
	return err == nil, err
}

// backfillChangeReviewPolicies gives every project with an approved
// repository policy the review policy approving it now seeds: projects
// approved before seeding existed had none, so no run could publish a
// version, and projects seeded while the default required review follow the
// repository gate like any other default. It is hub migration 40.
func backfillChangeReviewPolicies(ctx context.Context, tx *sql.Tx) error {
	scopes, err := approvedProjectScopes(ctx, tx)
	if err != nil {
		return err
	}
	for _, scope := range scopes {
		approval, err := requireProjectPolicy(ctx, tx, scope)
		if err != nil {
			continue
		}
		if _, err := followDefaultChangeReviewPolicy(ctx, tx, scope, approval.Policy); err != nil {
			return fmt.Errorf("backfill review policy for %s: %w", scope, err)
		}
	}
	return nil
}

func approvedProjectScopes(ctx context.Context, tx *sql.Tx) (scopes []string, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT scope FROM project_policies WHERE scope NOT LIKE 'repository:%' ORDER BY scope`)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var scope string
		if err := rows.Scan(&scope); err != nil {
			return nil, err
		}
		scopes = append(scopes, scope)
	}
	return scopes, rows.Err()
}

type policyQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readProjectPolicy(ctx context.Context, db policyQuerier, scope string) (policy.Approval, bool, error) {
	var result policy.Approval
	var id, raw string
	err := db.QueryRowContext(ctx, `SELECT r.policy_id, r.metadata_json, r.approved_by, r.approved_at
FROM project_policies p JOIN policy_revisions r ON r.scope = p.scope AND r.policy_id = p.policy_id WHERE p.scope = ?`, scope).Scan(&id, &raw, &result.ApprovedBy, &result.ApprovedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	result, err = decodeProjectPolicy(ctx, scope, id, raw, result)
	return result, err == nil, err
}

func requireProjectPolicy(ctx context.Context, db policyQuerier, scope string) (policy.Approval, error) {
	approval, found, err := readProjectPolicy(ctx, db, scope)
	if err != nil {
		return approval, err
	}
	if !found {
		return approval, errNoApprovedRepositoryPolicy
	}
	return approval, nil
}

type projectPolicyRead struct {
	policy.Approval
	Policy                       *policy.Descriptor      `json:"policy"`
	ObservedPolicies             []policy.ObservedPolicy `json:"observed_policies,omitempty"`
	ObservedPolicyDetailsOmitted bool                    `json:"observed_policy_details_omitted,omitempty"`
	ObservedPoliciesOmitted      int                     `json:"observed_policies_omitted,omitempty"`
}

func (s *Service) projectPolicyRead(ctx context.Context, query nativeQueryer, scope string, approval policy.Approval, observations bool) (projectPolicyRead, error) {
	result := projectPolicyRead{Approval: approval}
	if approval.Policy.ID != "" {
		result.Policy = &result.Approval.Policy
	}
	if observations {
		var err error
		result.ObservedPolicies, err = readObservedPolicies(ctx, query, scope, approval.Policy, s.config.now())
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

const projectPolicyCacheLimit = 256

type projectPolicyCacheKey struct {
	scope string
	id    string
}

type projectPolicyCache struct {
	mu       sync.Mutex
	entries  map[projectPolicyCacheKey]*workflowconfig.ResolvedNativePolicy
	inflight map[projectPolicyCacheKey]*projectPolicyDecode
	order    []projectPolicyCacheKey
	oldest   int
	decode   func(string) (*workflowconfig.ResolvedNativePolicy, error)
}

type projectPolicyDecode struct {
	done  chan struct{}
	value *workflowconfig.ResolvedNativePolicy
	err   error
}

type projectPolicyCacheContextKey struct{}

func projectPolicyCacheFromContext(ctx context.Context) *projectPolicyCache {
	cache, ok := ctx.Value(projectPolicyCacheContextKey{}).(*projectPolicyCache)
	if !ok {
		return nil
	}
	return cache
}

func (d *database) policyContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, projectPolicyCacheContextKey{}, &d.policies)
}

func (s *Service) policyCacheMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.SetRequest(c.Request().WithContext(s.database.policyContext(c.Request().Context())))
		return next(c)
	}
}

func decodeResolvedProjectPolicy(raw string) (*workflowconfig.ResolvedNativePolicy, error) {
	var descriptor policy.Descriptor
	if err := json.Unmarshal([]byte(raw), &descriptor); err != nil {
		return nil, err
	}
	return workflowconfig.ResolveNativePolicy(descriptor)
}

func (c *projectPolicyCache) resolve(scope, id, raw string) (*workflowconfig.ResolvedNativePolicy, error) {
	if c == nil {
		return decodeResolvedProjectPolicy(raw)
	}
	c.mu.Lock()
	key := projectPolicyCacheKey{scope: scope, id: id}
	if value, ok := c.entries[key]; ok {
		c.mu.Unlock()
		return value, nil
	}
	if pending, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		<-pending.done
		return pending.value, pending.err
	}
	pending := &projectPolicyDecode{done: make(chan struct{})}
	if c.inflight == nil {
		c.inflight = make(map[projectPolicyCacheKey]*projectPolicyDecode)
	}
	c.inflight[key] = pending
	decode := c.decode
	c.mu.Unlock()
	if decode == nil {
		decode = decodeResolvedProjectPolicy
	}
	value, err := func() (*workflowconfig.ResolvedNativePolicy, error) {
		defer func() {
			if value := recover(); value != nil {
				c.mu.Lock()
				pending.err = errors.New("decode project policy panicked")
				delete(c.inflight, key)
				close(pending.done)
				c.mu.Unlock()
				panic(value)
			}
		}()
		return decode(raw)
	}()
	c.mu.Lock()
	defer c.mu.Unlock()
	pending.value, pending.err = value, err
	delete(c.inflight, key)
	close(pending.done)
	if err != nil {
		return nil, err
	}
	if c.entries == nil {
		c.entries = make(map[projectPolicyCacheKey]*workflowconfig.ResolvedNativePolicy)
	}
	if len(c.order) == projectPolicyCacheLimit {
		delete(c.entries, c.order[c.oldest])
		c.order[c.oldest] = key
		c.oldest = (c.oldest + 1) % projectPolicyCacheLimit
	} else {
		c.order = append(c.order, key)
	}
	c.entries[key] = value
	return value, nil
}

func decodeProjectPolicy(ctx context.Context, scope, id, raw string, result policy.Approval) (policy.Approval, error) {
	cache := projectPolicyCacheFromContext(ctx)
	resolved, err := cache.resolve(scope, id, raw)
	if err != nil {
		return result, err
	}
	result.Policy = resolved.Descriptor()
	return result, nil
}

func projectPolicyWorkflow(ctx context.Context, scope string, descriptor policy.Descriptor) (workflowconfig.Workflow, error) {
	cache := projectPolicyCacheFromContext(ctx)
	if cache != nil {
		cache.mu.Lock()
		resolved := cache.entries[projectPolicyCacheKey{scope: scope, id: descriptor.ID}]
		cache.mu.Unlock()
		if resolved != nil {
			return resolved.Workflow()
		}
	}
	return workflowconfig.ApplyNativePolicy(workflowconfig.Workflow{Config: workflowconfig.Default()}, descriptor)
}

func claimPolicyScope(query claimCandidateQuery) (string, error) {
	if query.NativeScope != nil {
		return string(query.NativeScope.organization) + "/" + string(query.NativeScope.project), nil
	}
	if len(query.Repositories) != 1 {
		return "", policyMismatch("A repository policy claim requires exactly one repository")
	}
	return "repository:" + strings.ToLower(strings.TrimSpace(query.Repositories[0])), nil
}

func validateWorkflowPolicy(ctx context.Context, db policyQuerier, scope string, descriptor policy.Descriptor) error {
	organization, project, native := strings.Cut(scope, "/")
	if !native || strings.HasPrefix(scope, "repository:") || descriptor.Workflow != nil {
		return nil
	}
	var profile, markdown string
	if err := db.QueryRowContext(ctx, "SELECT profile, workflow_markdown FROM projects WHERE organization_id=? AND id=?", organization, project).Scan(&profile, &markdown); err != nil {
		return err
	}
	if profile != "native" || markdown == "" {
		return nil
	}
	workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{WorkflowPath: "Cloud WORKFLOW.md", Workflow: []byte(markdown)})
	if err != nil {
		return policyMismatch("Saved Cloud workflow is invalid; correct its Markdown definition before approving execution")
	}
	if descriptor.Authored != nil && len(descriptor.Authored.Files) > 0 {
		workflow.Definition.Layout = workflowconfig.ProjectDefinitionCloud
		expected, err := workflowconfig.ResolvePolicy(workflow)
		if err != nil {
			return err
		}
		if descriptor.Authored.Files["WORKFLOW.md"] != expected.Authored.Files["WORKFLOW.md"] {
			return policyMismatch("Cloud workflow changed; load its current Markdown definition and approve the resolved policy before claiming work")
		}
	} else if descriptor.Configuration == nil || descriptor.Configuration.DefinitionDigest != workflow.SourceHash {
		return policyMismatch("Cloud workflow changed; load its current Markdown definition and approve the resolved policy before claiming work")
	}
	return nil
}

func validateClaimPolicy(ctx context.Context, tx *sql.Tx, query claimCandidateQuery, machine tracker.MachineID) (string, error) {
	scope, err := claimPolicyScope(query)
	if err != nil {
		return "", err
	}
	var raw string
	if err := tx.QueryRowContext(ctx, "SELECT metadata_json FROM policy_revisions WHERE scope = ? AND policy_id = ?", scope, query.PolicyID).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nativePublicInvalid("Claim configuration has not been reported for this project")
		}
		return "", err
	}
	approval, err := decodeProjectPolicy(ctx, scope, query.PolicyID, raw, policy.Approval{})
	if err != nil {
		return "", err
	}
	var runnerID string
	if query.NativeScope != nil {
		runnerID = query.NativeScope.credential.Runner.RunnerID
	}
	var tags []string
	if runnerID == "" && (approval.Policy.Requirements.RunnerID != "" || approval.Policy.Requirements.MachineID != "" || len(approval.Policy.Requirements.RequiredTags) != 0) {
		return "", &nativeError{Code: "selector_no_match", Message: "Constrained routing requires an administrator-enrolled runner; legacy registration cannot assert trusted host identity or tags", status: http.StatusConflict, publicMessage: true}
	}
	if runnerID != "" {
		var raw string
		if err := tx.QueryRowContext(ctx, "SELECT tags_json FROM runner_identities WHERE id = ? AND machine_id = ? AND organization_id = ?", runnerID, machine, query.NativeScope.organization).Scan(&raw); err != nil {
			return "", err
		}
		if err := json.Unmarshal([]byte(raw), &tags); err != nil {
			return "", err
		}
	}
	if err := approval.Policy.Requirements.Match(runnerID, string(machine), tags); err != nil {
		return "", &nativeError{Code: "selector_no_match", Message: err.Error(), status: http.StatusConflict}
	}
	return scope, nil
}

func (d *database) leasePolicyID(ctx context.Context, lease tracker.LeaseID) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	var id string
	err := d.reader.QueryRowContext(ctx, "SELECT policy_id FROM lease_policies WHERE lease_id = ?", lease).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func requireLeaseRouting(ctx context.Context, tx nativeQueryer, lease tracker.LeaseID) error {
	var runner, machine, tags, state, raw string
	var access bool
	err := tx.QueryRowContext(ctx, `SELECT r.id, r.machine_id, r.tags_json, r.state, p.metadata_json,
`+runnerProjectAccessSQL("i.organization_id", "i.project_id")+`
FROM lease_runners lr JOIN runner_identities r ON r.id = lr.runner_id JOIN leases l ON l.lease_id = lr.lease_id
JOIN issues i ON i.id = l.issue_id JOIN lease_policies lp ON lp.lease_id = l.lease_id
JOIN policy_revisions p ON p.scope = lp.scope AND p.policy_id = lp.policy_id WHERE lr.lease_id = ?`, lease).Scan(&runner, &machine, &tags, &state, &raw, &access)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !access {
		return nativeNotFoundAt("lease_routing_grant")
	}
	if state == "disabled" {
		return &nativeError{Code: "runner_disabled", Message: "Runner is disabled", status: http.StatusConflict, publicMessage: true}
	}
	var descriptor policy.Descriptor
	var authorizedTags []string
	if err := json.Unmarshal([]byte(raw), &descriptor); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(tags), &authorizedTags); err != nil {
		return err
	}
	if err := descriptor.Requirements.Match(runner, machine, authorizedTags); err != nil {
		return &nativeError{Code: "selector_no_match", Message: err.Error(), status: http.StatusConflict}
	}
	return nil
}

func (d *database) revokePolicy(ctx context.Context, scope, actor, expectedID string) (result any, resultErr error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, tx.Rollback())
		}
	}()
	now, err := d.currentTime()
	if err != nil {
		return nil, err
	}
	result, err = revokeProjectPolicyInTx(ctx, tx, scope, actor, expectedID, now)
	if err != nil {
		return nil, err
	}
	return result, d.commit(ctx, tx)
}

func revokeProjectPolicyInTx(ctx context.Context, tx *sql.Tx, scope, actor, expectedID string, now time.Time) (any, error) {
	return nil, &nativeError{Code: "operation_retired", Message: "Repository configuration does not require execution approval; pause the runner or remove project access to stop work", status: http.StatusGone, publicMessage: true}
}
