package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/workduplicates"
	"github.com/digitaldrywood/detent/internal/hubserver/workitemread"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) workItemReader() workitemread.Reader {
	deps := workitemread.Deps{Tenants: s.config.TenantRepository, Access: s.config.AccessRepository, Details: s.runDetailStore(), Now: s.config.now, Organization: s.accessOrganization(), Authority: s.accessAuthority, Authorize: s.authorizeAccessScope}
	if s.config.Hosted != nil {
		deps.Hosted, deps.PublicURL = true, s.config.Hosted.PublicURL
	}
	return workitemread.New(deps)
}

func (s *Service) readWorkItemRecord(ctx context.Context, scope hubscope.NativeScope, id string) (corestore.WorkItem, eventstore.CollaborationVersion, error) {
	return s.workItemReader().Record(ctx, scope, id)
}

func (s *Service) hydrateWorkItemRecord(ctx context.Context, row corestore.WorkItem, version eventstore.CollaborationVersion) (tracker.NativeIssue, error) {
	return s.workItemReader().Hydrate(ctx, row, version)
}

func (s *Service) hydrateCanonicalWorkItemRecord(ctx context.Context, row corestore.WorkItem, version eventstore.CollaborationVersion) (tracker.NativeIssue, error) {
	return s.workItemReader().HydrateCanonical(ctx, row, version)
}

func (s *Service) readWorkItem(ctx context.Context, scope hubscope.NativeScope, id string) (tracker.NativeIssue, error) {
	return s.workItemReader().Read(ctx, scope, id)
}

func (s *Service) hydrateWorkItemActorNames(ctx context.Context, organization tracker.OrganizationID, issues []*tracker.NativeIssue) error {
	return s.workItemReader().HydrateActorNames(ctx, organization, issues)
}

func (s *Service) hydrateWorkItemRelationshipFacts(ctx context.Context, scope hubscope.NativeScope, issue *tracker.NativeIssue) error {
	return s.workItemReader().HydrateRelationshipFacts(ctx, scope, issue)
}

func (s *Service) relationshipProjectAccess(ctx context.Context, scope hubscope.NativeScope, visible map[tracker.ProjectID]bool) (map[tracker.ProjectID]bool, error) {
	return s.workItemReader().ProjectAccess(ctx, scope, visible)
}

func (s *Service) hydrateLinkedIssueSourceRecord(ctx context.Context, organization tracker.OrganizationID, record eventstore.LinkedSource) (tracker.LinkedIssueSource, error) {
	return s.workItemReader().HydrateLinkedSource(ctx, organization, record)
}

func (s *Service) issueBrowserURL(ctx context.Context, organization tracker.OrganizationID, id string) (string, error) {
	return s.workItemReader().BrowserURL(ctx, organization, id)
}

func (s *Service) readIssueBrowserPath(ctx context.Context, organization tracker.OrganizationID, id string) (string, error) {
	return s.workItemReader().BrowserPath(ctx, organization, id)
}

func (s *Service) nativeIssueResponse(issue tracker.NativeIssue) tracker.NativeIssue {
	return s.workItemReader().IssueResponse(issue)
}

func (s *Service) duplicateDeps() workduplicates.Deps {
	return workduplicates.Deps{Tenants: s.config.TenantRepository, Details: s.runDetailStore(), Now: s.config.now, Logger: s.config.Logger, AuthorizeRecord: s.authorizeWorkItemRecordScope, Read: s.readWorkItem}
}

func (s *Service) prepareWorkItemDuplicates(ctx context.Context, scope hubscope.NativeScope, request tracker.CreateIssue, machine bool) (*workduplicates.Search, error) {
	return workduplicates.Prepare(ctx, s.duplicateDeps(), scope, request, machine)
}
