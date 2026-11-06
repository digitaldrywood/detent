package hubserver

import (
	"context"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/securityaudit"
)

func (s *Service) checkReleasePublicationRules(ctx context.Context, scope nativeScope, plan releaseDeliveryCheck, approved ChangeVersionApproval) error {
	historicalSource := false
	if plan.publishing {
		issue := plan.issue
		if issue.IssueContract != nil && issue.IssueContract.HumanAction != "" || strings.EqualFold(issue.State, "Blocked") || !plan.facts.DependencyReady || strings.Contains(strings.ToLower(issue.Body), "detent:no-dispatch") {
			return nativePublicInvalid("Held delivery cannot publish a release PR")
		}
		for _, state := range plan.project.States {
			if state.Name == issue.State && state.OperatorOnly {
				return nativePublicInvalid("Operator-only delivery cannot publish a release PR")
			}
		}
		attempt := plan.attempt.attempt
		if attempt.ID != "" && attempt.Status == "succeeded" && attempt.WorkItemRevision >= plan.current.Revision && attempt.DispatchGeneration >= plan.current.DispatchGeneration {
			run, err := s.hydrateNativeAttemptTenant(ctx, plan.attempt, s.config.now())
			if err != nil {
				return err
			}
			if answeredClaimKind(run.NativeRunData, run.Checkpoint) != "" {
				return nativePublicInvalid("Delivery retains an unanswered attempt hold")
			}
		}
		content := eventstore.NewContent(s.runDetailStore())
		sourceVersion, err := content.Version(ctx, plan.sourceVersion)
		if err != nil {
			return err
		}
		prNumber := 0
		if sourceVersion.External != nil {
			prNumber, err = strconv.Atoi(sourceVersion.External.ID)
			if err != nil || prNumber < 1 {
				return nativePublicInvalid("Source Change requires an exact PR identity")
			}
		}
		audited := false
		for _, row := range plan.reviews {
			raw, err := content.Evidence(ctx, row)
			if err != nil {
				return err
			}
			review, err := eventstore.DecodeChangeReview(row, raw)
			if err != nil {
				return err
			}
			if review.VersionID != sourceVersion.ID {
				continue
			}
			if review.Decision == "changes_requested" {
				break
			}
			audit := review.SecurityAudit
			if audit == nil || audit.Landing {
				continue
			}
			key := securityaudit.Key{ProjectID: string(scope.project), Repository: sourceVersion.Repository, PRNumber: prNumber, BaseSHA: sourceVersion.BaseSHA, HeadSHA: sourceVersion.HeadSHA}
			evaluation := securityaudit.Evaluate(audit.Run, nil, key, "", []string{"p1", "p2"})
			if plan.found && evaluation.Reason == securityaudit.ReasonReviewerMismatch {
				evaluation = securityaudit.EvaluateLandedSource(audit.Run, key)
				historicalSource = evaluation.Allowed
			}
			audited = review.VersionID == sourceVersion.ID && review.Decision == "approved" && audit.Verdict == "pass" && audit.Run.Verdict == securityaudit.VerdictPass && audit.SessionID == audit.Run.ID && audit.SessionID > 0 && audit.Run.IssueID == string(plan.request.SourceIssueID) && evaluation.Allowed
			break
		}
		if !audited {
			return nativePublicInvalid("Source Change requires a passing immutable security audit")
		}
	}
	rules := approved.ReviewPolicy
	policy := approved.Policy
	independentReview := rules.RequireReview || policy.Gates.Validator || policy.Gates.SecurityAudit || policy.Gates.AutomatedReview == gate.AutomatedReviewRequired
	if historicalSource && (!policy.Gates.SecurityAudit || !rules.RequireReview && !policy.Gates.Validator && policy.Gates.AutomatedReview != gate.AutomatedReviewRequired) {
		return nativePublicInvalid("Historical source recovery requires current delivery security audit and independent scope review")
	}
	if !independentReview {
		return nativePublicInvalid("Release delivery requires the project's independent review policy")
	}
	return nil
}
