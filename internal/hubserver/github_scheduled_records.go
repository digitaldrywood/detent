package hubserver

import (
	"context"
	"fmt"
	"html"
	"strings"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) requireGitHubPolicyRecord(ctx context.Context, w tenantstore.Writer, scope nativeScope) error {
	if scope.scheduledRepair == nil || scope.scheduledRepair.policyID == "" {
		return nil
	}
	row, err := w.ProjectPolicy(ctx, scope.project)
	if err != nil {
		return err
	}
	if row.PolicyID != scope.scheduledRepair.policyID {
		return nativeConflict(0)
	}
	return nil
}

func (s *Service) applyScheduledRepairRecord(ctx context.Context, scope nativeScope, issue *tracker.NativeIssue) error {
	verified := scope.scheduledRepair
	if verified == nil || verified.importReason != "" || verified.policyID == "" {
		return nil
	}
	approval, err := s.readTenantProjectPolicy(ctx, scope.organization, scope.project)
	if err != nil {
		return err
	}
	if approval.Policy.ID != verified.policyID || approval.Policy.Authored == nil {
		return nativeConflict(0)
	}
	contract, err := config.ResolvePolicyIssueContract(approval.Policy)
	if err != nil {
		return err
	}
	_, _, priority, authorized := contract.ScheduledRepairWorkflow()
	if !authorized {
		return nativeConflict(0)
	}
	body := contract.ScheduledRepairBody()
	if verified.importURL != "" {
		body = contract.ScheduledImportBody()
		if issue.Body != verified.importBody {
			return nil
		}
	}
	origin, ok := issueorigin.Parse(issue.Body)
	if !ok || !scheduledRepairOriginMatches(origin.Source, verified.evidence) {
		return nil
	}
	project, err := s.readProjectRecord(ctx, scope)
	if err != nil {
		return err
	}
	eligible := false
	for _, state := range project.States {
		eligible = eligible || state.Name == "Todo" && state.Dispatchable && !state.Terminal && !state.OperatorOnly
	}
	if !eligible {
		return nativePublicInvalid("Scheduled repair requires dispatchable Todo")
	}
	issue.State, issue.Priority = "Todo", new(priority)
	issue.IssueContract.ConfirmedSections = config.IssueContractSectionDigests(body)
	issue.IssueContract.RecordedAt = s.config.now()
	body += fmt.Sprintf("\n\n## Verified CI occurrence\nRun: %s\nJob: %s\nTested commit: %s\nApproved policy: %s", verified.evidence.RunURL, verified.evidence.JobURL, verified.evidence.HeadSHA, verified.policyID)
	body += "\n\n## Diagnostic evidence (untrusted)\n\n> " + strings.ReplaceAll(html.EscapeString(strings.ReplaceAll(issue.Body, "\r", "")), "\n", "\n> ")
	issue.Body = issueorigin.Stamp(body, origin) + verified.evidence.Stamp()
	return nil
}
