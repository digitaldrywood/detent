package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/changerequest"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const projectPolicyObservationLimit = 512

func (s *Service) policyScope(c echo.Context) (string, error) {
	if s.config.TenantRepository == nil {
		return "", hubapi.NativeWorkItemStorageUnavailable()
	}
	if c.Param("organization") == "" || c.Param("project") == "" {
		return "", hubapi.NativeNotFound()
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	scope := hubscope.NativeScope{Organization: tracker.OrganizationID(c.Param("organization")), Project: tracker.ProjectID(c.Param("project"))}
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		_, err := reader.ProjectRecord(ctx, scope.Project)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return "", hubapi.NativeNotFound()
	}
	if err != nil {
		return "", err
	}
	return string(scope.Organization) + "/" + string(scope.Project), nil
}

func (s *Service) getProjectPolicy(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	version := c.QueryParam("execution_configuration")
	if version != "" && version != "1" && version != strconv.Itoa(policy.ExecutionConfigurationVersion) {
		return s.nativeAPIError(c, hubapi.NativeInvalid("unsupported execution configuration version"))
	}
	scope, err := s.policyScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	limit, err := hubapi.ParsePageLimit(c.QueryParam("limit"))
	if err != nil {
		return s.nativeAPIError(c, hubapi.NativeInvalid(err.Error()))
	}
	var approval policy.Approval
	credential, ok := c.Get("hub_api_credential").(hubapi.APICredential)
	approval, err = s.readTenantProjectPolicy(ctx, tracker.OrganizationID(c.Param("organization")), tracker.ProjectID(c.Param("project")))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if version != "" && approval.Policy.ID != "" {
		if version == "1" && approval.Policy.Authored != nil && approval.Policy.Authored.Version > 3 {
			return s.nativeAPIError(c, hubapi.NativePublicInvalid("this approved policy requires execution configuration version 2; upgrade the runner"))
		}
		workflow, err := hubrecords.ProjectPolicyWorkflow(ctx, scope, approval.Policy)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		selection := workflowconfig.ResolveCloudModelSelection(workflowconfig.ModelSelection{}, workflowconfig.ModelSelection{})
		if c.Param("organization") != "" {
			projectScope := hubscope.NativeScope{Organization: tracker.OrganizationID(c.Param("organization")), Project: tracker.ProjectID(c.Param("project"))}
			record, err := s.readProjectRecord(ctx, projectScope)
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			resolved, err := s.projectRecordModel(ctx, record)
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			selection = resolved.Effective
		}
		approval.ExecutionConfiguration, err = workflowconfig.NewExecutionConfiguration(workflow, approval.Policy, selection)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		if version == "1" {
			approval.ExecutionConfiguration.Version = 1
		}
	}
	view := projectPolicyView(approval)
	if !ok || !credential.Runner.Valid() {
		view, err = s.tenantProjectPolicyRead(ctx, tracker.OrganizationID(c.Param("organization")), tracker.ProjectID(c.Param("project")), approval, limit, c.QueryParam("after"))
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
		rows, policyCount, err = reader.ProjectPolicyObservations(ctx, project, approval.Policy.ID, "", s.config.Clock(), projectPolicyObservationLimit)
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
			ObservedAt:         hubapi.FormatHubTime(row.ObservedAt),
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
		limit = hubapi.DefaultAPIPageLimit
	}
	if limit < 1 || limit > hubapi.MaxAPIPageLimit {
		return nil, "", hubapi.NativePublicInvalid("History page limit is invalid")
	}
	var before int64
	if after != "" {
		parsed, err := strconv.ParseInt(after, 10, 64)
		if err != nil || parsed < 1 {
			return nil, "", hubapi.NativePublicInvalid("History cursor is invalid")
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
		for index := range rows {
			row := &rows[index]
			if row.ChangeID != "" || row.PullRequestURL != "" || row.Repository == "" || !hubapi.ValidCommitID(row.SourceCommit) {
				continue
			}
			landing, err := reader.ProjectPolicyLanding(ctx, project, row.Repository, row.SourceCommit, "")
			if errors.Is(err, corestore.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			row.ChangeID, row.PullRequestURL = landing.ChangeID, landing.PullRequestURL
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
			RunnerID: row.RunnerID, AppliedBy: row.AppliedBy, AppliedAt: hubapi.FormatHubTime(row.AppliedAt),
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
	approval, err := s.decodeTenantProjectPolicy(ctx, organization, project, revision)
	return approval, revision, err
}

func (s *Service) decodeTenantProjectPolicy(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, revision policystore.Revision) (policy.Approval, error) {
	raw, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, eventstore.Receipt{OrganizationID: organization, PayloadObjectKey: revision.PayloadObjectKey, PayloadSHA256: revision.PayloadSHA256, PayloadBytes: revision.PayloadBytes, PayloadContentBytes: revision.PayloadContentBytes})
	if err != nil {
		return policy.Approval{}, err
	}
	return decodeProjectPolicy(ctx, string(organization)+"/"+string(project), revision.PolicyID, string(raw), policy.Approval{ApprovedBy: revision.ApprovedBy, ApprovedAt: hubapi.FormatHubTime(revision.ApprovedAt)})
}

func (s *Service) approveProjectPolicy(c echo.Context) error {
	var change policy.Change
	if err := hubapi.DecodeAPIJSON(c, &change); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if change.ObservedPolicyID == "" {
		if err := change.Policy.Validate(); err != nil {
			if errors.Is(err, policy.ErrMissingAuthoredWorkflow) {
				return s.nativeAPIError(c, hubapi.NativePublicInvalid(err.Error()))
			}
			return s.nativeAPIError(c, hubapi.NativeInvalid(err.Error()))
		}
	}
	_, err := s.policyScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(hubapi.APICredential)
	if !ok || credential.ID == "" {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	scope := hubscope.NativeScope{Organization: tracker.OrganizationID(c.Param("organization")), Project: tracker.ProjectID(c.Param("project")), Credential: credential, RequireHostedAdmin: true}
	approval, err := s.approveTenantProjectPolicy(c.Request().Context(), scope, credential.ID, change)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, approval)
}

func (s *Service) approveTenantProjectPolicy(ctx context.Context, scope hubscope.NativeScope, actor string, change policy.Change) (policy.Approval, error) {
	return s.approveTenantProjectPolicyWithReceipt(ctx, scope, actor, change, nil)
}

type tenantPolicyCommandReceipt struct {
	Operation string
	Key       string
	Hash      string
}

func (s *Service) approveTenantProjectPolicyWithReceipt(ctx context.Context, scope hubscope.NativeScope, actor string, change policy.Change, command *tenantPolicyCommandReceipt) (policy.Approval, error) {
	if s.config.AccessRepository == nil {
		return policy.Approval{}, hubapi.NativeNotFound()
	}
	previous, previousRevision, err := s.readTenantProjectPolicyRevision(ctx, scope.Organization, scope.Project)
	if err != nil {
		return policy.Approval{}, err
	}
	if change.ObservedPolicyID != "" {
		if !reflect.ValueOf(change.Policy).IsZero() {
			return policy.Approval{}, hubapi.NativeInvalid("policy must be omitted when observed_policy_id is supplied")
		}
		change.ObservedPolicyID, err = s.resolveObservedApprovalID(ctx, scope, previous.Policy, change.ObservedPolicyID)
		if err != nil {
			return policy.Approval{}, err
		}
		if change.ObservedPolicyID == previous.Policy.ID {
			return s.reconcileTenantPolicyApproval(ctx, scope, previous, previousRevision, command)
		}
		observed, err := s.readTenantObservedPolicy(ctx, scope.Organization, scope.Project, previous.Policy.ID, change.ObservedPolicyID)
		if err != nil {
			return policy.Approval{}, err
		}
		change.Policy = observed
	}
	resolved, err := workflowconfig.ResolveSharedPolicy(change.Policy)
	if errors.Is(err, workflowconfig.ErrNativeAuthoredMismatch) {
		return policy.Approval{}, hubapi.NativePublicInvalid(workflowconfig.ErrNativeAuthoredMismatch.Error())
	}
	if err != nil {
		return policy.Approval{}, hubapi.NativeInvalid(err.Error())
	}
	if err := workflowconfig.ValidateNativePromptInputs(resolved); err != nil {
		return policy.Approval{}, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	canonical, revision, revisionFound, err := s.prepareTenantPolicyRevision(ctx, scope, resolved, content)
	if err != nil {
		return policy.Approval{}, err
	}
	if previous.Policy.ID != change.ExpectedID && previous.Policy.ID != canonical.ID {
		return policy.Approval{}, hubapi.PolicyMismatch("Approved policy changed; inspect the current policy and supply its expected_policy_id")
	}
	if previous.Policy.ID == canonical.ID {
		return s.reconcileTenantPolicyApproval(ctx, scope, previous, previousRevision, command)
	}
	now := s.config.Clock().UTC().Truncate(time.Microsecond)
	if !revisionFound || revision.ApprovedBy == "runner-report" {
		revision.ApprovedBy = actor
		revision.ApprovedAt = now
	}
	review, err := s.prepareDefaultReviewPolicy(ctx, scope, canonical)
	if err != nil {
		return policy.Approval{}, err
	}
	result := policy.Approval{Policy: canonical, ApprovedBy: revision.ApprovedBy, ApprovedAt: hubapi.FormatHubTime(revision.ApprovedAt)}
	var receipt eventstore.Receipt
	if command != nil {
		response, err := json.Marshal(result)
		if err != nil {
			return policy.Approval{}, err
		}
		receipt, err = content.PrepareReceipt(ctx, eventstore.Receipt{
			OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: command.Operation, CommandKey: command.Key,
			RequestHash: command.Hash, CreatedAt: now,
		}, response)
		if err != nil {
			return policy.Approval{}, err
		}
	}
	var concurrent policystore.Revision
	receiptReplayed := false
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, writer, scope, true); err != nil {
			return err
		}
		if command != nil {
			prior, err := writer.Receipt(ctx, scope.Credential.ID, command.Operation, command.Key)
			if err == nil {
				if prior.RequestHash != command.Hash {
					return hubapi.NativeConflict(0)
				}
				receiptReplayed = true
				return nil
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
		}
		if err := writer.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		project, err := writer.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if change.ObservedPolicyID != "" {
			current, err := writer.CurrentProjectPolicyObservation(ctx, scope.Project, change.ObservedPolicyID, s.config.Clock())
			if err != nil {
				return err
			}
			if !current {
				return hubapi.PolicyMismatch("Observed repository policy is no longer reported by an authorized runner")
			}
		}
		current, err := writer.ProjectPolicy(ctx, scope.Project)
		if errors.Is(err, corestore.ErrNotFound) {
			current = policystore.Revision{}
		} else if err != nil {
			return err
		}
		currentID := current.PolicyID
		if currentID != change.ExpectedID && currentID != canonical.ID {
			return hubapi.PolicyMismatch("Approved policy changed; inspect the current policy and supply its expected_policy_id")
		}
		if currentID == canonical.ID {
			concurrent = current
			return nil
		}
		if err := s.chargeTenantPolicyCommand(ctx, writer, scope, command); err != nil {
			return err
		}
		storedRevision, err := writer.ProjectPolicyRevision(ctx, scope.Project, canonical.ID)
		if err == nil {
			if !sameTenantProjectPolicyRevisionContent(storedRevision, revision) {
				return corestore.ErrUniqueViolation
			}
			if storedRevision.ApprovedBy != "runner-report" && (storedRevision.ApprovedBy != revision.ApprovedBy || !storedRevision.ApprovedAt.Equal(revision.ApprovedAt)) {
				return corestore.ErrRevisionConflict
			}
		} else if errors.Is(err, corestore.ErrNotFound) {
			if revisionFound {
				return corestore.ErrRevisionConflict
			}
		} else {
			return err
		}
		if err := reconcileTenantWorkflowPolicy(ctx, writer, project, canonical); err != nil {
			return err
		}
		if err := writer.ApproveProjectPolicy(ctx, revision, change.ExpectedID); err != nil {
			return err
		}
		if err := applyDefaultReviewPolicy(ctx, writer, review, actor, now); err != nil {
			return err
		}
		if currentID != canonical.ID && (canonical.Workflow != nil || previous.Policy.SameAuthoredInputs(canonical)) {
			reporters, _, err := writer.ProjectPolicyObservations(ctx, scope.Project, "", canonical.ID, now, 1)
			if err != nil {
				return err
			}
			reporter := ""
			if len(reporters) > 0 {
				reporter = reporters[0].RunnerID
			}
			if err := s.createTenantWorkflowApply(ctx, writer, scope, project, currentID, previous.Policy, canonical, actor, reporter, now, eventstore.PolicyLanding{}, false); err != nil {
				return err
			}
		}
		if command != nil {
			_, err := writer.PutReceipt(ctx, receipt)
			return err
		}
		return nil
	})
	if errors.Is(err, projectstore.ErrStateInUse) {
		return policy.Approval{}, hubapi.ProjectRecordError(err)
	}
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
	if concurrent.PolicyID != "" {
		approval, err := s.decodeTenantProjectPolicy(ctx, scope.Organization, scope.Project, concurrent)
		if err != nil {
			return policy.Approval{}, err
		}
		if !sameTenantProjectPolicyDescriptorContent(approval.Policy, canonical) {
			return policy.Approval{}, corestore.ErrUniqueViolation
		}
		return s.reconcileTenantPolicyApproval(ctx, scope, approval, concurrent, command)
	}
	return result, nil
}

func (s *Service) chargeTenantPolicyCommand(ctx context.Context, writer tenantstore.Writer, scope hubscope.NativeScope, command *tenantPolicyCommandReceipt) error {
	if command == nil {
		return s.chargeProjectCommand(ctx, writer, scope, projectOperationID(scope, "PUT", "/policy"), "")
	}
	return s.chargeProjectCommand(ctx, writer, scope, command.Operation, command.Key)
}

func (s *Service) reconcileTenantPolicyApproval(ctx context.Context, scope hubscope.NativeScope, approval policy.Approval, planned policystore.Revision, command *tenantPolicyCommandReceipt) (policy.Approval, error) {
	review, err := s.prepareDefaultReviewPolicy(ctx, scope, approval.Policy)
	if err != nil {
		return policy.Approval{}, err
	}
	var receipt eventstore.Receipt
	if command != nil {
		raw, err := json.Marshal(approval)
		if err != nil {
			return policy.Approval{}, err
		}
		receipt, err = eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{
			OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: command.Operation,
			CommandKey: command.Key, RequestHash: command.Hash, CreatedAt: s.config.Clock().UTC(),
		}, raw)
		if err != nil {
			return policy.Approval{}, err
		}
	}
	snapshot, err := s.readTenantProjectRecord(ctx, scope)
	if err != nil {
		return policy.Approval{}, err
	}
	var cloudWorkflowErr error
	if approval.Policy.Workflow == nil && snapshot.Core.Profile == "native" {
		markdown, err := s.projectRecordWorkflow(ctx, snapshot)
		if err != nil {
			return policy.Approval{}, err
		}
		cloudWorkflowErr = validateCloudWorkflowPolicy(markdown, approval.Policy)
	}
	replayed := false
	var replayReceipt eventstore.Receipt
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, writer, scope, true); err != nil {
			return err
		}
		if err := writer.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		if command != nil {
			prior, err := writer.Receipt(ctx, scope.Credential.ID, command.Operation, command.Key)
			if err == nil {
				if prior.RequestHash != command.Hash {
					return hubapi.NativeConflict(0)
				}
				replayed = true
				replayReceipt = prior
				return nil
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
		}
		current, err := writer.ProjectPolicy(ctx, scope.Project)
		if err != nil {
			return err
		}
		if !hubrecords.SameNativeClaimProjectPolicy(current, planned) {
			return corestore.ErrRevisionConflict
		}
		project, err := writer.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.Core.Profile != snapshot.Core.Profile || project.Settings.WorkflowKey != snapshot.Settings.WorkflowKey || project.Settings.WorkflowSHA256 != snapshot.Settings.WorkflowSHA256 {
			return corestore.ErrRevisionConflict
		}
		if err := validateApprovedWorkflowContent(project, approval.Policy); err != nil {
			return err
		}
		if cloudWorkflowErr != nil {
			return cloudWorkflowErr
		}
		if err := s.chargeTenantPolicyCommand(ctx, writer, scope, command); err != nil {
			return err
		}
		if err := reconcileTenantWorkflowPolicy(ctx, writer, project, approval.Policy); err != nil {
			return err
		}
		if err := applyDefaultReviewPolicy(ctx, writer, review, approval.ApprovedBy, s.config.Clock().UTC()); err != nil {
			return err
		}
		if command != nil {
			_, err = writer.PutReceipt(ctx, receipt)
		}
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

func (s *Service) resolveObservedApprovalID(ctx context.Context, scope hubscope.NativeScope, current policy.Descriptor, requested string) (string, error) {
	if requested != current.ID && requested != current.OriginalAuthored().ID {
		return requested, nil
	}
	if current.Authored != nil && len(current.Authored.Files) == 0 {
		pinned := current.WithPinnedFiles().ID
		var reported bool
		err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			reported, err = r.CurrentProjectPolicyObservation(ctx, scope.Project, pinned, s.config.Clock())
			return err
		})
		if err != nil {
			return "", err
		}
		if reported && pinned != current.ID {
			return pinned, nil
		}
	}
	return current.ID, workflowconfig.ValidateNativePromptInputs(current)
}

