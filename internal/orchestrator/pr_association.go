package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/provenance"
	"github.com/digitaldrywood/detent/internal/store"
)

const staleTodoPRRecovered = "stale_todo_pr_recovered"
const associationUnavailable = "association_unavailable"

func (o *Orchestrator) revalidateTickPullRequestAssociations(ctx context.Context, fetched tickFetchedIssues) tickFetchedIssues {
	validator, ok := o.connector.(connector.PullRequestAssociationRevalidator)
	if !ok {
		return fetched
	}
	validated := map[string]connector.Issue{}
	for _, issue := range mergeIssueSlices(fetched.candidates, fetched.status) {
		if issue.PullRequest == nil && issue.PRNumber == nil {
			continue
		}
		fresh, err := validator.RevalidatePullRequestAssociation(ctx, cloneIssue(issue))
		if err != nil {
			fresh = cloneIssue(issue)
			if fresh.PullRequest == nil {
				fresh.PullRequest = &connector.PullRequest{}
			}
			fresh.PullRequest.HydrationUnavailableReason = associationUnavailable
			fresh.PullRequest.State = ""
			fresh.PRVerifiedAt = time.Time{}
			if o.logger != nil {
				o.logger.Warn("pull request association revalidation failed", "identifier", issue.Identifier, "error", err)
			}
		}
		if normalizeState(fresh.State) == "todo" && fresh.PullRequest != nil && !pullRequestHydrationBlocksProgress(fresh.PullRequest) {
			if event, ok := o.latestAssociationLaneEntry(ctx, fresh); ok && event.Reason == staleTodoPRRecovered && o.issueHasNoWorkAttempts(ctx, fresh) {
				metadata, parsed := workflowLaneMetadataFromJSON(event.MetadataJSON)
				if parsed && metadata.Reconciliation == staleTodoPRRecovered && metadata.PullRequest != nil &&
					int(metadata.PullRequest.Number) == fresh.PullRequest.Number && strings.EqualFold(metadata.PullRequest.Repository, fresh.PRRepository) {
					fresh.PRNumber, fresh.PullRequest, fresh.PRRepository = nil, nil, ""
					fresh.PRSource = ""
				}
			}
		}
		validated[issueIdentityKey(issue)] = fresh
	}
	for _, issues := range [][]connector.Issue{fetched.candidates, fetched.status} {
		for i, issue := range issues {
			if fresh, ok := validated[issueIdentityKey(issue)]; ok {
				issues[i].PRNumber, issues[i].PRRepository, issues[i].PullRequest = fresh.PRNumber, fresh.PRRepository, fresh.PullRequest
				issues[i].PRSource, issues[i].PRVerifiedAt = fresh.PRSource, fresh.PRVerifiedAt
				issues[i].StageUpdatedAt, issues[i].StageUpdatedActor = fresh.StageUpdatedAt, fresh.StageUpdatedActor
			}
		}
	}
	return fetched
}

func (o *Orchestrator) recoverStaleTodoReviews(ctx context.Context, state *State, issues []connector.Issue, now time.Time) map[string]struct{} {
	validator, ok := o.connector.(connector.PullRequestAssociationRevalidator)
	if !ok {
		return nil
	}
	transitioned := map[string]struct{}{}
	for _, issue := range issuesInStates(issues, []string{normalizeAutoPromoteConfig(o.cfg.AutoPromote).SourceState}) {
		if issue.PullRequest != nil || issue.PRNumber != nil || staleTodoPullRequestAlreadyActive(state, issue.ID) {
			continue
		}
		event, found := o.latestAssociationLaneEntry(ctx, issue)
		if !found || !staleTodoReviewEntry(issue, event) || !o.issueHasNoWorkAttempts(ctx, issue) {
			continue
		}
		metadata, _ := workflowLaneMetadataFromJSON(event.MetadataJSON)
		recovery := workflowLaneMetadata{Reconciliation: staleTodoPRRecovered, PullRequest: metadata.PullRequest}
		if recovery.PullRequest == nil {
			recovery.PullRequest = &workflowLanePullRequestMetadata{Number: *event.PRNumber}
		}
		if recovery.PullRequest.Repository == "" {
			recovery.PullRequest.Repository = workAttemptRepository(issue)
		}
		candidate := cloneIssue(issue)
		candidate.PRNumber = new(int(recovery.PullRequest.Number))
		candidate.PRRepository = recovery.PullRequest.Repository
		fresh, err := validator.RevalidatePullRequestAssociation(ctx, candidate)
		if err != nil || fresh.PullRequest != nil || fresh.PRNumber != nil || !staleTodoReviewEntry(fresh, event) {
			continue
		}
		if err := o.updateIssueStateByIDWithMetadata(ctx, state, issue.ID, fresh, "Todo", now, staleTodoPRRecovered, recovery, laneMutationRevokeWorker); err != nil {
			continue
		}
		transitioned[issue.ID] = struct{}{}
		o.clearAutoPromotedIssueDispatchMemory(state, issue.ID)
		if o.logger != nil {
			o.logger.Info(staleTodoPRRecovered, "identifier", issue.Identifier, "previous_pull_request_number", recovery.PullRequest.Number)
		}
	}
	return transitioned
}

