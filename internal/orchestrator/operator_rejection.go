package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/provenance"
	"github.com/digitaldrywood/detent/internal/store"
)

func (o *Orchestrator) hydrateOperatorRework(ctx context.Context, issue connector.Issue, target string) connector.Issue {
	if normalizeState(target) != normalizeState(normalizeAutoPromoteConfig(o.cfg.AutoPromote).ReworkState) || normalizeState(issue.State) == normalizeState(target) {
		return issue
	}
	if hydrator, ok := o.connector.(connector.PullRequestHydrator); ok {
		hydrated, err := hydrator.HydratePullRequest(ctx, issue)
		if err == nil {
			hydrated.State = issue.State
			return hydrated
		}
		// The lane move is authoritative even when the forge is unavailable. Record
		// an unknown head instead of treating a cached head as freshly rejected.
		if o.logger != nil {
			o.logger.Warn("hydrate operator rejection head", "issue_id", issue.ID, "error", err)
		}
		issue = cloneIssue(issue)
		if issue.PullRequest != nil {
			issue.PullRequest.HeadSHA = ""
		}
	}
	return issue
}

// operatorRejectedHead consumes the existing durable lane history. An unknown
// rejection head requires a commit newer than the move, rather than interpreting
// missing evidence as permission to promote.
func (o *Orchestrator) operatorRejectedHead(ctx context.Context, issue connector.Issue) (bool, error) {
	if issue.PullRequest == nil || normalizePullRequestState(issue.PullRequest.State) != "open" {
		return false, nil
	}
	cfg := normalizeAutoPromoteConfig(o.cfg.AutoPromote)
	// Deliberate later operator moves to review or Merging remain authoritative.
	if normalizeState(issue.State) == normalizeState(cfg.SourceState) || mergeWorkerIssue(issue) {
		return false, nil
	}
	reader, ok := o.workflowMetrics.(WorkflowMetricsTimelineReader)
	if !ok {
		return false, nil
	}
	identity := store.IssueIdentity{ProjectID: o.workflowMetricsProjectID(), IssueID: issue.ID, Identifier: issue.Identifier, IssueURL: issue.URL}
	timeline, err := reader.IssueWorkflowTimeline(ctx, identity)
	if err != nil {
		return false, err
	}
	var recordedAt time.Time
	for _, event := range timeline.Events {
		if event.PhaseType != store.WorkflowPhaseTypeLane || event.Status != "entered" || normalizeState(event.PhaseName) != normalizeState(cfg.ReworkState) {
			continue
		}
		metadata, _ := workflowLaneMetadataFromJSON(event.MetadataJSON)
		if event.Reason != "operator_move" && metadata.Provenance.Origin != provenance.OriginHuman {
			continue
		}
		at := workflowLaneTransitionAt(event)
		if at.After(recordedAt) {
			recordedAt = at
		}
		pr := metadata.PullRequest
		if pr != nil {
			if pr.Number > 0 && pr.Number != int64(issue.PullRequest.Number) || pr.Repository != "" && pr.Repository != pullRequestRepository(issue) {
				continue
			}
			if head := strings.TrimSpace(pr.HeadSHA); head != "" {
				if head == strings.TrimSpace(issue.PullRequest.HeadSHA) {
					return true, nil
				}
				continue
			}
		}
		if !pullRequestHeadAfter(issue, at) {
			return true, nil
		}
	}
	// Lane observations are persisted independently of best-effort history events.
	// A missing event must not erase a recorded human move, including after restart.
	ledger := o.laneLedger
	if ledger == nil {
		if metricsLedger, ok := o.workflowMetrics.(store.LaneLedgerStore); ok {
			ledger = metricsLedger
		}
	}
	observation := o.laneObservations[issue.ID]
	if ledger != nil {
		observation, err = ledger.LaneObservation(ctx, identity)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
	}
	if observation.Origin == string(provenance.OriginHuman) && normalizeState(observation.State) == normalizeState(cfg.ReworkState) && observation.EnteredAt.After(recordedAt) && !pullRequestHeadAfter(issue, observation.EnteredAt) {
		return false, fmt.Errorf("operator Rework history is unavailable for %s", issue.ID)
	}
	return false, nil
}

func pullRequestHeadAfter(issue connector.Issue, at time.Time) bool {
	return !at.IsZero() && strings.TrimSpace(issue.PullRequest.HeadSHA) != "" && issue.PullRequest.HeadCommittedAt != nil && issue.PullRequest.HeadCommittedAt.After(at)
}