func (s *Service) readTenantObservedPolicy(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, approvedPolicyID, policyID string) (policy.Descriptor, error) {
	var rows []policystore.ObservedRevision
	unreported := false
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		rows, _, err = reader.ProjectPolicyObservations(ctx, project, approvedPolicyID, policyID, s.config.Clock(), 1)
		if err != nil || len(rows) > 0 {
			return err
		}
		_, err = reader.ProjectPolicyRevision(ctx, project, policyID)
		if errors.Is(err, corestore.ErrNotFound) {
			unreported = true
			return nil
		}
		return err
	})
	if err != nil {
		return policy.Descriptor{}, err
	}
	if unreported {
		return policy.Descriptor{}, hubapi.NativePublicInvalid("observed_policy_id does not identify a current authorized observation in this project")
	}
	if len(rows) == 0 {
		return policy.Descriptor{}, hubapi.PolicyMismatch("Observed repository policy is no longer reported by an authorized runner")
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
	if err := hubapi.DecodeAPIJSON(c, &observation); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	descriptor, err := workflowconfig.ResolveSharedPolicy(observation.Descriptor)
	if err != nil {
		return s.nativeAPIError(c, hubapi.NativeInvalid(err.Error()))
	}
	observation.Descriptor = descriptor
	_, err = s.policyScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(hubapi.APICredential)
	if !ok || credential.ID == "" {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	reporter := credential.Runner.RunnerID
	if reporter == "" {
		reporter = credential.ID
	}
	if err := s.observeTenantProjectPolicy(c.Request().Context(), hubscope.NativeRequestScope(c), credential, reporter, observation); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) observeTenantProjectPolicy(ctx context.Context, scope hubscope.NativeScope, credential hubapi.APICredential, reporter string, observation policy.Observation) error {
	if s.config.AccessRepository == nil || credential.Runner.RunnerID == "" || credential.Runner.OrganizationID != scope.Organization {
		return hubapi.NativeNotFound()
	}
	previous, previousRevision, err := s.readTenantProjectPolicyRevision(ctx, scope.Organization, scope.Project)
	if err != nil {
		return err
	}
	projectSnapshot, err := s.readTenantProjectRecord(ctx, scope)
	if err != nil {
		return err
	}
	if observation.Workflow == nil && projectSnapshot.Core.Profile == "native" && !reflect.DeepEqual(observation.Descriptor, previous.Policy) {
		markdown, err := s.projectRecordWorkflow(ctx, projectSnapshot)
		if err != nil {
			return err
		}
		if err := validateCloudWorkflowPolicy(markdown, observation.Descriptor); err != nil {
			return err
		}
	}
	content := eventstore.NewContent(s.runDetailStore())
	canonical, revision, found, err := s.prepareTenantPolicyRevision(ctx, scope, observation.Descriptor, content)
	if err != nil {
		return err
	}
	observation.Descriptor.ID = canonical.ID
	observation.Descriptor.Authored = canonical.Authored
	review, err := s.prepareDefaultReviewPolicy(ctx, scope, canonical)
	if err != nil {
		return err
	}
	now := s.config.Clock().UTC()
	record := policystore.Observation{
		OrganizationID: scope.Organization,
		ProjectID:      scope.Project,
		RunnerID:       reporter,
		PolicyID:       observation.ID,
		ObservedAt:     now,
		Source:         observation.Source,
		Revision:       revision,
	}
	preparedLanding, err := s.prepareObservedPolicyLanding(ctx, scope, observation)
	if err != nil {
		return err
	}
	apply := true
	writeObservation := func(writer tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, writer, scope, true); err != nil {
			return err
		}
		identity, err := writer.RunnerIdentityByCredential(ctx, credential.ID)
		if err != nil {
			return err
		}
		if identity.RunnerID != credential.Runner.RunnerID || identity.OrganizationID != scope.Organization || identity.RemovedAt != nil || !identity.ExpiresAt.After(now) {
			return hubapi.NativeNotFound()
		}
		allowed, err := writer.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.Project, now)
		if err != nil {
			return err
		}
		if !allowed {
			return hubapi.NativeNotFound()
		}
		if err := writer.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		project, err := writer.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if observation.Workflow == nil && (project.Core.Profile != projectSnapshot.Core.Profile || project.Settings.WorkflowKey != projectSnapshot.Settings.WorkflowKey || project.Settings.WorkflowSHA256 != projectSnapshot.Settings.WorkflowSHA256 || project.Settings.WorkflowBytes != projectSnapshot.Settings.WorkflowBytes || project.Settings.WorkflowContentBytes != projectSnapshot.Settings.WorkflowContentBytes) {
			return corestore.ErrRevisionConflict
		}
		storedRevision, err := writer.ProjectPolicyRevision(ctx, scope.Project, observation.ID)
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
		if !accepted || !apply {
			return nil
		}
		return s.applyObservedTenantDefaultBranchPolicy(ctx, writer, scope, project, reporter, observation, canonical, previous, previousRevision, record.Revision, review, preparedLanding, now)
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, writeObservation)
	if !errors.Is(err, projectstore.ErrStateInUse) {
		return err
	}
	apply = false
	if err := s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, writeObservation); err != nil {
		return err
	}
	return &hubapi.NativeError{Code: "workflow_conflict", Message: "Repository workflow removes a state used by work items; observation retained without approval", Status: http.StatusConflict}
}