func (o *Orchestrator) latestAssociationLaneEntry(ctx context.Context, issue connector.Issue) (store.WorkflowPhaseEvent, bool) {
	reader, ok := o.workflowMetrics.(WorkflowMetricsTimelineReader)
	if !ok {
		return store.WorkflowPhaseEvent{}, false
	}
	timeline, err := reader.IssueWorkflowTimeline(ctx, store.IssueIdentity{ProjectID: o.workflowMetricsProjectID(), IssueID: issue.ID, Identifier: issue.Identifier, IssueURL: issue.URL})
	if err != nil {
		return store.WorkflowPhaseEvent{}, false
	}
	var latest store.WorkflowPhaseEvent
	for _, event := range timeline.Events {
		if event.PhaseType == store.WorkflowPhaseTypeLane && event.Status == "entered" &&
			(event.StartedAt.After(latest.StartedAt) || event.StartedAt.Equal(latest.StartedAt) && event.ID > latest.ID) {
			latest = event
		}
	}
	return latest, !latest.StartedAt.IsZero()
}

func staleTodoReviewEntry(issue connector.Issue, event store.WorkflowPhaseEvent) bool {
	if normalizeState(event.PhaseName) != normalizeState(issue.State) || normalizeState(event.PreviousPhaseName) != "todo" ||
		event.PRNumber == nil || *event.PRNumber <= 0 || issue.StageUpdatedAt == nil ||
		!issue.StageUpdatedAt.UTC().Truncate(time.Second).Equal(workflowLaneTransitionAt(event).Truncate(time.Second)) {
		return false
	}
	metadata, ok := workflowLaneMetadataFromJSON(event.MetadataJSON)
	if !ok || metadata.Provenance.Origin != provenance.OriginDetent || metadata.Provenance.Initiator != provenance.InitiatorDetentInstance {
		return false
	}
	return metadata.Reconciliation == "stale_todo_pr" || metadata.Reconciliation == "" && event.Reason == string(AutoPromoteReasonCINotGreen)
}

func (o *Orchestrator) issueHasNoWorkAttempts(ctx context.Context, issue connector.Issue) bool {
	if o.workAttempts == nil {
		return false
	}
	attempts, err := o.workAttempts.ListRecentTerminalWorkAttempts(ctx, store.WorkAttemptHistoryQuery{ProjectID: o.workflowMetricsProjectID(), IssueID: issue.ID, Identifier: issue.Identifier, IssueURL: issue.URL, Limit: 1})
	if err != nil || len(attempts) != 0 {
		return false
	}
	active, err := o.workAttempts.ListActiveWorkAttempts(ctx, store.WorkAttemptQuery{ProjectID: o.workflowMetricsProjectID()})
	if err != nil {
		return false
	}
	for _, attempt := range active {
		if attempt.IssueID == issue.ID || attempt.Identifier != "" && strings.EqualFold(attempt.Identifier, issue.Identifier) || attempt.IssueURL != "" && attempt.IssueURL == issue.URL {
			return false
		}
	}
	return true
}
