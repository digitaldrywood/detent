package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
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

func (s *Service) readWorkItemRelationshipPage(ctx context.Context, scope hubscope.NativeScope, request operatortool.WorkReadRequest, item tracker.NativeIssue) (workItemRelationshipPage, error) {
	var page workItemRelationshipPage
	err := s.readWorkItemPageSnapshot(ctx, scope, func(ctx context.Context) error {
		var err error
		page, err = s.readRelationshipPageModel(ctx, scope, request, item)
		return err
	})
	if pageModelPending(err) {
		return s.readWorkItemRelationshipSourcePage(ctx, scope, request, item)
	}
	return page, err
}

func (s *Service) readRelationshipPageModel(ctx context.Context, scope hubscope.NativeScope, request operatortool.WorkReadRequest, item tracker.NativeIssue) (workItemRelationshipPage, error) {
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
	path := "/api/v2/organizations/" + url.PathEscape(string(scope.Organization)) + "/projects/" + url.PathEscape(string(scope.Project)) + "/work-items/" + url.PathEscape(string(item.WorkItemID)) + "/relationships"
	limit, cursor, key, err := s.readNativePageLimit(ctx, scope, path, params, hubapi.MaxAPIPageLimit, hubapi.DefaultAPIPageLimit)
	if err != nil {
		return page, err
	}
	after := corestore.RelationshipCursor{}
	if cursor.After != "" {
		if err := json.Unmarshal([]byte(cursor.After), &after); err != nil {
			return page, hubapi.NativePublicInvalid("Relationship cursor is invalid")
		}
	}
	if err := s.authorizeCommentWorkItem(ctx, scope, string(item.WorkItemID)); err != nil {
		return page, err
	}
	var records []viewstore.RelationshipPageRecord
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		reader, ok := r.(viewstore.RelationshipPageReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		records, err = reader.RelationshipPageRecords(ctx, scope.Project, item.WorkItemID, after, limit)
		return err
	})
	if err != nil {
		return page, pageModelError(err)
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
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(relation.ActorPrincipalID)}) {
			continue
		}
		if scope.Credential.EntryKey != nil && !scope.Credential.EntryKey.Allows(string(relation.RelatedProjectID)) {
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

func (s *Service) GetWorkItemRelationships(c echo.Context) error {
	params, err := nativeReadQuery(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := hubapi.ValidateNativeQuery(params); err != nil {
		return s.nativeAPIError(c, err)
	}
	limit := 0
	if raw := params.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > hubapi.MaxAPIPageLimit {
			return s.nativeAPIError(c, hubapi.NativePublicInvalid("Page limit is invalid"))
		}
	}
	scope := hubscope.NativeRequestScope(c)
	item := tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: tracker.NativeWorkItemID(c.Param("item"))}}
	var page workItemRelationshipPage
	err = s.readWorkItemPageSnapshot(c.Request().Context(), scope, func(ctx context.Context) error {
		var err error
		page, err = s.readRelationshipPageModel(ctx, scope, operatortool.WorkReadRequest{Reference: c.Param("item"), Limit: limit, Cursor: params.Get("cursor")}, item)
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, page)
}

func (s *Service) readWorkItemRelationshipSourcePage(ctx context.Context, scope hubscope.NativeScope, request operatortool.WorkReadRequest, item tracker.NativeIssue) (workItemRelationshipPage, error) {
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
	path := "/api/v2/organizations/" + url.PathEscape(string(scope.Organization)) + "/projects/" + url.PathEscape(string(scope.Project)) + "/work-items/" + url.PathEscape(string(item.WorkItemID)) + "/relationships"
	limit, cursor, key, err := s.readNativePageLimit(ctx, scope, path, params, hubapi.MaxAPIPageLimit, hubapi.DefaultAPIPageLimit)
	if err != nil {
		return page, err
	}
	after := corestore.RelationshipCursor{}
	if cursor.After != "" {
		if err := json.Unmarshal([]byte(cursor.After), &after); err != nil {
			return page, hubapi.NativePublicInvalid("Relationship cursor is invalid")
		}
	}
	var relations corestore.RelationshipPage
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		relations, err = r.Relationships(ctx, scope.Project, item.WorkItemID, after, limit)
		return err
	})
	if err != nil {
		return page, hubapi.ProjectRecordError(err)
	}
	projectVisible := map[tracker.ProjectID]bool{}
	for _, relation := range relations.Items {
		projectVisible[relation.RelatedProjectID] = true
	}
	authorizedProjects, err := s.relationshipProjectAccess(ctx, scope, projectVisible)
	if err != nil {
		return page, err
	}
	for _, relation := range relations.Items {
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(relation.ActorPrincipalID)}) {
			continue
		}
		if scope.Credential.EntryKey != nil && !scope.Credential.EntryKey.Allows(string(relation.RelatedProjectID)) {
			continue
		}
		if !authorizedProjects[relation.RelatedProjectID] {
			continue
		}
		browserURL := ""
		if relation.OrganizationSlug != "" && relation.ProjectSlug != "" && s.config.Hosted != nil {
			browserURL = s.nativeIssueResponse(tracker.NativeIssue{WebURL: hubrecords.IssueBrowserPath(relation.OrganizationSlug, relation.ProjectSlug, relation.Number)}).WebURL
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
				page.Blockers = append(page.Blockers, link)
			case "blocks":
				page.Dependents = append(page.Dependents, link)
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