func (s *Service) prepareTenantPolicyRevision(ctx context.Context, scope hubscope.NativeScope, descriptor policy.Descriptor, content *eventstore.Content) (policy.Descriptor, policystore.Revision, bool, error) {
	descriptor, revision, found, err := s.readTenantPolicyRevisionForDescriptor(ctx, scope, descriptor, content)
	if err == nil && !found {
		revision, err = content.PrepareProjectPolicy(ctx, scope.Organization, descriptor)
		revision.ProjectID = scope.Project
	}
	return descriptor, revision, found, err
}

func (s *Service) readTenantPolicyRevisionForDescriptor(ctx context.Context, scope hubscope.NativeScope, descriptor policy.Descriptor, content *eventstore.Content) (policy.Descriptor, policystore.Revision, bool, error) {
	if err := descriptor.Validate(); err != nil {
		return policy.Descriptor{}, policystore.Revision{}, false, err
	}
	return s.lookupTenantPolicyRevision(ctx, scope, descriptor.OriginalAuthored(), content)
}

func (s *Service) lookupTenantPolicyRevision(ctx context.Context, scope hubscope.NativeScope, descriptor policy.Descriptor, content *eventstore.Content) (policy.Descriptor, policystore.Revision, bool, error) {
	var revision policystore.Revision
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		revision, err = reader.ProjectPolicyRevision(ctx, scope.Project, descriptor.ID)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		snapshot := descriptor.Configuration
		if descriptor.Authored != nil && len(descriptor.Authored.Files) == 0 && snapshot != nil && (snapshot.Prompt != "" || snapshot.SharedPrompt != "" || snapshot.AgentsPrompt != "") {
			if err := workflowconfig.ValidateNativePromptInputs(descriptor); err != nil {
				return policy.Descriptor{}, revision, false, err
			}
		}
		return descriptor, revision, false, nil
	}
	if err != nil {
		return policy.Descriptor{}, revision, false, err
	}
	stored, err := content.ProjectPolicy(ctx, revision)
	if err != nil {
		return policy.Descriptor{}, revision, false, err
	}
	if err := stored.Validate(); err != nil {
		return policy.Descriptor{}, revision, false, err
	}
	if revision.GitHubPullRequest == nil || *revision.GitHubPullRequest != stored.Gates.GitHubPullRequest {
		return policy.Descriptor{}, revision, false, corestore.ErrUniqueViolation
	}
	repaired, err := repairPolicyDescriptor(stored, descriptor)
	if err != nil {
		return policy.Descriptor{}, revision, false, err
	}
	if repaired.ID != descriptor.ID {
		return s.lookupTenantPolicyRevision(ctx, scope, repaired, content)
	}
	if !reflect.DeepEqual(stored, descriptor) {
		resolved, err := workflowconfig.ResolveSharedPolicy(stored)
		if err != nil {
			return policy.Descriptor{}, revision, false, err
		}
		if !sameTenantProjectPolicyDescriptorContent(resolved, descriptor) {
			return policy.Descriptor{}, revision, false, corestore.ErrUniqueViolation
		}
	}
	revision.Compatibility = policystore.CompatibilityOf(stored)
	return stored, revision, true, nil
}

