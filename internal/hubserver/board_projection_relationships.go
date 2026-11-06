package hubserver

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) boardProjectedRelationshipIssues(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, issue tracker.NativeIssue) (tracker.NativeIssue, viewstore.BoardReporterRelations, error) {
	var facts []tenantstore.WorkItemRelationshipFact
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		if err := loadDependencyReasonRecords(ctx, reader, project, []*tracker.NativeIssue{&issue}); err != nil {
			return err
		}
		batch, ok := reader.(tenantstore.WorkItemRelationshipBatchReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		facts, err = batch.WorkItemRelationshipFacts(ctx, project, []tracker.NativeWorkItemID{issue.WorkItemID})
		return err
	})
	if err != nil {
		return issue, viewstore.BoardReporterRelations{}, err
	}
	broad := issue
	s.projectBoardRelationships(nativeScope{organization: organization, project: project}, &broad, facts)
	reporter := issue
	scope := nativeScope{organization: organization, project: project, credential: apiCredential{HostedRole: "reporter", ID: issue.Actor.PrincipalID}, sourceActor: &issue.Actor}
	s.projectBoardRelationships(scope, &reporter, facts)
	result := viewstore.BoardReporterRelations{ReporterStatus: reporterStatus(issue.State), Dependencies: reporter.Dependencies, Blockers: reporter.Blockers, Dependents: reporter.Dependents, Related: reporter.Related, DependencyReady: reporter.DependencyReady, BlockedReason: reporter.BlockedReason, UnblocksReason: reporter.UnblocksReason}
	return broad, result, nil
}

func (s *Service) projectBoardRelationships(scope nativeScope, issue *tracker.NativeIssue, facts []tenantstore.WorkItemRelationshipFact) {
	issue.Dependencies = []tracker.NativeWorkItemID{}
	issue.Blockers = []tracker.NativeDependency{}
	issue.Dependents = []tracker.NativeDependency{}
	issue.Related = []tracker.NativeDependency{}
	issue.DependencyReady = true
	type dependencyEdges struct {
		blocked, unblocks               []string
		privateBlocked, privateUnblocks bool
	}
	edges := dependencyEdges{}
	for _, fact := range facts {
		relation := fact.Relationship
		visible := relation.RelatedProjectID == scope.project || fact.RelatedProjectVisible
		if relation.Type == "dependency" {
			switch relation.Direction {
			case "blocked_by":
				edges.blocked = append(edges.blocked, string(relation.RelatedWorkItemID))
				edges.privateBlocked = edges.privateBlocked || relation.RelatedProjectID != scope.project
			case "blocks":
				edges.unblocks = append(edges.unblocks, string(relation.RelatedWorkItemID))
				edges.privateUnblocks = edges.privateUnblocks || relation.RelatedProjectID != scope.project
			}
		}
		visible = visible && scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(relation.ActorPrincipalID)})
		if !visible {
			if relation.Type == "dependency" {
				switch relation.Direction {
				case "blocked_by":
					edges.privateBlocked = true
				case "blocks":
					edges.privateUnblocks = true
				}
			}
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
			link.WebURL = s.nativeIssueResponse(tracker.NativeIssue{WebURL: issueBrowserPath(relation.OrganizationSlug, relation.ProjectSlug, relation.Number)}).WebURL
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
	issue.BlockedReason.CurrentBasis = dependencyBasis(edges.blocked)
	issue.UnblocksReason.CurrentBasis = dependencyBasis(edges.unblocks)
	if issue.BlockedReason.Basis != issue.BlockedReason.CurrentBasis || edges.privateBlocked {
		issue.BlockedReason.Text = ""
	}
	if issue.UnblocksReason.Basis != issue.UnblocksReason.CurrentBasis || edges.privateUnblocks {
		issue.UnblocksReason.Text = ""
	}
	if edges.privateBlocked {
		issue.BlockedReason.CurrentBasis = ""
	}
	if edges.privateUnblocks {
		issue.UnblocksReason.CurrentBasis = ""
	}
}
