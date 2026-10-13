package hubserver

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func nativeIssueContract(ctx context.Context, q nativeQueryer, scope nativeScope) (config.IssueContract, error) {
	approval, err := readProjectPolicy(ctx, q, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		var nativeErr *nativeError
		if !errors.As(err, &nativeErr) || nativeErr.Code != "policy_mismatch" {
			return config.IssueContract{}, err
		}
	}
	return config.ResolvePolicyIssueContract(approval.Policy)
}

func nativeReporterContracts(ctx context.Context, q nativeQueryer, scope nativeScope) (config.ReporterContracts, error) {
	contract, err := nativeIssueContract(ctx, q, scope)
	if err != nil {
		return nil, err
	}
	return config.ResolveReporterContracts(contract.Context)
}

func nativeContractIssue(issue tracker.NativeIssue) connector.Issue {
	return connector.Issue{Title: issue.Title, Description: issue.Body, IssueContract: issue.IssueContract}
}

func nativeIssueContractConfig(ctx context.Context, q nativeQueryer, scope nativeScope) (config.Config, error) {
	approval, err := readProjectPolicy(ctx, q, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		return config.Config{}, err
	}
	workflow, err := config.ApplyNativePolicy(config.Workflow{Config: config.Default()}, approval.Policy)
	if err != nil {
		return config.Config{}, policyMismatch(err.Error())
	}
	return workflow.Config, nil
}

func nativeIssueContractClaimable(ctx context.Context, q nativeQueryer, scope nativeScope, id tracker.WorkItemID) (bool, string, error) {
	var native string
	if err := q.QueryRowContext(ctx, "SELECT native_id, project_id FROM issues WHERE id = ? AND organization_id = ?", id, scope.organization).Scan(&native, &scope.project); err != nil {
		return false, "", err
	}
	issue, _, err := readNativeIssue(ctx, q, scope, native)
	if err != nil {
		return false, "", err
	}
	if strings.Contains(strings.ToLower(issue.Body), "detent:no-dispatch") {
		return false, "The issue contains detent:no-dispatch; remove the marker to permit dispatch", nil
	}
	if issue.LinkedSource != nil && issue.LinkedSource.Status != "complete" {
		return true, "", nil
	}
	if strings.EqualFold(issue.State, "Triage") || issue.IssueContract.Exempt {
		return true, "", nil
	}
	cfg, err := nativeIssueContractConfig(ctx, q, scope)
	if err != nil {
		return false, "", err
	}
	target := cfg.BacklogAdmission.TargetState
	if target == "" {
		target = "Todo"
	}
	if !strings.EqualFold(issue.State, target) {
		return true, "", nil
	}
	contract, err := nativeIssueContract(ctx, q, scope)
	if err != nil {
		return false, "", err
	}
	evaluation := contract.Evaluate(nativeContractIssue(issue))
	var reasons []string
	if len(evaluation.Missing) > 0 {
		reasons = append(reasons, "Add or complete these required issue sections: "+strings.Join(evaluation.Missing, ", "))
	}
	if evaluation.NeedsConfirmation {
		reasons = append(reasons, "A human must confirm the success criteria by editing or saving the issue body")
	}
	return evaluation.Satisfied(), strings.Join(reasons, "; "), nil
}

func requireNativeIssueContractAtFiling(ctx context.Context, q nativeQueryer, scope nativeScope, issue tracker.NativeIssue) error {
	if strings.EqualFold(issue.State, "Triage") {
		return nil
	}
	cfg, err := nativeIssueContractConfig(ctx, q, scope)
	if err != nil {
		return nil
	}
	target := cfg.BacklogAdmission.TargetState
	if target == "" {
		target = "Todo"
	}
	if !strings.EqualFold(issue.State, target) || strings.Contains(strings.ToLower(issue.Body), "detent:no-dispatch") {
		return nil
	}
	contract, err := nativeIssueContract(ctx, q, scope)
	if err != nil {
		return fmt.Errorf("resolve issue contract: %w", err)
	}
	if missing := contract.Evaluate(nativeContractIssue(issue)).Missing; len(missing) > 0 {
		return nativeWorkflowInvalid(fmt.Sprintf("Add these required issue sections before filing into %s: %s. File into Backlog to draft it first.", issue.State, strings.Join(missing, ", ")))
	}
	return nil
}

