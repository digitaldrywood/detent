package hubserver

import (
	"context"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeContractEvaluation struct {
	Allowed    bool
	ReasonCode string
	Reason     string
	Missing    []string
}

func (s *Service) validateWorkItemRecordFilingTenant(ctx context.Context, scope hubscope.NativeScope, plan hubrecords.WorkItemRecordPlan) (policystore.Revision, bool, error) {
	reporterCreate := scope.Credential.HostedRole == "reporter" && plan.Create
	var revision policystore.Revision
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		revision, err = reader.ProjectPolicy(ctx, scope.Project)
		return err
	})
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return revision, false, err
	}
	found := err == nil
	_, machine := issueorigin.Parse(plan.Issue.Body)
	exempt := !reporterCreate && (machine || plan.Issue.LinkedSource != nil && plan.Issue.LinkedSource.Status != "complete" || strings.EqualFold(plan.Issue.State, "Triage") || plan.Issue.IssueContract != nil && plan.Issue.IssueContract.Exempt || strings.Contains(strings.ToLower(plan.Issue.Body), "detent:no-dispatch"))
	if exempt && (!plan.Create || plan.Project.Core.WorkflowSource == "") {
		return revision, found, nil
	}
	descriptor := policy.Descriptor{}
	if found {
		descriptor, err = eventstore.NewContent(s.runDetailStore()).ProjectPolicy(ctx, revision)
		if err != nil {
			return revision, found, err
		}
	}
	if plan.Create {
		if err := validateConfiguredWorkflowPolicy(plan.Project, descriptor, found); err != nil {
			return revision, found, err
		}
	}
	if exempt {
		return revision, found, nil
	}
	workflow := config.Workflow{Config: config.Default()}
	if descriptor.ID != "" {
		workflow, err = config.ReadNativePolicy(descriptor)
		if err != nil {
			return revision, found, hubapi.PolicyMismatch(err.Error())
		}
	}
	target := workflow.Config.BacklogAdmission.TargetState
	if target == "" {
		target = "Todo"
	}
	if !reporterCreate && !strings.EqualFold(plan.Issue.State, target) {
		return revision, found, nil
	}
	contract, err := config.ResolvePolicyIssueContract(descriptor)
	if err != nil {
		return revision, found, err
	}
	if reporterCreate {
		reporterContracts, err := config.ResolveReporterContracts(contract.Context)
		if err != nil {
			return revision, found, err
		}
		reporterContract := config.IssueContract{Sections: reporterContracts[plan.Issue.Type]}
		missing := reporterSectionsMissing(reporterContract.Evaluate(hubrecords.NativeContractIssue(plan.Issue)).Missing, plan.Issue.ReportContext)
		if len(missing) > 0 {
			return revision, found, hubapi.NativePublicInvalid("Add or complete these required Reporter sections: " + strings.Join(missing, ", "))
		}
	}
	if strings.EqualFold(plan.Issue.State, "Triage") {
		return revision, found, nil
	}
	if !strings.EqualFold(plan.Issue.State, target) {
		return revision, found, nil
	}
	if plan.Issue.IssueContract != nil && plan.Issue.IssueContract.ReleaseDelivery != nil {
		return revision, found, hubapi.NativeWorkItemStorageUnavailable()
	}
	evaluation := contract.Evaluate(hubrecords.NativeContractIssue(plan.Issue))
	if len(evaluation.Missing) > 0 {
		return revision, found, hubapi.NativeWorkflowInvalid(strings.Join([]string{"Add or complete these required issue sections", strings.Join(evaluation.Missing, ", ")}, ": ") + ". File into Backlog/Triage to draft it first.")
	}
	return revision, found, nil
}

func validateConfiguredWorkflowPolicy(project projectstore.Record, descriptor policy.Descriptor, found bool) error {
	return validateNativeWorkflowPolicy(project, descriptor, found)
}