func repairPolicyDescriptor(current, incoming policy.Descriptor) (policy.Descriptor, error) {
	if incoming.Authored == nil || len(incoming.Authored.Files) == 0 || current.Authored == nil {
		return incoming, nil
	}
	if current.ID != incoming.ID || len(current.Authored.Files) != 0 {
		return incoming, nil
	}
	if (current.Workflow == nil) != (incoming.Workflow == nil) || current.Workflow != nil && current.Workflow.Source != incoming.Workflow.Source {
		return policy.Descriptor{}, hubapi.PolicyMismatch("The verified files do not identify the approved workflow source")
	}
	if current.Workflow == nil && current.Configuration != nil && current.Configuration.DefinitionDigest != "" && (incoming.Configuration == nil || incoming.Configuration.DefinitionDigest != current.Configuration.DefinitionDigest) {
		return policy.Descriptor{}, hubapi.PolicyMismatch("The verified files do not identify the approved Cloud workflow")
	}
	resolved, err := workflowconfig.ResolveSharedPolicy(incoming)
	if err != nil {
		return policy.Descriptor{}, err
	}
	if err := workflowconfig.ValidateNativePromptInputs(resolved); err != nil {
		return policy.Descriptor{}, err
	}
	if resolved.ID != current.ID {
		return policy.Descriptor{}, workflowconfig.ErrNativeAuthoredMismatch
	}
	if resolved.Workflow != nil && current.Workflow.Revision != "" {
		resolved.Workflow.Revision = current.Workflow.Revision
	}
	return resolved.WithPinnedFiles(), nil
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

func (s *Service) prepareObservedPolicyLanding(ctx context.Context, scope hubscope.NativeScope, observation policy.Observation) (eventstore.PolicyLanding, error) {
	source := observation.Source
	if observation.Workflow == nil || source == nil || !hubapi.ValidCommitID(source.Commit) || !hubapi.ValidCommitID(source.DefaultBranchHead) || source.DefaultBranch == "" || source.Commit != observation.Workflow.Revision {
		return eventstore.PolicyLanding{}, nil
	}
	var landing eventstore.PolicyLanding
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		repository := project.Settings.CheckoutRepository
		if repository == "" {
			repository = project.Settings.Repository
		}
		if repository == "" || !strings.EqualFold(repository, source.Repository) {
			return nil
		}
		landing, err = r.ProjectPolicyLanding(ctx, scope.Project, repository, source.Commit, source.DefaultBranch)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		return eventstore.PolicyLanding{}, err
	}
	if landing.Native != nil {
		frozen, err := eventstore.NewContent(s.runDetailStore()).VersionLanding(ctx, *landing.Native)
		if err != nil {
			return eventstore.PolicyLanding{}, err
		}
		landing.Actor = frozen.Actor.PrincipalID
	}
	return landing, nil
}