type nativeScheduledRepair struct {
	policyID string
	evidence gate.ScheduledEvidence
}

func (s *Service) observeScheduledRepair(ctx context.Context, scope nativeScope, request tracker.CreateIssue) *nativeScheduledRepair {
	origin, machine := issueorigin.Parse(request.Body)
	if string(scope.project) != issueorigin.DetentCloudProjectID || !machine || origin.Kind != "doctor" || origin.Instance != "github-actions" || scope.credential.Scope == apiScopeWorker || request.GitHubIssueURL != "" || request.Provenance != nil || s.config.ImportBackend == nil {
		return nil
	}
	evidence := gate.ParseScheduledEvidence(request.Body)
	if len(evidence) != 1 || evidence[0].RunURL != origin.Source {
		return nil
	}
	approval, err := readProjectPolicy(ctx, s.database.db, string(scope.organization)+"/"+string(scope.project))
	if err != nil || approval.Policy.Authored == nil {
		return nil
	}
	contract, err := config.ResolvePolicyIssueContract(approval.Policy)
	if err != nil || contract.ScheduledRepairBody() == "" {
		return nil
	}
	repository, err := policyRepository(ctx, s.database.db, string(scope.organization)+"/"+string(scope.project))
	if err != nil || !strings.EqualFold(repository, "digitaldrywood/detent") || !strings.EqualFold(repository, evidence[0].Repository) {
		return nil
	}
	page, err := s.config.ImportBackend.FetchImportPage(ctx, GitHubImportRequest{Profile: "native", Repository: evidence[0].Repository, Stage: "scheduled", Scheduled: &evidence[0]})
	if err != nil || page.Scheduled == nil {
		return nil
	}
	return &nativeScheduledRepair{policyID: approval.Policy.ID, evidence: *page.Scheduled}
}

func applyScheduledRepairContract(ctx context.Context, q nativeQueryer, scope nativeScope, issue *tracker.NativeIssue, now time.Time) {
	verified := scope.scheduledRepair
	if verified == nil {
		return
	}
	approval, err := readProjectPolicy(ctx, q, string(scope.organization)+"/"+string(scope.project))
	if err != nil || approval.Policy.ID != verified.policyID || approval.Policy.Authored == nil {
		return
	}
	repository, err := policyRepository(ctx, q, string(scope.organization)+"/"+string(scope.project))
	if err != nil || !strings.EqualFold(repository, "digitaldrywood/detent") || !strings.EqualFold(repository, verified.evidence.Repository) {
		return
	}
	contract, err := config.ResolvePolicyIssueContract(approval.Policy)
	if err != nil {
		return
	}
	body := contract.ScheduledRepairBody()
	if body == "" {
		return
	}
	origin, ok := issueorigin.Parse(issue.Body)
	if !ok || origin.Kind != "doctor" || origin.Instance != "github-actions" || origin.Source != verified.evidence.RunURL {
		return
	}
	issue.IssueContract.ConfirmedSections = config.IssueContractSectionDigests(body)
	issue.IssueContract.RecordedAt = now
	body += fmt.Sprintf("\n\n## Verified CI occurrence\nRun: %s\nJob: %s\nTested commit: %s\nApproved policy: %s", verified.evidence.RunURL, verified.evidence.JobURL, verified.evidence.HeadSHA, verified.policyID)
	body += "\n\n## Diagnostic evidence (untrusted)\n\n> " + strings.ReplaceAll(html.EscapeString(strings.ReplaceAll(issue.Body, "\r", "")), "\n", "\n> ")
	issue.Body = issueorigin.Stamp(body, origin) + verified.evidence.Stamp()
}
