package hubserver

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workitemread"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (snapshot *boardProjectionSnapshot) readRelationships(ctx context.Context, reader tenantstore.Reader, candidate viewstore.BoardCandidate) error {
	ids := []tracker.NativeWorkItemID{candidate.WorkItemID}
	if reasons, ok := reader.(corestore.DependencyReasonReader); ok {
		var err error
		snapshot.dependencyReasons, err = reasons.DependencyReasonRecords(ctx, candidate.ProjectID, ids)
		if err != nil {
			return err
		}
	}
	batch, ok := reader.(tenantstore.WorkItemRelationshipBatchReader)
	if !ok {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	var err error
	snapshot.relationships, err = batch.WorkItemRelationshipFacts(ctx, candidate.ProjectID, ids)
	return err
}

func (s *Service) boardProjectedRelationshipIssues(organization tracker.OrganizationID, project tracker.ProjectID, issue tracker.NativeIssue, snapshot boardProjectionSnapshot) (tracker.NativeIssue, viewstore.BoardReporterRelations) {
	for _, record := range snapshot.dependencyReasons {
		if issue.WorkItemID == record.WorkItemID {
			workitemread.ApplyDependencyReason(&issue, record)
		}
	}
	broad := issue
	s.projectBoardRelationships(hubscope.NativeScope{Organization: organization, Project: project}, &broad, snapshot.relationships)
	reporter := issue
	scope := hubscope.NativeScope{Organization: organization, Project: project, Credential: hubapi.APICredential{HostedRole: "reporter", ID: issue.Actor.PrincipalID}, SourceActor: &issue.Actor}
	s.projectBoardRelationships(scope, &reporter, snapshot.relationships)
	result := viewstore.BoardReporterRelations{ReporterStatus: hubscope.ReporterStatus(issue.State), Dependencies: reporter.Dependencies, Blockers: reporter.Blockers, Dependents: reporter.Dependents, Related: reporter.Related, DependencyReady: reporter.DependencyReady, BlockedReason: reporter.BlockedReason, UnblocksReason: reporter.UnblocksReason}
	return broad, result
}

func (s *Service) projectBoardRelationships(scope hubscope.NativeScope, issue *tracker.NativeIssue, facts []tenantstore.WorkItemRelationshipFact) {
	issue.Dependencies = []tracker.NativeWorkItemID{}
	issue.Blockers = []tracker.NativeDependency{}
	issue.Dependents = []tracker.NativeDependency{}
	issue.Related = []tracker.NativeDependency{}
	issue.DependencyReady = true
	for _, fact := range facts {
		relation := fact.Relationship
		visible := relation.RelatedProjectID == scope.Project || fact.RelatedProjectVisible
		visible = visible && scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(relation.ActorPrincipalID)})
		if !visible {
			continue
		}
		link := tracker.NativeDependency{
			Dispatchable: relation.Dispatchable,
			Number:       relation.Number,
			Title:        relation.Title,
			Identifier:   string(relation.RelatedProjectID) + "#" + strconv.Itoa(relation.Number),
			ID:           relation.RelatedWorkItemID,
			ProjectID:    relation.RelatedProjectID,
			State:        relation.State,
			Terminal:     relation.Terminal,
			ClosedAt:     relation.ClosedAt,
			StartedAt:    relation.StartedAt,
		}
		if relation.OrganizationSlug != "" && relation.ProjectSlug != "" && s.config.Hosted != nil {
			link.WebURL = s.nativeIssueResponse(tracker.NativeIssue{WebURL: hubrecords.IssueBrowserPath(relation.OrganizationSlug, relation.ProjectSlug, relation.Number)}).WebURL
		}
		switch relation.Type {
		case "dependency":
			switch relation.Direction {
			case "blocked_by":
				issue.Dependencies = append(issue.Dependencies, relation.RelatedWorkItemID)
				issue.Blockers = append(issue.Blockers, link)
				issue.DependencyReady = issue.DependencyReady && relation.Terminal
			case "blocks":
				issue.Dependents = append(issue.Dependents, link)
			}
		case "related":
			issue.Related = append(issue.Related, link)
		}
	}
	slices.Sort(issue.Dependencies)
	slices.SortFunc(issue.Blockers, func(a, b tracker.NativeDependency) int { return strings.Compare(string(a.ID), string(b.ID)) })
	slices.SortFunc(issue.Dependents, func(a, b tracker.NativeDependency) int { return strings.Compare(string(a.ID), string(b.ID)) })
	slices.SortFunc(issue.Related, func(a, b tracker.NativeDependency) int { return strings.Compare(string(a.ID), string(b.ID)) })
	blocked, unblocks := make([]string, 0, len(issue.Dependencies)), make([]string, 0, len(issue.Dependents))
	for _, id := range issue.Dependencies {
		blocked = append(blocked, string(id))
	}
	for _, link := range issue.Dependents {
		unblocks = append(unblocks, string(link.ID))
	}
	issue.BlockedReason.CurrentBasis, issue.UnblocksReason.CurrentBasis = workitemread.DependencyBasis(blocked), workitemread.DependencyBasis(unblocks)
}