func samePolicyLandingSource(left, right eventstore.PolicyLanding) bool {
	if left.Native != nil && right.Native != nil {
		left.Actor, right.Actor = "", ""
	}
	return reflect.DeepEqual(left, right)
}

func (s *Service) applyObservedTenantDefaultBranchPolicy(ctx context.Context, writer tenantstore.Writer, scope hubscope.NativeScope, project projectstore.Record, reporter string, observation policy.Observation, canonical policy.Descriptor, previous policy.Approval, previousRevision, observedRevision policystore.Revision, review preparedDefaultReviewPolicy, preparedLanding eventstore.PolicyLanding, now time.Time) error {
	descriptor := observation.Descriptor
	current, err := writer.ProjectPolicy(ctx, scope.Project)
	if errors.Is(err, corestore.ErrNotFound) {
		current = policystore.Revision{}
	} else if err != nil {
		return err
	}
	if !sameProjectPolicySnapshot(current, previousRevision) {
		return nil
	}
	if current.PolicyID != "" && current.PolicyID != descriptor.ID {
		candidate, err := writer.ProjectPolicyRevision(ctx, scope.Project, descriptor.ID)
		if err == nil && candidate.ApprovedBy != "runner-report" {
			return nil
		}
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
	}
	carryover := descriptor.SameAuthoredInputs(previous.Policy)
	landing := eventstore.PolicyLanding{}
	sourceUnavailable := false
	if descriptor.Workflow != nil {
		repository := project.Settings.CheckoutRepository
		if repository == "" {
			repository = project.Settings.Repository
		}
		source := observation.Source
		sourceUnavailable = source == nil || !hubapi.ValidCommitID(source.Commit) || !hubapi.ValidCommitID(source.DefaultBranchHead) || source.DefaultBranch == "" || source.Commit != descriptor.Workflow.Revision || repository == "" || !strings.EqualFold(repository, source.Repository)
		if !sourceUnavailable {
			landing, err = writer.ProjectPolicyLanding(ctx, scope.Project, repository, source.Commit, source.DefaultBranch)
			if errors.Is(err, corestore.ErrNotFound) {
				landing, err = eventstore.PolicyLanding{}, nil
			}
			if err != nil {
				return err
			}
			if !samePolicyLandingSource(landing, preparedLanding) {
				return nil
			}
			landing.Actor = preparedLanding.Actor
		}
	}
	if current.PolicyID == descriptor.ID {
		if err := reconcileTenantWorkflowPolicy(ctx, writer, project, previous.Policy); err != nil {
			return err
		}
		return applyDefaultReviewPolicy(ctx, writer, review, previous.ApprovedBy, now)
	}
	if !carryover {
		return nil
	}
	actor := "repository-default-branch"
	if previous.ApprovedBy != "" {
		actor = previous.ApprovedBy
	} else if landing.Actor != "" {
		actor = landing.Actor
	}
	if err := reconcileTenantWorkflowPolicy(ctx, writer, project, canonical); err != nil {
		return err
	}
	approved := observedRevision
	approved.ApprovedBy = actor
	approved.ApprovedAt = now
	if err := writer.ApproveProjectPolicy(ctx, approved, current.PolicyID); err != nil {
		return err
	}
	if err := applyDefaultReviewPolicy(ctx, writer, review, actor, now); err != nil {
		return err
	}
	return s.createTenantWorkflowApply(ctx, writer, scope, project, current.PolicyID, previous.Policy, descriptor, actor, reporter, now, landing, sourceUnavailable)
}

