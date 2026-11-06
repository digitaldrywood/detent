package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func projectedBoardCard(row viewstore.BoardRecord) tracker.NativeBoardCard {
	d := row.Display
	actor := d.Actor
	actor.PrincipalID = row.ActorPrincipalID
	issue := tracker.NativeIssue{
		NativeReference: tracker.NativeReference{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, WorkItemID: row.WorkItemID, Number: d.Number, Revision: tracker.Revision(row.SourceRevision), Profile: "native"},
		Title:           d.Title, Type: row.IssueType, State: row.Position.State, Archived: row.Archived, Terminal: row.Terminal, Priority: row.Priority, Labels: row.Labels, Assignees: row.Assignees, Actor: actor,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, ClosedAt: row.ClosedAt, LatestTransitionReason: d.Reason, ChangeSequence: row.SourceEventSequence,
		OmittedFields: []string{"body", "issue_contract", "report_context", "linked_source", "provenance", "external_references", "release_delivery"},
	}
	if row.Position.Activity != nil {
		issue.LastActivityAt = *row.Position.Activity
	}
	if row.VerificationState != "" {
		issue.Verification = &tracker.NativeVerification{State: row.VerificationState}
	}
	return tracker.NativeBoardCard{Issue: issue, StatusLine: d.StatusLine, Attempt: d.Attempt, Execution: d.Execution, Change: d.Change}
}

func (s *Service) prepareProjectedBoardPage(ctx context.Context, scope nativeScope, records []viewstore.BoardRecord, prepared map[tracker.NativeWorkItemID]boardPreparedRecord) *boardRecordPreparation {
	cards := make(map[tracker.NativeWorkItemID]tracker.NativeBoardCard, len(records))
	issues := make([]*tracker.NativeIssue, 0, len(records))
	for _, row := range records {
		if prior, ok := prepared[row.WorkItemID]; ok {
			cards[row.WorkItemID] = prior.card
			continue
		}
		card := projectedBoardCard(row)
		if !scope.reporterOwns(tracker.Actor{PrincipalID: row.Display.ChangePrincipalID}) {
			card.Change = nil
		}
		cards[row.WorkItemID] = card
		issues = append(issues, &card.Issue)
	}
	finish := timeSpan(ctx, "board_relationships")
	err := s.hydrateWorkItemRelationshipFactsPage(ctx, scope, issues)
	finish()
	for _, issue := range issues {
		card := cards[issue.WorkItemID]
		card.Issue = *issue
		cards[issue.WorkItemID] = card
	}
	for _, row := range records {
		prepared[row.WorkItemID] = boardPreparedRecord{record: row, card: cards[row.WorkItemID], err: err}
	}
	return prepareBoardRecordPage(ctx, records, func(_ context.Context, row viewstore.BoardRecord) (tracker.NativeBoardCard, error) {
		value := prepared[row.WorkItemID]
		return value.card, value.err
	})
}
