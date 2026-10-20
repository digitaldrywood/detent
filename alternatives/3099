package tracker

import (
	"strings"

	"github.com/digitaldrywood/detent/internal/workpad"
)

func RecordedNativeBlockers(issue NativeIssue, attempts []NativeAttempt, history []CollaborationEvent) (NativeAttempt, string, bool) {
	if issue.Profile != "native" || issue.Archived || !strings.EqualFold(issue.State, "Blocked") {
		return NativeAttempt{}, "", false
	}
	var latest NativeAttempt
	for _, attempt := range attempts {
		if attempt.FencingToken > latest.FencingToken {
			latest = attempt
		}
	}
	disposition := latest.Disposition
	if latest.Status != "succeeded" || latest.Identity == nil || latest.WorkItemRevision <= 0 || disposition == nil || disposition.Status != workpad.StatusBlocked {
		return NativeAttempt{}, "", false
	}
	reportRevision := latest.WorkItemRevision
	for _, event := range history {
		if event.AggregateID == issue.WorkItemID && event.ProjectID == issue.ProjectID && event.OrganizationID == issue.OrganizationID && event.Type == "run.finished" && event.Actor.Kind == "runner" && event.Data.Run != nil && event.Data.Run.AttemptID == latest.AttemptID && event.Data.Run.FencingToken == latest.FencingToken && event.Data.Revision > reportRevision {
			reportRevision = event.Data.Revision
		}
	}
	if reportRevision > issue.Revision {
		return NativeAttempt{}, "", false
	}
	var lane CollaborationEvent
	for _, event := range history {
		if event.AggregateID != issue.WorkItemID || event.ProjectID != issue.ProjectID || event.OrganizationID != issue.OrganizationID {
			continue
		}
		if event.Data.Revision > reportRevision && event.Data.Revision <= issue.Revision && (event.Type != "workflow.transitioned" || event.Actor.Kind != "runner" || event.Data.Reason != "worker_progress") {
			return NativeAttempt{}, "", false
		}
		if event.Type == "workflow.transitioned" && event.AggregateSequence > lane.AggregateSequence {
			lane = event
		}
	}
	if lane.Data.Revision != issue.Revision || lane.Data.ToState != issue.State || lane.Actor.Kind != "runner" || lane.Data.Reason != "worker_progress" || lane.RecordedAt.Before(latest.StartedAt) {
		return NativeAttempt{}, "", false
	}
	return latest, lane.Data.FromState, true
}
