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
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) hydrateWorkItemRelationshipFactsPage(ctx context.Context, scope nativeScope, issues []*tracker.NativeIssue) error {
	if len(issues) == 0 {
		return nil
	}
	workItems := make([]tracker.NativeWorkItemID, 0, len(issues))
	byWorkItem := make(map[tracker.NativeWorkItemID][]*tracker.NativeIssue, len(issues))
	factsByWorkItem := make(map[tracker.NativeWorkItemID][]tenantstore.WorkItemRelationshipFact, len(issues))
	projectVisible := map[tracker.ProjectID]bool{scope.project: true}
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
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := loadDependencyReasonRecords(ctx, r, scope.project, issues); err != nil {
			return err
		}
		if batch, ok := r.(tenantstore.WorkItemRelationshipBatchReader); ok {
			for start := 0; start < len(workItems); start += corestore.MaxPageSize {
				end := min(start+corestore.MaxPageSize, len(workItems))
				facts, err := batch.WorkItemRelationshipFacts(ctx, scope.project, workItems[start:end])
				if err != nil {
					return err
				}
				for _, fact := range facts {
					factsByWorkItem[fact.WorkItemID] = append(factsByWorkItem[fact.WorkItemID], fact)
					if fact.Relationship.RelatedProjectID != scope.project {
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
		if projectID != scope.project && projectVisible[projectID] {
			projectIDs = append(projectIDs, projectID)
		}
	}
	slices.Sort(projectIDs)
	authorizedProjects := map[tracker.ProjectID]bool{scope.project: true}
	if len(projectIDs) > 0 {
		if s.config.AccessRepository != nil {
			err := s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error {
				for _, projectID := range projectIDs {
					targetScope := scope
					targetScope.project = projectID
					err := s.authorizeAccessScope(ctx, r, targetScope, false)
					if errors.Is(err, accessstore.ErrDenied) || errors.Is(err, auth.ErrHostedIdentity) || errors.Is(err, operatortool.ErrAccessDenied) || isNativeNotFound(err) {
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
		} else {
			for _, projectID := range projectIDs {
				targetScope := scope
				targetScope.project = projectID
				err := s.authorizeProjectRecordWith(ctx, s.database.auth(), targetScope, false)
				if isNativeNotFound(err) {
					authorizedProjects[projectID] = false
					continue
				}
				if err != nil {
					return err
				}
				authorizedProjects[projectID] = true
			}
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
				visible := relation.RelatedProjectID == scope.project || fact.RelatedProjectVisible && authorizedProjects[relation.RelatedProjectID]
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
	}
	return nil
}
