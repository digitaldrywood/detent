package hubserver

import (
	"context"
	"fmt"
	"html"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func scheduledRepairTodoAvailable(project projectstore.Record) bool {
	return slices.ContainsFunc(project.States, func(state tracker.NativeState) bool {
		return state.Name == "Todo" && state.Dispatchable && !state.Terminal && !state.OperatorOnly
	})
}

func scheduledRepairProjectTrusted(project projectstore.Record, verified *hubscope.NativeScheduledRepair) bool {
	repository := project.Settings.CheckoutRepository
	if repository == "" {
		repository = project.Settings.Repository
	}
	return strings.EqualFold(repository, verified.Evidence.Repository) && scheduledRepairTodoAvailable(project)
}

func (s *Service) requireGitHubPolicyRecord(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope) error {
	verified := scope.ScheduledRepair
	if !verified.Trusted() {
		return nil
	}
	if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
		return err
	}
	row, err := w.ProjectPolicy(ctx, scope.Project)
	if err != nil {
		return err
	}
	if row.PolicyID == verified.PolicyID {
		project, err := w.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if scheduledRepairProjectTrusted(project, verified) {
			return nil
		}
	}
	verified.DowngradeTrust()
	return corestore.ErrSerializationFailure
}

func (s *Service) applyScheduledRepairRecord(ctx context.Context, scope hubscope.NativeScope, issue *tracker.NativeIssue) error {
	verified := scope.ScheduledRepair
	if !verified.Trusted() {
		return nil
	}
	approval, err := s.readTenantProjectPolicy(ctx, scope.Organization, scope.Project)
	if err != nil {
		return err
	}
	if approval.Policy.ID != verified.PolicyID || approval.Policy.Authored == nil {
		verified.DowngradeTrust()
		return nil
	}
	contract, err := config.ResolvePolicyIssueContract(approval.Policy)
	if err != nil {
		return err
	}
	_, _, priority, authorized := contract.ScheduledRepairWorkflow()
	if !authorized {
		verified.DowngradeTrust()
		return nil
	}
	body := contract.ScheduledRepairBody()
	if verified.ImportURL != "" {
		project, err := s.readProjectRecord(ctx, scope)
		if err != nil {
			return err
		}
		body = contract.ScheduledImportBody()
		if body == "" || !scheduledRepairProjectTrusted(project, verified) {
			verified.DowngradeTrust()
			return nil
		}
		if issue.Body != verified.ImportBody {
			return nil
		}
	}
	origin, ok := issueorigin.Parse(issue.Body)
	if !ok || !hubrecords.ScheduledRepairOriginMatches(origin.Source, verified.Evidence) {
		return nil
	}
	if verified.ImportURL == "" {
		project, err := s.readProjectRecord(ctx, scope)
		if err != nil {
			return err
		}
		if body == "" || !scheduledRepairProjectTrusted(project, verified) {
			verified.DowngradeTrust()
			return nil
		}
	}
	issue.State, issue.Priority = "Todo", new(priority)
	issue.IssueContract.ConfirmedSections = config.IssueContractSectionDigests(body)
	issue.IssueContract.RecordedAt = s.config.Clock()
	body += fmt.Sprintf("\n\n## Verified CI occurrence\nRun: %s\nJob: %s\nTested commit: %s\nApproved policy: %s", verified.Evidence.RunURL, verified.Evidence.JobURL, verified.Evidence.HeadSHA, verified.PolicyID)
	body += "\n\n## Diagnostic evidence (untrusted)\n\n> " + strings.ReplaceAll(html.EscapeString(strings.ReplaceAll(issue.Body, "\r", "")), "\n", "\n> ")
	issue.Body = issueorigin.Stamp(body, origin) + verified.Evidence.Stamp()
	return nil
}
