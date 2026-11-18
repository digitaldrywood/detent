package boardview

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func ExactDisplay(row viewstore.BoardRecord, source tracker.NativeBoardCard) bool {
	projected := ProjectedCard(row)
	for _, card := range []*tracker.NativeBoardCard{&source, &projected} {
		issue := &card.Issue
		issue.WebURL = ""
		issue.Dependencies, issue.Blockers, issue.Dependents, issue.Related = nil, nil, nil, nil
		issue.DependencyReady = false
		issue.BlockedReason, issue.UnblocksReason = tracker.DependencyReason{}, tracker.DependencyReason{}
	}
	times := []time.Time{source.Issue.CreatedAt, source.Issue.UpdatedAt, source.Issue.LastActivityAt}
	if p := source.Issue.Provenance; p != nil {
		times = append(times, p.CreatedAt, p.UpdatedAt, p.ObservedAt)
	}
	if source.Issue.ClosedAt != nil {
		times = append(times, *source.Issue.ClosedAt)
	}
	if source.Attempt != nil {
		times = append(times, source.Attempt.StartedAt, source.Attempt.ExpiresAt)
	}
	if source.Execution != nil {
		times = append(times, source.Execution.ExpiresAt)
		if source.Execution.StartedAt != nil {
			times = append(times, *source.Execution.StartedAt)
		}
	}
	for _, at := range times {
		if at.Format(time.RFC3339Nano) != at.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano) {
			return false
		}
	}
	want, err := json.Marshal(source)
	if err != nil {
		return false
	}
	got, err := json.Marshal(projected)
	return err == nil && bytes.Equal(want, got)
}

func ProjectedCard(row viewstore.BoardRecord) tracker.NativeBoardCard {
	d := row.Display
	actor := d.Actor
	actor.PrincipalID = row.ActorPrincipalID
	issue := tracker.NativeIssue{
		NativeReference: tracker.NativeReference{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, WorkItemID: row.WorkItemID, Number: d.Number, Revision: tracker.Revision(row.SourceRevision), Profile: "native"},
		Title:           d.Title, Type: row.IssueType, State: row.Position.State, Archived: row.Archived, Terminal: row.Terminal, Priority: row.Priority, Labels: row.Labels, Assignees: row.Assignees, Actor: actor,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, ClosedAt: row.ClosedAt, LatestTransitionReason: d.Reason,
		Provenance: d.Provenance,
	}
	if len(issue.Labels) == 0 {
		issue.Labels = nil
	}
	if len(issue.Assignees) == 0 {
		issue.Assignees = nil
	}
	if row.Position.Activity != nil {
		issue.LastActivityAt = *row.Position.Activity
	}
	if row.VerificationState != "" {
		issue.Verification = &tracker.NativeVerification{State: row.VerificationState}
	}
	return tracker.NativeBoardCard{Issue: issue, StatusLine: d.StatusLine, Attempt: d.Attempt, Execution: d.Execution, Change: d.Change}
}
