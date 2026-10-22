package orchestrator

import (
	"context"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

const spendProgressReason = "spend_since_progress_circuit_breaker"

func (o *Orchestrator) spendProgressDeliverableKind(running Running) string {
	if o != nil {
		switch strings.ToLower(strings.TrimSpace(o.cfg.DeliverableKind)) {
		case workflowconfig.DeliverableArtifact:
			return workflowconfig.DeliverableArtifact
		case workflowconfig.DeliverablePullRequest:
			return workflowconfig.DeliverablePullRequest
		}
	}
	return spendProgressRunningDeliverableKind(running)
}

func spendProgressRunningDeliverableKind(running Running) string {
	values := []string{running.DeliverableKind}
	if running.Issue.Deliverable != nil {
		values = append(values, running.Issue.Deliverable.Kind)
	}
	for _, value := range values {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case workflowconfig.DeliverableArtifact, "artifacts", "file", "files":
			return workflowconfig.DeliverableArtifact
		case workflowconfig.DeliverablePullRequest:
			return workflowconfig.DeliverablePullRequest
		}
	}
	return workflowconfig.DeliverablePullRequest
}

func (o *Orchestrator) refreshSpendProgressIssue(ctx context.Context, issue connector.Issue) (connector.Issue, string) {
	if o.spendProgressDeliverableKind(Running{Issue: issue}) == workflowconfig.DeliverableArtifact {
		refreshed, current := o.refreshImplementCompletionIssue(ctx, issue)
		if !current {
			return refreshed, "artifact progress evidence refresh unavailable"
		}
		return refreshed, ""
	}
	refreshed := cloneIssue(issue)
	if !implementProgressLinkedPullRequest(refreshed) {
		if o == nil || o.connector == nil || strings.TrimSpace(refreshed.ID) == "" {
			return refreshed, "pull request evidence refresh unavailable"
		}
		refresher, ok := o.connector.(connector.PullRequestReferenceRefresher)
		if ok {
			linked, err := refresher.RefreshPullRequestReference(ctx, refreshed)
			if err != nil {
				return refreshed, "pull request reference refresh failed: " + err.Error()
			}
			refreshed = mergeIssueTrackerFields(refreshed, linked)
			if strings.TrimSpace(linked.PRRepository) != "" {
				refreshed.PRRepository = strings.TrimSpace(linked.PRRepository)
			}
		} else {
			issues, err := o.connector.FetchIssueStatesByIDs(ctx, []string{refreshed.ID})
			if err != nil {
				return refreshed, "pull request evidence refresh failed: " + err.Error()
			}
			for _, candidate := range issues {
				if strings.TrimSpace(candidate.ID) == strings.TrimSpace(refreshed.ID) {
					refreshed = mergeIssueTrackerFields(refreshed, candidate)
					if strings.TrimSpace(candidate.PRRepository) != "" {
						refreshed.PRRepository = strings.TrimSpace(candidate.PRRepository)
					}
					break
				}
			}
		}
	}
	if !implementProgressLinkedPullRequest(refreshed) {
		return refreshed, ""
	}
	hydrator, ok := o.connector.(connector.PullRequestHydrator)
	if !ok {
		return refreshed, "pull request hydrator unavailable"
	}
	hydrated, err := hydrator.HydratePullRequest(ctx, refreshed)
	if err != nil {
		return refreshed, "pull request hydration failed: " + err.Error()
	}
	if reason := implementProgressHydrationUnavailableReason(hydrated.PullRequest); reason != "" {
		return hydrated, reason
	}
	return hydrated, ""
}

func mergeWorkAttemptMetadata(groups ...map[string]any) map[string]any {
	var merged map[string]any
	for _, group := range groups {
		for key, value := range group {
			if merged == nil {
				merged = map[string]any{}
			}
			merged[key] = value
		}
	}
	return merged
}

func priorAttemptPresent(prior runpkg.PriorAttempt) bool {
	return strings.TrimSpace(prior.Source) != "" || strings.TrimSpace(prior.Reason) != "" || prior.ExplainBeforeRetry || prior.Validator.Submitted
}