func (s *Service) observeImportedScheduledRepair(ctx context.Context, scope hubscope.NativeScope, snapshot tracker.GitHubIssueSnapshot) *hubscope.NativeScheduledRepair {
	origin, machine := issueorigin.Parse(snapshot.Body)
	if !machine || origin.Kind != "doctor" || origin.Instance != "github-actions" {
		return nil
	}
	result := &hubscope.NativeScheduledRepair{ImportURL: snapshot.URL, ImportBody: snapshot.Body, ImportReason: "Scheduled CI import requires an approved repair delegation and verified GitHub Actions provenance"}
	if snapshot.Provenance.Provider != "github" || snapshot.Provenance.AuthorID != "github-actions[bot]" || strings.Contains(strings.ToLower(snapshot.Body), "detent:no-dispatch") || s.config.ImportBackend == nil {
		return result
	}
	approval, err := s.readTenantProjectPolicy(ctx, scope.Organization, scope.Project)
	found := approval.Policy.ID != ""
	if err != nil || !found || approval.Policy.Authored == nil {
		return result
	}
	contract, err := config.ResolvePolicyIssueContract(approval.Policy)
	if err != nil || contract.ScheduledImportBody() == "" {
		return result
	}
	result.ImportReason = "Scheduled CI import requires complete sections matching the approved bounded repair contract"
	digests := config.IssueContractSectionDigests(snapshot.Body)
	for section, digest := range config.IssueContractSectionDigests(contract.ScheduledImportBody()) {
		if digests[section] != digest {
			return result
		}
	}
	evidence := gate.ParseScheduledEvidence(snapshot.Body)
	_, sourceRepository, _, err := tracker.ParseGitHubIssueURL(snapshot.URL)
	repository, repoErr := s.scheduledRepository(ctx, scope)
	result.ImportReason = "Scheduled CI import requires a verified failed job from the configured repository CI workflow"
	if err != nil || repoErr != nil || len(evidence) != 1 || !strings.EqualFold(repository, sourceRepository) || !strings.EqualFold(repository, evidence[0].Repository) || !hubrecords.ScheduledRepairOriginMatches(origin.Source, evidence[0]) {
		return result
	}
	workflow, branch, _, authorized := contract.ScheduledImportWorkflow()
	if !authorized {
		return result
	}
	page, err := s.config.ImportBackend.FetchImportPage(ctx, GitHubImportRequest{Profile: "native", Repository: repository, Stage: "scheduled", Scheduled: &evidence[0], ScheduledWorkflow: workflow, ScheduledBranch: branch})
	if err != nil || page.Scheduled == nil {
		return result
	}
	result.PolicyID, result.Evidence, result.ImportReason = approval.Policy.ID, *page.Scheduled, ""
	return result
}

func (s *Service) observeScheduledRepair(ctx context.Context, scope hubscope.NativeScope, request tracker.CreateIssue) *hubscope.NativeScheduledRepair {
	origin, machine := issueorigin.Parse(request.Body)
	if !machine || origin.Kind != "doctor" || origin.Instance != "github-actions" || scope.Credential.Scope == hubapi.APIScopeWorker || request.GitHubIssueURL != "" || request.Provenance != nil || s.config.ImportBackend == nil {
		return nil
	}
	evidence := gate.ParseScheduledEvidence(request.Body)
	if len(evidence) != 1 || !hubrecords.ScheduledRepairOriginMatches(origin.Source, evidence[0]) {
		return nil
	}
	approval, err := s.readTenantProjectPolicy(ctx, scope.Organization, scope.Project)
	found := approval.Policy.ID != ""
	if err != nil || !found || approval.Policy.Authored == nil {
		return nil
	}
	contract, err := config.ResolvePolicyIssueContract(approval.Policy)
	if err != nil || contract.ScheduledRepairBody() == "" {
		return nil
	}
	repository, err := s.scheduledRepository(ctx, scope)
	if err != nil || !strings.EqualFold(repository, evidence[0].Repository) {
		return nil
	}
	workflow, branch, _, authorized := contract.ScheduledRepairWorkflow()
	if !authorized {
		return nil
	}
	page, err := s.config.ImportBackend.FetchImportPage(ctx, GitHubImportRequest{Profile: "native", Repository: repository, Stage: "scheduled", Scheduled: &evidence[0], ScheduledWorkflow: workflow, ScheduledBranch: branch})
	if err != nil || page.Scheduled == nil {
		return nil
	}
	return &hubscope.NativeScheduledRepair{PolicyID: approval.Policy.ID, Evidence: *page.Scheduled}
}