func reconcileTenantWorkflowPolicy(ctx context.Context, writer tenantstore.Writer, project projectstore.Record, descriptor policy.Descriptor) error {
	if project.Core.Profile != "native" {
		return nil
	}
	if descriptor.Workflow == nil {
		if project.Core.WorkflowSource != "" {
			return hubapi.PolicyMismatch("Repository-controlled workflow requires a resolved workflow in the policy descriptor; inspect the repository definition with an updated runner")
		}
		return nil
	}
	revision := descriptor.Workflow.Revision
	if revision == "" {
		revision = descriptor.SourceRevision
	}
	states := hubrecords.NativeTriageStates(descriptor.Workflow.States)
	if project.Core.WorkflowSource == descriptor.Workflow.Source && project.Core.WorkflowSourceRevision == revision && reflect.DeepEqual(project.States, states) && project.Settings.WorkflowKey == "" {
		return nil
	}
	project.Core.WorkflowSource = descriptor.Workflow.Source
	project.Core.WorkflowSourceRevision = revision
	project.States = states
	project.Settings.WorkflowKey, project.Settings.WorkflowSHA256, project.Settings.WorkflowBytes, project.Settings.WorkflowContentBytes = "", "", 0, 0
	_, err := writer.UpdateProjectRecord(ctx, project)
	return err
}

