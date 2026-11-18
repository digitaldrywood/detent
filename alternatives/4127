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

type relationshipPageQuery struct {
	limit      int
	cursor     nativeCursor
	key        []byte
	after      corestore.RelationshipCursor
	visibility corestore.RelationshipVisibility
}

func newWorkItemRelationshipPage(item tracker.NativeIssue) workItemRelationshipPage {
	return workItemRelationshipPage{
		Dependencies: []tracker.NativeWorkItemID{},
		Blockers:     []tracker.NativeDependency{},
		Dependents:   []tracker.NativeDependency{},
		References:   append([]tracker.ExternalReference{}, item.ExternalReferences...),
		Related:      []tracker.NativeDependency{},
	}
}

func (s *Service) readWorkItemRelationshipPage(ctx context.Context, scope hubscope.NativeScope, request operatortool.WorkReadRequest, item tracker.NativeIssue) (workItemRelationshipPage, error) {
	var page workItemRelationshipPage
	var query relationshipPageQuery
	prepared := false
	err := s.readWorkItemPageSnapshot(ctx, scope, func(ctx context.Context) error {
		var err error
		query, err = s.prepareRelationshipPage(ctx, scope, request, item)
		if err != nil {
			return err
		}
		prepared = true
		page, err = s.readRelationshipPageModel(ctx, scope, item, query)
		return err
	})
	if prepared && pageModelPending(err) {
		return s.readWorkItemRelationshipSourcePage(ctx, scope, item, query)
	}
	return page, err
}

func (s *Service) prepareRelationshipPage(ctx context.Context, scope hubscope.NativeScope, request operatortool.WorkReadRequest, item tracker.NativeIssue) (relationshipPageQuery, error) {
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
		return relationshipPageQuery{}, err
	}
	query := relationshipPageQuery{limit: limit, cursor: cursor, key: key}
	if cursor.After != "" {
		if err := json.Unmarshal([]byte(cursor.After), &query.after); err != nil {
			return query, hubapi.NativePublicInvalid("Relationship cursor is invalid")
		}
	}
	if err := s.authorizeCommentWorkItem(ctx, scope, string(item.WorkItemID)); err != nil {
		return query, err
	}
	var related []tracker.ProjectID
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		related, err = r.RelatedProjects(ctx, scope.Project, item.WorkItemID)
		return err
	})
	if err != nil {
		return query, hubapi.ProjectRecordError(err)
	}
	query.visibility, err = s.workItemReader().RelationshipVisibility(ctx, scope, related)
	return query, err
}

func (s *Service) readRelationshipPageModel(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeIssue, query relationshipPageQuery) (workItemRelationshipPage, error) {
	page := newWorkItemRelationshipPage(item)
	var records []viewstore.RelationshipPageRecord
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		reader, ok := r.(viewstore.RelationshipPageReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		records, err = reader.RelationshipPageRecords(ctx, scope.Project, item.WorkItemID, query.after, query.limit, query.visibility)
		return err
	})
	if err != nil {
		return page, pageModelError(err)
	}
	for _, record := range records[:min(query.limit, len(records))] {
		page.add(record.Relationship, record.WebURL, record.Identifier)
	}
	next := corestore.RelationshipCursor{}
	if len(records) > query.limit {
		last := records[query.limit-1].Relationship
		next = corestore.RelationshipCursor{Type: last.Type, Direction: last.Direction, RelatedProjectID: last.RelatedProjectID, RelatedWorkItemID: last.RelatedWorkItemID}
	}
	return page, s.continueRelationshipPage(&page, query, next)
}

func (page *workItemRelationshipPage) add(relation corestore.Relationship, webURL, identifier string) {
	link := tracker.NativeDependency{
		Dispatchable: relation.Dispatchable,
		Number:       relation.Number,
		Title:        relation.Title,
		WebURL:       webURL,
		Identifier:   identifier,
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

func (s *Service) continueRelationshipPage(page *workItemRelationshipPage, query relationshipPageQuery, next corestore.RelationshipCursor) error {
	if next.RelatedWorkItemID == "" {
		return nil
	}
	cursorAfter, err := json.Marshal(next)
	if err != nil {
		return err
	}
	cursor := query.cursor
	cursor.After = string(cursorAfter)
	cursor.Expires = s.config.Clock().Add(time.Hour).Unix()
	page.NextCursor, err = encodeNativeCursor(cursor, query.key)
	return err
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
		query, err := s.prepareRelationshipPage(ctx, scope, operatortool.WorkReadRequest{Reference: c.Param("item"), Limit: limit, Cursor: params.Get("cursor")}, item)
		if err != nil {
			return err
		}
		page, err = s.readRelationshipPageModel(ctx, scope, item, query)
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, page)
}

func (s *Service) readWorkItemRelationshipSourcePage(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeIssue, query relationshipPageQuery) (workItemRelationshipPage, error) {
	page := newWorkItemRelationshipPage(item)
	var relations corestore.RelationshipPage
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		relations, err = r.VisibleRelationships(ctx, scope.Project, item.WorkItemID, query.after, query.limit, query.visibility)
		return err
	})
	if err != nil {
		return page, hubapi.ProjectRecordError(err)
	}
	for _, relation := range relations.Items {
		browserURL := ""
		if relation.OrganizationSlug != "" && relation.ProjectSlug != "" && s.config.Hosted != nil {
			browserURL = s.nativeIssueResponse(tracker.NativeIssue{WebURL: hubrecords.IssueBrowserPath(relation.OrganizationSlug, relation.ProjectSlug, relation.Number)}).WebURL
		}
		page.add(relation, browserURL, string(relation.RelatedProjectID)+"#"+strconv.Itoa(relation.Number))
	}
	return page, s.continueRelationshipPage(&page, query, relations.Next)
}
