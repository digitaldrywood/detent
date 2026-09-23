package orchestrator

import (
	"context"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/provenance"
	"github.com/digitaldrywood/detent/internal/store"
)

func (o *Orchestrator) hydrateOperatorRework(ctx context.Context, issue connector.Issue, target string) (connector.Issue, error) {
	if normalizeState(target) != normalizeState(normalizeAutoPromoteConfig(o.cfg.AutoPromote).ReworkState) ||
		normalizeState(issue.State) == normalizeState(target) {
		return issue, nil
	}
	if hydrator, ok := o.connector.(connector.PullRequestHydrator); ok {
		hydrated, err := hydrator.HydratePullRequest(ctx, issue)
		hydrated.State = issue.State
		return hydrated, err
	}
	return issue, nil
}

// operatorRejectedHead consumes the existing durable lane history: moving a
// reviewed PR to Rework rejects that head even when GitHub cannot accept a
// changes-requested review from the PR's author.
func (o *Orchestrator) operatorRejectedHead(ctx context.Context, issue connector.Issue) bool {
	if issue.PullRequest == nil {
		return false
	}
	cfg := normalizeAutoPromoteConfig(o.cfg.AutoPromote)
	// A deliberate subsequent operator move to review or Merging remains
	// authoritative; rejection only prevents automatic promotion out of work.
	if normalizeState(issue.State) == normalizeState(cfg.SourceState) || mergeWorkerIssue(issue) {
		return false
	}
	timeline, ok := o.issueWorkflowTimeline(ctx, issue)
	if !ok {
		return false
	}
	for _, event := range timeline.Events {
		if event.PhaseType != store.WorkflowPhaseTypeLane || event.Status != "entered" ||
			normalizeState(event.PhaseName) != normalizeState(cfg.ReworkState) {
			continue
		}
		metadata, ok := workflowLaneMetadataFromJSON(event.MetadataJSON)
		if !ok || metadata.PullRequest == nil {
			continue
		}
		if event.Reason != "operator_move" && metadata.Provenance.Origin != provenance.OriginHuman {
			continue
		}
		pr := metadata.PullRequest
		if pr.Number != int64(issue.PullRequest.Number) || pr.Repository != pullRequestRepository(issue) {
			continue
		}
		if head := strings.TrimSpace(pr.HeadSHA); head != "" && head == strings.TrimSpace(issue.PullRequest.HeadSHA) {
			return true
		}
	}
	return false
}
