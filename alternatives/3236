package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type workItemRelationshipPage struct {
	Dependencies []tracker.NativeWorkItemID  `json:"dependencies"`
	Blockers     []tracker.NativeDependency  `json:"blockers"`
	References   []tracker.ExternalReference `json:"references"`
	Related      []tracker.NativeDependency  `json:"related"`
	NextCursor   string                      `json:"next_cursor,omitempty"`
}

func (s *Service) hydrateWorkItemRelationshipFacts(ctx context.Context, scope nativeScope, issue *tracker.NativeIssue) error {
	type dependencyEdges struct {
		blocked, unblocks               []string
		privateBlocked, privateUnblocks bool
	}

	issue.Dependencies = []tracker.NativeWorkItemID{}
	issue.Blockers = []tracker.NativeDependency{}
	issue.Dependents = []tracker.NativeDependency{}
	issue.Related = []tracker.NativeDependency{}
	issue.DependencyReady = true
	edges := dependencyEdges{}
	projectVisible := map[tracker.ProjectID]bool{scope.project: true}
	projectChecked := map[tracker.ProjectID]bool{scope.project: true}
	var relations []corestore.Relationship
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := loadDependencyReasonRecords(ctx, r, scope.project, []*tracker.NativeIssue{issue}); err != nil {
			return err
		}
		cursor := corestore.RelationshipCursor{}
		for {
			page, err := r.Relationships(ctx, scope.project, issue.WorkItemID, cursor, corestore.MaxPageSize)
			if err != nil {
				return err
			}
			for _, relation := range page.Items {
				relations = append(relations, relation)
				if !projectChecked[relation.RelatedProjectID] {
					targetProject, err := r.ProjectRecord(ctx, relation.RelatedProjectID)
					if err != nil && !errors.Is(err, corestore.ErrNotFound) {
						return err
					}
					projectChecked[relation.RelatedProjectID] = true
					projectVisible[relation.RelatedProjectID] = err == nil && targetProject.Core.DeletedAt == nil && targetProject.Core.Profile == "native"
				}
			}
			if page.Next.RelatedWorkItemID == "" {
				break
			}
			cursor = page.Next
		}
		return nil
	})
	if err != nil {
		return err
	}
	authorizedProjects := map[tracker.ProjectID]bool{scope.project: true}
	authorizationChecked := map[tracker.ProjectID]bool{scope.project: true}
	for _, relation := range relations {
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
		visible, checked := authorizedProjects[relation.RelatedProjectID]
		if !checked && !authorizationChecked[relation.RelatedProjectID] {
			visible = projectVisible[relation.RelatedProjectID]
			if visible {
				targetScope := scope
				targetScope.project = relation.RelatedProjectID
				err := s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error {
					return s.authorizeAccessScope(ctx, r, targetScope, false)
				})
				if errors.Is(err, accessstore.ErrDenied) || errors.Is(err, auth.ErrHostedIdentity) || errors.Is(err, operatortool.ErrAccessDenied) || isNativeNotFound(err) {
					visible = false
				} else if err != nil {
					return err
				}
			}
			authorizationChecked[relation.RelatedProjectID] = true
			authorizedProjects[relation.RelatedProjectID] = visible
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
	slices.SortFunc(issue.Blockers, func(a, b tracker.NativeDependency) int {
		return strings.Compare(string(a.ID), string(b.ID))
	})
	slices.SortFunc(issue.Dependents, func(a, b tracker.NativeDependency) int {
		return strings.Compare(string(a.ID), string(b.ID))
	})
	slices.SortFunc(issue.Related, func(a, b tracker.NativeDependency) int {
		return strings.Compare(string(a.ID), string(b.ID))
	})
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
	return nil
}

func (s *Service) readWorkItemRelationshipPage(ctx context.Context, scope nativeScope, request operatortool.WorkReadRequest, item tracker.NativeIssue) (workItemRelationshipPage, error) {
	page := workItemRelationshipPage{
		Dependencies: []tracker.NativeWorkItemID{},
		Blockers:     []tracker.NativeDependency{},
		References:   item.ExternalReferences,
		Related:      []tracker.NativeDependency{},
	}
	params := url.Values{"reference": {request.Reference}}
	if request.Limit > 0 {
		params.Set("limit", strconv.Itoa(request.Limit))
	}
	if request.Cursor != "" {
		params.Set("cursor", request.Cursor)
	}
	path := "/api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(string(scope.project)) + "/work-items/" + url.PathEscape(string(item.WorkItemID)) + "/relationships"
	limit, cursor, key, err := s.readNativePageLimit(ctx, scope, path, params, maxAPIPageLimit, defaultAPIPageLimit)
	if err != nil {
		return page, err
	}
	after := corestore.RelationshipCursor{}
	if cursor.After != "" {
		if err := json.Unmarshal([]byte(cursor.After), &after); err != nil {
			return page, nativePublicInvalid("Relationship cursor is invalid")
		}
	}
	var relations corestore.RelationshipPage
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		relations, err = r.Relationships(ctx, scope.project, item.WorkItemID, after, limit)
		return err
	})
	if err != nil {
		return page, projectRecordError(err)
	}
	for _, relation := range relations.Items {
		if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(relation.ActorPrincipalID)}) {
			continue
		}
		if scope.credential.EntryKey != nil && !scope.credential.EntryKey.Allows(string(relation.RelatedProjectID)) {
			continue
		}
		targetScope := scope
		targetScope.project = relation.RelatedProjectID
		err := s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error {
			return s.authorizeAccessScope(ctx, r, targetScope, false)
		})
		if errors.Is(err, accessstore.ErrDenied) || errors.Is(err, auth.ErrHostedIdentity) || errors.Is(err, operatortool.ErrAccessDenied) || isNativeNotFound(err) {
			continue
		}
		if err != nil {
			return page, err
		}
		browserURL := ""
		if relation.OrganizationSlug != "" && relation.ProjectSlug != "" && s.config.Hosted != nil {
			browserURL = s.nativeIssueResponse(tracker.NativeIssue{WebURL: issueBrowserPath(relation.OrganizationSlug, relation.ProjectSlug, relation.Number)}).WebURL
		}
		link := tracker.NativeDependency{
			Dispatchable: relation.Dispatchable,
			Number:       relation.Number,
			Title:        relation.Title,
			WebURL:       browserURL,
			Identifier:   string(relation.RelatedProjectID) + "#" + strconv.Itoa(relation.Number),
			ID:           relation.RelatedWorkItemID,
			ProjectID:    relation.RelatedProjectID,
			State:        relation.State,
			Terminal:     relation.Terminal,
			ClosedAt:     relation.ClosedAt,
			StartedAt:    relation.StartedAt,
		}
		switch relation.Type {
		case "dependency":
			switch relation.Direction {
			case "blocked_by":
				page.Dependencies = append(page.Dependencies, relation.RelatedWorkItemID)
			case "blocks":
				page.Blockers = append(page.Blockers, link)
			}
		case "related":
			page.Related = append(page.Related, link)
		}
	}
	if relations.Next.RelatedWorkItemID != "" {
		cursorAfter, err := json.Marshal(relations.Next)
		if err != nil {
			return page, err
		}
		cursor.After = string(cursorAfter)
		cursor.Expires = s.config.now().Add(time.Hour).Unix()
		page.NextCursor, err = encodeNativeCursor(cursor, key)
		if err != nil {
			return page, err
		}
	}
	return page, nil
}
