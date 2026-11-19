package hubserver

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workitemread"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) hydrateWorkItemRelationshipFactsPage(ctx context.Context, scope hubscope.NativeScope, issues []*tracker.NativeIssue) error {
	if len(issues) == 0 {
		return nil
	}
	workItems := make([]tracker.NativeWorkItemID, 0, len(issues))
	byWorkItem := make(map[tracker.NativeWorkItemID][]*tracker.NativeIssue, len(issues))
	factsByWorkItem := make(map[tracker.NativeWorkItemID][]tenantstore.WorkItemRelationshipFact, len(issues))
	projectVisible := map[tracker.ProjectID]bool{scope.Project: true}
	for _, issue := range issues {
		if issue == nil || issue.WorkItemID == "" {
			return corestore.ErrInvalidRelationship
		}
		issue.Dependencies = []tracker.NativeWorkItemID{}
		issue.Blockers = []tracker.NativeDependency{}
		issue.Dependents = []tracker.NativeDependency{}
		issue.Related = []tracker.NativeDependency{}
		issue.DependencyReady = true
		if _, exists := byWorkItem[issue.WorkItemID]; !exists {
			workItems = append(workItems, issue.WorkItemID)
		}
		byWorkItem[issue.WorkItemID] = append(byWorkItem[issue.WorkItemID], issue)
	}
	batchUnavailable := false
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := workitemread.LoadDependencyReasons(ctx, r, scope.Project, issues); err != nil {
			return err
		}
		if batch, ok := r.(tenantstore.WorkItemRelationshipBatchReader); ok {
			for start := 0; start < len(workItems); start += corestore.MaxRelationshipBatch {
				end := min(start+corestore.MaxRelationshipBatch, len(workItems))
				facts, err := batch.WorkItemRelationshipFacts(ctx, scope.Project, workItems[start:end])
				if err != nil {
					return err
				}
				for _, fact := range facts {
					factsByWorkItem[fact.WorkItemID] = append(factsByWorkItem[fact.WorkItemID], fact)
					if fact.Relationship.RelatedProjectID != scope.Project {
						projectVisible[fact.Relationship.RelatedProjectID] = fact.RelatedProjectVisible
					}
				}
			}
			return nil
		}
		batchUnavailable = true
		return nil
	})
	if err != nil {
		return err
	}
	if batchUnavailable {
		for _, issue := range issues {
			if err := s.hydrateWorkItemRelationshipFacts(ctx, scope, issue); err != nil {
				return err
			}
		}
		return nil
	}

	projectIDs := make([]tracker.ProjectID, 0, len(projectVisible))
	for projectID := range projectVisible {
		if projectID != scope.Project && projectVisible[projectID] {
			projectIDs = append(projectIDs, projectID)
		}
	}
	slices.Sort(projectIDs)
	authorizedProjects := map[tracker.ProjectID]bool{scope.Project: true}
	if len(projectIDs) > 0 {
		err := s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error {
			for _, projectID := range projectIDs {
				targetScope := scope
				targetScope.Project = projectID
				err := s.authorizeAccessScope(ctx, r, targetScope, false)
				if errors.Is(err, accessstore.ErrDenied) || errors.Is(err, auth.ErrHostedIdentity) || errors.Is(err, operatortool.ErrAccessDenied) || hubapi.IsNativeNotFound(err) {
					authorizedProjects[projectID] = false
					continue
				}
				if err != nil {
					return err
				}
				authorizedProjects[projectID] = true
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	for workItem, matchingIssues := range byWorkItem {
		for _, issue := range matchingIssues {
			type dependencyEdges struct {
				blocked, unblocks               []string
				privateBlocked, privateUnblocks bool
			}
			edges := dependencyEdges{}
			for _, fact := range factsByWorkItem[workItem] {
				relation := fact.Relationship
				visible := relation.RelatedProjectID == scope.Project || fact.RelatedProjectVisible && authorizedProjects[relation.RelatedProjectID]
				visible = visible && scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(relation.ActorPrincipalID)})
				if !visible {
					continue
				}
				if relation.Type == "dependency" {
					switch relation.Direction {
					case "blocked_by":
						edges.blocked = append(edges.blocked, string(relation.RelatedWorkItemID))
						edges.privateBlocked = edges.privateBlocked || relation.RelatedProjectID != scope.Project
					case "blocks":
						edges.unblocks = append(edges.unblocks, string(relation.RelatedWorkItemID))
						edges.privateUnblocks = edges.privateUnblocks || relation.RelatedProjectID != scope.Project
					}
				}
				link := s.nativeRelationshipLink(relation)
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
			issue.BlockedReason = workitemread.VisibleDependencyReason(issue.BlockedReason, edges.blocked, edges.privateBlocked)
			issue.UnblocksReason = workitemread.VisibleDependencyReason(issue.UnblocksReason, edges.unblocks, edges.privateUnblocks)
		}
	}
	return nil
}

func (s *Service) nativeRelationshipLink(relation corestore.Relationship) tracker.NativeDependency {
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
	return link
}

func readNativeClaimBlockerFacts(ctx context.Context, r tenantstore.Reader, project tracker.ProjectID, ids []tracker.NativeWorkItemID) ([]tenantstore.WorkItemRelationshipFact, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	batch, ok := r.(tenantstore.WorkItemBlockerBatchReader)
	if !ok {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	return batch.WorkItemBlockerFacts(ctx, project, ids)
}

func (s *Service) visibleNativeClaimBlockers(ctx context.Context, scope hubscope.NativeScope, facts []tenantstore.WorkItemRelationshipFact) (map[tracker.NativeWorkItemID][]tracker.NativeDependency, error) {
	projects := map[tracker.ProjectID]bool{}
	for _, fact := range facts {
		if fact.RelatedProjectVisible {
			projects[fact.Relationship.RelatedProjectID] = true
		}
	}
	allowed, err := s.relationshipProjectAccess(ctx, scope, projects)
	if err != nil {
		return nil, err
	}
	byItem := map[tracker.NativeWorkItemID][]tracker.NativeDependency{}
	for _, fact := range facts {
		relation := fact.Relationship
		if !allowed[relation.RelatedProjectID] {
			continue
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(relation.ActorPrincipalID)}) {
			continue
		}
		byItem[fact.WorkItemID] = append(byItem[fact.WorkItemID], s.nativeRelationshipLink(relation))
	}
	for _, blockers := range byItem {
		slices.SortFunc(blockers, func(a, b tracker.NativeDependency) int { return strings.Compare(string(a.ID), string(b.ID)) })
	}
	return byItem, nil
}

func nativeClaimDependenciesReady(blockers []tracker.NativeDependency) bool {
	return !slices.ContainsFunc(blockers, func(blocker tracker.NativeDependency) bool { return !blocker.Terminal })
}