func sameProjectPolicySnapshot(current, planned policystore.Revision) bool {
	if current.PolicyID == "" || planned.PolicyID == "" {
		return current.PolicyID == planned.PolicyID
	}
	return hubrecords.SameNativeClaimProjectPolicy(current, planned)
}

func (s *Service) createTenantWorkflowApply(ctx context.Context, writer tenantstore.Writer, scope hubscope.NativeScope, project projectstore.Record, previousID string, previous policy.Descriptor, descriptor policy.Descriptor, actor, runner string, appliedAt time.Time, landing eventstore.PolicyLanding, sourceUnavailable bool) error {
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
	if sourceUnavailable {
		repository, commit = "", ""
	}
	_, err := writer.CreateProjectWorkflowApply(ctx, policystore.WorkflowApply{
		OrganizationID: scope.Organization, ProjectID: scope.Project, PolicyID: descriptor.ID, PreviousPolicyID: previousID,
		Repository: repository, SourceCommit: commit, PreviousDefinitionDigest: previousDigest,
		DefinitionDigest: descriptor.SourceDigest, RunnerID: runner, AppliedBy: actor, AppliedAt: appliedAt,
		ChangeID: landing.ChangeID, PullRequestURL: landing.PullRequestURL,
	})
	return err
}

func (s *Service) revokeProjectPolicy(c echo.Context) error {
	var request struct {
		ExpectedID string `json:"expected_policy_id"`
	}
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	_, err := s.policyScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(hubapi.APICredential)
	if !ok || credential.ID == "" {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	return s.nativeAPIError(c, &hubapi.NativeError{Code: "operation_retired", Message: "Repository configuration does not require execution approval; pause the runner or remove project access to stop work", Status: http.StatusGone, PublicMessage: true})
}

type preparedDefaultReviewPolicy struct {
	current eventstore.ReviewPolicy
	next    eventstore.ReviewPolicy
}

func defaultFollowingReviewPolicy(current tracker.ChangeReviewPolicy, descriptor policy.Descriptor) (tracker.ChangeReviewPolicy, bool) {
	rules := defaultChangeReviewPolicy(descriptor)
	if current.ID != "" && (current.PolicyID == descriptor.ID || !followsRepositoryGate(current)) {
		return rules, false
	}
	return rules, changerequest.ValidatePolicy(rules, descriptor) == nil
}

func (s *Service) prepareDefaultReviewPolicy(ctx context.Context, scope hubscope.NativeScope, descriptor policy.Descriptor) (preparedDefaultReviewPolicy, error) {
	var current eventstore.ReviewPolicy
	var native bool
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		project, err := r.Project(ctx, scope.Project)
		if err != nil {
			return err
		}
		native = project.Profile == "native"
		if !native {
			return nil
		}
		current, err = r.ReviewPolicy(ctx, scope.Project)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil || !native {
		return preparedDefaultReviewPolicy{}, err
	}
	var rules tracker.ChangeReviewPolicy
	content := eventstore.NewContent(s.runDetailStore())
	if current.ReviewPolicyID != "" {
		if current.OrganizationID != scope.Organization || current.ProjectID != scope.Project {
			return preparedDefaultReviewPolicy{}, corestore.ErrTenantMismatch
		}
		rules, err = content.ReviewPolicy(ctx, current)
		if err != nil {
			return preparedDefaultReviewPolicy{}, err
		}
	}
	nextRules, follow := defaultFollowingReviewPolicy(rules, descriptor)
	if !follow {
		return preparedDefaultReviewPolicy{}, nil
	}
	next, err := content.PrepareReviewPolicy(ctx, eventstore.ReviewPolicy{OrganizationID: scope.Organization, ProjectID: scope.Project, PolicyRevisionID: descriptor.ID, Revision: current.Revision + 1}, nextRules)
	if err != nil {
		return preparedDefaultReviewPolicy{}, err
	}
	return preparedDefaultReviewPolicy{current: current, next: next}, nil
}

func applyDefaultReviewPolicy(ctx context.Context, w tenantstore.Writer, prepared preparedDefaultReviewPolicy, actor string, now time.Time) error {
	if prepared.next.ReviewPolicyID == "" {
		return nil
	}
	current, err := w.ReviewPolicy(ctx, prepared.next.ProjectID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	if err == nil && (current.OrganizationID != prepared.next.OrganizationID || current.ProjectID != prepared.next.ProjectID) {
		return corestore.ErrTenantMismatch
	}
	if err == nil && current.ReviewPolicyID == prepared.next.ReviewPolicyID && current.PolicyRevisionID == prepared.next.PolicyRevisionID {
		return nil
	}
	if current.ReviewPolicyID != prepared.current.ReviewPolicyID || current.PolicyRevisionID != prepared.current.PolicyRevisionID || current.Revision != prepared.current.Revision {
		return hubapi.PolicyMismatch("Change review policy changed; read the current policy and retry")
	}
	next := prepared.next
	next.UpdatedBy, next.UpdatedAt = actor, now
	return w.SetReviewPolicy(ctx, next, prepared.current.ReviewPolicyID)
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

type projectPolicyRead struct {
	policy.Approval
	Policy                       *policy.Descriptor      `json:"policy"`
	ObservedPolicies             []policy.ObservedPolicy `json:"observed_policies,omitempty"`
	ObservedPolicyDetailsOmitted bool                    `json:"observed_policy_details_omitted,omitempty"`
	ObservedPoliciesOmitted      int                     `json:"observed_policies_omitted,omitempty"`
}

func (d *database) policyContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, hubrecords.ProjectPolicyCacheContextKey{}, &d.policies)
}

func (s *Service) policyCacheMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.SetRequest(c.Request().WithContext(s.database.policyContext(c.Request().Context())))
		return next(c)
	}
}

