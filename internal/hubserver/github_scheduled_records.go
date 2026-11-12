package hubserver

import (
	"context"
	"fmt"
	"html"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const scheduledImportTrustChanged = "Scheduled CI import authorization changed before creation or Todo is not dispatchable"

func (v *nativeScheduledRepair) trusted() bool {
	return v != nil && v.importReason == "" && v.policyID != ""
}

func (v *nativeScheduledRepair) trustedLinkedImport() bool {
	return v.trusted() && v.importURL != ""
}

func (v *nativeScheduledRepair) downgradeTrust() {
	v.policyID = ""
	if v.importURL != "" {
		v.importReason = scheduledImportTrustChanged
	}
}

func scheduledRepairTodoAvailable(project projectstore.Record) bool {
	return slices.ContainsFunc(project.States, func(state tracker.NativeState) bool {
		return state.Name == "Todo" && state.Dispatchable && !state.Terminal && !state.OperatorOnly
	})
}

func scheduledRepairProjectTrusted(project projectstore.Record, verified *nativeScheduledRepair) bool {
	repository := project.Settings.CheckoutRepository
	if repository == "" {
		repository = project.Settings.Repository
	}
	return strings.EqualFold(repository, verified.evidence.Repository) && scheduledRepairTodoAvailable(project)
}

func (s *Service) requireGitHubPolicyRecord(ctx context.Context, w tenantstore.Writer, scope nativeScope) error {
	verified := scope.scheduledRepair
	if !verified.trusted() {
		return nil
	}
	if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
		return err
	}
	row, err := w.ProjectPolicy(ctx, scope.project)
	if err != nil {
		return err
	}
	if row.PolicyID == verified.policyID {
		project, err := w.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if scheduledRepairProjectTrusted(project, verified) {
			return nil
		}
	}
	verified.downgradeTrust()
	return corestore.ErrSerializationFailure
}

func (s *Service) applyScheduledRepairRecord(ctx context.Context, scope nativeScope, issue *tracker.NativeIssue) error {
	verified := scope.scheduledRepair
	if !verified.trusted() {
		return nil
	}
	approval, err := s.readTenantProjectPolicy(ctx, scope.organization, scope.project)
	if err != nil {
		return err
	}
	if approval.Policy.ID != verified.policyID || approval.Policy.Authored == nil {
		verified.downgradeTrust()
		return nil
	}
	contract, err := config.ResolvePolicyIssueContract(approval.Policy)
	if err != nil {
		return err
	}
	_, _, priority, authorized := contract.ScheduledRepairWorkflow()
	if !authorized {
		verified.downgradeTrust()
		return nil
	}
	body := contract.ScheduledRepairBody()
	if verified.importURL != "" {
		project, err := s.readProjectRecord(ctx, scope)
		if err != nil {
			return err
		}
		body = contract.ScheduledImportBody()
		if body == "" || !scheduledRepairProjectTrusted(project, verified) {
			verified.downgradeTrust()
			return nil
		}
		if issue.Body != verified.importBody {
			return nil
		}
	}
	origin, ok := issueorigin.Parse(issue.Body)
	if !ok || !scheduledRepairOriginMatches(origin.Source, verified.evidence) {
		return nil
	}
	if verified.importURL == "" {
		project, err := s.readProjectRecord(ctx, scope)
		if err != nil {
			return err
		}
		if body == "" || !scheduledRepairProjectTrusted(project, verified) {
			verified.downgradeTrust()
			return nil
		}
	}
	issue.State, issue.Priority = "Todo", new(priority)
	issue.IssueContract.ConfirmedSections = config.IssueContractSectionDigests(body)
	issue.IssueContract.RecordedAt = s.config.now()
	body += fmt.Sprintf("\n\n## Verified CI occurrence\nRun: %s\nJob: %s\nTested commit: %s\nApproved policy: %s", verified.evidence.RunURL, verified.evidence.JobURL, verified.evidence.HeadSHA, verified.policyID)
	body += "\n\n## Diagnostic evidence (untrusted)\n\n> " + strings.ReplaceAll(html.EscapeString(strings.ReplaceAll(issue.Body, "\r", "")), "\n", "\n> ")
	issue.Body = issueorigin.Stamp(body, origin) + verified.evidence.Stamp()
	return nil
}
