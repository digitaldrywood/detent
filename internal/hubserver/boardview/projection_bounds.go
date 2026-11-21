package boardview

import (
	"encoding/json"
	"slices"

	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func BoundProjection(projected tracker.NativeIssue, reporter viewstore.BoardReporterRelations) (board tracker.NativeIssue, list tracker.NativeIssue, boundedReporter viewstore.BoardReporterRelations, listBytes int, err error) {
	list, err = operatortool.BoundNativeListIssueBytes(projected, operatortool.WorkListPageBytes-1024)
	if err != nil {
		return
	}
	board = list
	board.Provenance = projected.Provenance
	board.Change = projected.Change
	board.OmittedFields = slices.DeleteFunc(slices.Clone(board.OmittedFields), func(field string) bool { return field == "provenance" })
	list = operatortool.NativeListIssue(board)
	boundedReporter = reporter
	for _, field := range list.OmittedFields {
		switch field {
		case "dependencies":
			boundedReporter.Dependencies = list.Dependencies
		case "blockers":
			boundedReporter.Blockers = list.Blockers
		case "dependents":
			boundedReporter.Dependents = list.Dependents
		case "related":
			boundedReporter.Related = list.Related
		case "dependency_ready":
			boundedReporter.DependencyReady = list.DependencyReady
		case "blocked_reason":
			boundedReporter.BlockedReason = list.BlockedReason
		case "unblocks_reason":
			boundedReporter.UnblocksReason = list.UnblocksReason
		}
	}
	reporterIssue := list
	reporterIssue.ReporterStatus = boundedReporter.ReporterStatus
	reporterIssue.Dependencies, reporterIssue.Blockers, reporterIssue.Dependents, reporterIssue.Related = boundedReporter.Dependencies, boundedReporter.Blockers, boundedReporter.Dependents, boundedReporter.Related
	reporterIssue.DependencyReady, reporterIssue.BlockedReason, reporterIssue.UnblocksReason = boundedReporter.DependencyReady, boundedReporter.BlockedReason, boundedReporter.UnblocksReason
	for _, issue := range []tracker.NativeIssue{list, reporterIssue} {
		issue = IssueCollections(issue)
		var raw []byte
		raw, err = json.Marshal(operatortool.NativeItemPage(string(projected.ProjectID), tracker.Page[tracker.NativeIssue]{Items: []tracker.NativeIssue{issue}}))
		if err != nil {
			return
		}
		listBytes = max(listBytes, len(raw))
	}
	return
}

func ModelCard(scope hubscope.NativeScope, row viewstore.BoardRecord) (tracker.NativeBoardCard, error) {
	if row.PageIssue == nil {
		return tracker.NativeBoardCard{}, viewstore.ErrInvalidView
	}
	issue := *row.PageIssue
	if scope.Credential.HostedRole == "reporter" {
		facts := row.ReporterRelations
		if facts == nil {
			return tracker.NativeBoardCard{}, viewstore.ErrInvalidView
		}
		issue.ReporterStatus = facts.ReporterStatus
		issue.Dependencies, issue.Blockers, issue.Dependents, issue.Related = facts.Dependencies, facts.Blockers, facts.Dependents, facts.Related
		issue.DependencyReady, issue.BlockedReason, issue.UnblocksReason = facts.DependencyReady, facts.BlockedReason, facts.UnblocksReason
	}
	issue = IssueCollections(issue)
	card := tracker.NativeBoardCard{Issue: issue, Attempt: row.Display.Attempt, Execution: row.Display.Execution, Change: row.Display.Change}
	card.StatusLine = viewstore.StatusLine(card)
	if !scope.ReporterOwns(tracker.Actor{PrincipalID: row.Display.ChangePrincipalID}) {
		card.Change = nil
		card.Issue.Change, card.Issue.ListChange, card.Issue.PullRequest = nil, nil, nil
	}
	return card, nil
}

func IssueCollections(issue tracker.NativeIssue) tracker.NativeIssue {
	for _, values := range []*[]string{&issue.Labels, &issue.Assignees} {
		if *values == nil {
			*values = []string{}
		}
	}
	if issue.Dependencies == nil {
		issue.Dependencies = []tracker.NativeWorkItemID{}
	}
	for _, values := range []*[]tracker.NativeDependency{&issue.Blockers, &issue.Dependents, &issue.Related} {
		if *values == nil {
			*values = []tracker.NativeDependency{}
		}
	}
	if issue.ExternalReferences == nil {
		issue.ExternalReferences = []tracker.ExternalReference{}
	}
	return issue
}