func decodeProjectPolicy(ctx context.Context, scope, id, raw string, result policy.Approval) (policy.Approval, error) {
	cache := hubrecords.ProjectPolicyCacheFromContext(ctx)
	resolved, err := cache.Resolve(scope, id, raw)
	if err != nil {
		return result, err
	}
	result.Policy = resolved.Descriptor()
	return result, nil
}

func validateCloudWorkflowPolicy(markdown string, descriptor policy.Descriptor) error {
	if markdown == "" {
		return nil
	}
	workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{WorkflowPath: "Cloud WORKFLOW.md", Workflow: []byte(markdown)})
	if err != nil {
		return hubapi.PolicyMismatch("Saved Cloud workflow is invalid; correct its Markdown definition before approving execution")
	}
	if descriptor.Authored != nil && len(descriptor.Authored.Files) > 0 {
		workflow.Definition.Layout = workflowconfig.ProjectDefinitionCloud
		workflow.Authored = &policy.Authored{Version: descriptor.Authored.Version}
		expected, err := workflowconfig.ResolvePolicy(workflow)
		if err != nil {
			return err
		}
		if descriptor.Authored.Files["WORKFLOW.md"] == expected.Authored.Files["WORKFLOW.md"] {
			return nil
		}
	} else if descriptor.Configuration != nil && descriptor.Configuration.DefinitionDigest == workflow.SourceHash {
		return nil
	}
	return hubapi.PolicyMismatch("Cloud workflow changed; load its current Markdown definition and approve the resolved policy before claiming work")
}

func (d *database) leasePolicyID(ctx context.Context, lease tracker.LeaseID) (string, error) {
	if d.postgres == nil || d.hostedOrganization == "" {
		return "", hubapi.NativeWorkItemStorageUnavailable()
	}
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	var id string
	err := d.postgres.ReadTenant(ctx, d.hostedOrganization, corestore.Serving, func(reader tenantstore.Reader) error {
		record, err := reader.LeasePolicy(ctx, lease)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		id = record.PolicyID
		return nil
	})
	return id, err
}
