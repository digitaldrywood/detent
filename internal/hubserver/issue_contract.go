package hubserver

import (
	"context"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func nativeContractIssue(issue tracker.NativeIssue) connector.Issue {
	return connector.Issue{Title: issue.Title, Description: issue.Body, IssueContract: issue.IssueContract}
}

type nativeContractEvaluation struct {
	Allowed    bool
	ReasonCode string
	Reason     string
	Missing    []string
}

func (s *Service) validateWorkItemRecordFilingTenant(ctx context.Context, scope nativeScope, plan workItemRecordPlan) (policystore.Revision, bool, error) {
	reporterCreate := scope.credential.HostedRole == "reporter" && plan.create
	var revision policystore.Revision
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		revision, err = reader.ProjectPolicy(ctx, scope.project)
		return err
	})
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return revision, false, err
	}
	found := err == nil
	_, machine := issueorigin.Parse(plan.issue.Body)
	exempt := !reporterCreate && (machine || plan.issue.LinkedSource != nil && plan.issue.LinkedSource.Status != "complete" || strings.EqualFold(plan.issue.State, "Triage") || plan.issue.IssueContract != nil && plan.issue.IssueContract.Exempt || strings.Contains(strings.ToLower(plan.issue.Body), "detent:no-dispatch"))
	if exempt && (!plan.create || plan.project.Core.WorkflowSource == "") {
		return revision, found, nil
	}
	descriptor := policy.Descriptor{}
	if found {
		descriptor, err = eventstore.NewContent(s.runDetailStore()).ProjectPolicy(ctx, revision)
		if err != nil {
			return revision, found, err
		}
	}
	if plan.create {
		if err := validateConfiguredWorkflowPolicy(plan.project, descriptor, found); err != nil {
			return revision, found, err
		}
	}
	if exempt {
		return revision, found, nil
	}
	workflow := config.Workflow{Config: config.Default()}
	if descriptor.ID != "" {
		workflow, err = config.ApplyNativePolicy(workflow, descriptor)
		if err != nil {
			return revision, found, policyMismatch(err.Error())
		}
	}
	target := workflow.Config.BacklogAdmission.TargetState
	if target == "" {
		target = "Todo"
	}
	if !reporterCreate && !strings.EqualFold(plan.issue.State, target) {
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
		reporterContract := config.IssueContract{Sections: reporterContracts[plan.issue.Type]}
		missing := reporterSectionsMissing(reporterContract.Evaluate(nativeContractIssue(plan.issue)).Missing, plan.issue.ReportContext)
		if len(missing) > 0 {
			return revision, found, nativePublicInvalid("Add or complete these required Reporter sections: " + strings.Join(missing, ", "))
		}
	}
	if strings.EqualFold(plan.issue.State, "Triage") {
		return revision, found, nil
	}
	if !strings.EqualFold(plan.issue.State, target) {
		return revision, found, nil
	}
	if plan.issue.IssueContract != nil && plan.issue.IssueContract.ReleaseDelivery != nil {
		return revision, found, workItemStorageUnavailable()
	}
	evaluation := contract.Evaluate(nativeContractIssue(plan.issue))
	if len(evaluation.Missing) > 0 {
		return revision, found, nativeWorkflowInvalid(strings.Join([]string{"Add or complete these required issue sections", strings.Join(evaluation.Missing, ", ")}, ": ") + ". File into Backlog/Triage to draft it first.")
	}
	return revision, found, nil
}

func validateConfiguredWorkflowPolicy(project projectstore.Record, descriptor policy.Descriptor, found bool) error {
	return validateNativeWorkflowPolicy(project, descriptor, found)
}

type nativeScheduledRepair struct {
	policyID     string
	evidence     gate.ScheduledEvidence
	importURL    string
	importBody   string
	importReason string
}

func (s *Service) observeImportedScheduledRepair(ctx context.Context, scope nativeScope, snapshot tracker.GitHubIssueSnapshot) *nativeScheduledRepair {
	origin, machine := issueorigin.Parse(snapshot.Body)
	if !machine || origin.Kind != "doctor" || origin.Instance != "github-actions" {
		return nil
	}
	result := &nativeScheduledRepair{importURL: snapshot.URL, importBody: snapshot.Body, importReason: "Scheduled CI import requires an approved repair delegation and verified GitHub Actions provenance"}
	if snapshot.Provenance.Provider != "github" || snapshot.Provenance.AuthorID != "github-actions[bot]" || strings.Contains(strings.ToLower(snapshot.Body), "detent:no-dispatch") || s.config.ImportBackend == nil {
		return result
	}
	approval, err := s.readTenantProjectPolicy(ctx, scope.organization, scope.project)
	found := approval.Policy.ID != ""
	if err != nil || !found || approval.Policy.Authored == nil {
		return result
	}
	contract, err := config.ResolvePolicyIssueContract(approval.Policy)
	if err != nil || contract.ScheduledImportBody() == "" {
		return result
	}
	result.importReason = "Scheduled CI import requires complete sections matching the approved bounded repair contract"
	digests := config.IssueContractSectionDigests(snapshot.Body)
	for section, digest := range config.IssueContractSectionDigests(contract.ScheduledImportBody()) {
		if digests[section] != digest {
			return result
		}
	}
	evidence := gate.ParseScheduledEvidence(snapshot.Body)
	_, sourceRepository, _, err := tracker.ParseGitHubIssueURL(snapshot.URL)
	repository, repoErr := s.scheduledRepository(ctx, scope)
	result.importReason = "Scheduled CI import requires a verified failed job from the configured repository CI workflow"
	if err != nil || repoErr != nil || len(evidence) != 1 || !strings.EqualFold(repository, sourceRepository) || !strings.EqualFold(repository, evidence[0].Repository) || !scheduledRepairOriginMatches(origin.Source, evidence[0]) {
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
	result.policyID, result.evidence, result.importReason = approval.Policy.ID, *page.Scheduled, ""
	return result
}

func (s *Service) observeScheduledRepair(ctx context.Context, scope nativeScope, request tracker.CreateIssue) *nativeScheduledRepair {
	origin, machine := issueorigin.Parse(request.Body)
	if !machine || origin.Kind != "doctor" || origin.Instance != "github-actions" || scope.credential.Scope == apiScopeWorker || request.GitHubIssueURL != "" || request.Provenance != nil || s.config.ImportBackend == nil {
		return nil
	}
	evidence := gate.ParseScheduledEvidence(request.Body)
	if len(evidence) != 1 || !scheduledRepairOriginMatches(origin.Source, evidence[0]) {
		return nil
	}
	approval, err := s.readTenantProjectPolicy(ctx, scope.organization, scope.project)
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
	return &nativeScheduledRepair{policyID: approval.Policy.ID, evidence: *page.Scheduled}
}

func scheduledRepairOriginMatches(source string, evidence gate.ScheduledEvidence) bool {
	runURL := "https://github.com/" + evidence.Repository + "/actions/runs/" + evidence.RunID
	return source == runURL || source == runURL+"/attempts/"+evidence.RunAttempt
}
