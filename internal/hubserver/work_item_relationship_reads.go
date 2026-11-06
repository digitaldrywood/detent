package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type workItemRelationshipPage struct {
	Dependencies []tracker.NativeWorkItemID  `json:"dependencies"`
	Blockers     []tracker.NativeDependency  `json:"blockers"`
	Dependents   []tracker.NativeDependency  `json:"dependents"`
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

	var relations []corestore.Relationship
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := loadDependencyReasonRecords(ctx, r, scope.project, []*tracker.NativeIssue{issue}); err != nil {
			return err
		}
		batch, ok := r.(tenantstore.WorkItemRelationshipBatchReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		facts, err := batch.WorkItemRelationshipFacts(ctx, scope.project, []tracker.NativeWorkItemID{issue.WorkItemID})
		if err != nil {
			return err
		}
		for _, fact := range facts {
			relations = append(relations, fact.Relationship)
			if fact.Relationship.RelatedProjectID != scope.project {
				projectVisible[fact.Relationship.RelatedProjectID] = fact.RelatedProjectVisible
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	authorizedProjects, err := s.relationshipProjectAccess(ctx, scope, projectVisible)
	if err != nil {
		return err
	}

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
		visible := authorizedProjects[relation.RelatedProjectID]
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
	var page workItemRelationshipPage
	err := s.readWorkItemPageSnapshot(ctx, scope, func(ctx context.Context) error {
		var err error
		page, err = s.readRelationshipPageModel(ctx, scope, request, item)
		return err
	})
	return page, err
}

func (s *Service) readRelationshipPageModel(ctx context.Context, scope nativeScope, request operatortool.WorkReadRequest, item tracker.NativeIssue) (workItemRelationshipPage, error) {
	page := workItemRelationshipPage{
		Dependencies: []tracker.NativeWorkItemID{},
		Blockers:     []tracker.NativeDependency{},
		Dependents:   []tracker.NativeDependency{},
		References:   append([]tracker.ExternalReference{}, item.ExternalReferences...),
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
	if err := s.authorizeCommentWorkItem(ctx, scope, string(item.WorkItemID)); err != nil {
		return page, err
	}
	var records []viewstore.RelationshipPageRecord
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		reader, ok := r.(viewstore.RelationshipPageReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		records, err = reader.RelationshipPageRecords(ctx, scope.project, item.WorkItemID, after, limit)
		return err
	})
	if err != nil {
		return page, projectRecordError(err)
	}
	projectVisible := map[tracker.ProjectID]bool{}
	for _, record := range records[:min(limit, len(records))] {
		relation := record.Relationship
		projectVisible[relation.RelatedProjectID] = true
	}
	authorizedProjects, err := s.relationshipProjectAccess(ctx, scope, projectVisible)
	if err != nil {
		return page, err
	}
	for _, record := range records[:min(limit, len(records))] {
		relation := record.Relationship
		if !record.TargetExists {
			continue
		}
		if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(relation.ActorPrincipalID)}) {
			continue
		}
		if scope.credential.EntryKey != nil && !scope.credential.EntryKey.Allows(string(relation.RelatedProjectID)) {
			continue
		}
		if !authorizedProjects[relation.RelatedProjectID] {
			continue
		}

		link := tracker.NativeDependency{
			Dispatchable: relation.Dispatchable,
			Number:       relation.Number,
			Title:        relation.Title,
			WebURL:       record.WebURL,
			Identifier:   record.Identifier,
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
				page.Blockers = append(page.Blockers, link)
			case "blocks":
				page.Dependents = append(page.Dependents, link)
			}
		case "related":
			page.Related = append(page.Related, link)
		}
	}
	if len(records) > limit {
		last := records[limit-1].Relationship
		next := corestore.RelationshipCursor{Type: last.Type, Direction: last.Direction, RelatedProjectID: last.RelatedProjectID, RelatedWorkItemID: last.RelatedWorkItemID}
		cursorAfter, err := json.Marshal(next)
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

type relationshipProjectReader interface {
	RelationshipProjects(context.Context, accessstore.Authority, []tracker.ProjectID, time.Time) (map[tracker.ProjectID]bool, error)
}

func (s *Service) relationshipProjectAccess(ctx context.Context, scope nativeScope, visible map[tracker.ProjectID]bool) (map[tracker.ProjectID]bool, error) {
	allowed := map[tracker.ProjectID]bool{scope.project: true}
	projects := []tracker.ProjectID{}
	for project, active := range visible {
		if project != scope.project && active && (scope.credential.EntryKey == nil || scope.credential.EntryKey.Allows(string(project))) {
			projects = append(projects, project)
		}
	}
	if len(projects) == 0 {
		return allowed, nil
	}
	err := s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error {
		if batch, ok := r.(relationshipProjectReader); ok {
			if s.config.Hosted != nil && scope.organization != s.accessOrganization() {
				return auth.ErrHostedIdentity
			}
			required := apikey.ScopeRead
			if scope.requireHostedAdmin {
				required = apikey.ScopeAdmin
			}
			if scope.credential.EntryKey != nil && (!hostedKeyAllows(scope.credential.HostedKeyScope, required) || !hostedRoleAllows(scope.credential.HostedRole, required, scope.action)) {
				return auth.ErrHostedIdentity
			}
			found, err := batch.RelationshipProjects(ctx, s.accessAuthority(scope, false), projects, s.config.now())
			if err != nil {
				return err
			}
			for project, visible := range found {
				allowed[project] = visible
			}
			return nil
		}
		for _, project := range projects {
			target := scope
			target.project = project
			err := s.authorizeAccessScope(ctx, r, target, false)
			if errors.Is(err, accessstore.ErrDenied) || errors.Is(err, auth.ErrHostedIdentity) || errors.Is(err, operatortool.ErrAccessDenied) || isNativeNotFound(err) {
				continue
			}
			if err != nil {
				return err
			}
			allowed[project] = true
		}
		return nil
	})
	return allowed, err
}

func (s *Service) GetWorkItemRelationships(c echo.Context) error {
	params, err := nativeReadQuery(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := validateNativeQuery(params); err != nil {
		return s.nativeAPIError(c, err)
	}
	limit := 0
	if raw := params.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxAPIPageLimit {
			return s.nativeAPIError(c, nativePublicInvalid("Page limit is invalid"))
		}
	}
	scope := nativeRequestScope(c)
	item := tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: tracker.NativeWorkItemID(c.Param("item"))}}
	page, err := s.readWorkItemRelationshipPage(c.Request().Context(), scope, operatortool.WorkReadRequest{Reference: c.Param("item"), Limit: limit, Cursor: params.Get("cursor")}, item)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, page)
}
