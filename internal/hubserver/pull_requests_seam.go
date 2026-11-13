package hubserver

import (
	"context"
	"github.com/digitaldrywood/detent/internal/hubserver/pullrequests"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"time"
)

const pullRequestViewTTL = pullrequests.ViewTTL
const pullRequestRefreshInterval = pullrequests.RefreshInterval

type pullRequestMergeable = pullrequests.Mergeable
type pullRequestRef = pullrequests.Ref
type pullRequestCheck = pullrequests.Check
type pullRequestReview = pullrequests.Review
type pullRequestConnector = pullrequests.Connector
type pullRequestView = pullrequests.View

func mergeableFromState(state string) pullRequestMergeable {
	return pullrequests.MergeableFromState(state)
}
func externalPullRequestNumber(reference tracker.ChangeExternalReference) int {
	return pullrequests.ExternalNumber(reference)
}
func connectorPullRequestState(state string) string { return pullrequests.ConnectorState(state) }
func pullRequestCacheKey(scope nativeScope, item string) string {
	return pullrequests.CacheKey(scope.organization, scope.project, item)
}

type pullRequestCache struct{ cache *pullrequests.Cache }

func newPullRequestCache() *pullRequestCache {
	return &pullRequestCache{cache: pullrequests.NewCache()}
}
func (c *pullRequestCache) inner() *pullrequests.Cache {
	if c == nil {
		return nil
	}
	return c.cache
}
func (c *pullRequestCache) get(key string, now time.Time) ([]pullRequestView, bool) {
	return c.inner().Get(key, now)
}
func (c *pullRequestCache) set(key string, views []pullRequestView, now time.Time) {
	c.inner().Set(key, views, now)
}
func (c *pullRequestCache) invalidate(key string) { c.inner().Invalidate(key) }
func (c *pullRequestCache) allowRefresh(org tracker.OrganizationID, now time.Time) bool {
	return c.inner().AllowRefresh(org, now)
}

func (s *Service) pullRequestService(scope nativeScope) *pullrequests.Service {
	return pullrequests.New(pullrequests.Deps{
		Repository: s.config.TenantRepository, Now: s.config.now, Cache: s.pullRequests.inner(), TransportAvailable: s.config.ReconcileBackend != nil, Unavailable: ErrPostgresFeatureUnavailable,
		ValidCheckout: validCheckoutRepository,
		VerifiedCheckout: func(ctx context.Context, repository string, now time.Time) (bool, error) {
			return s.verifiedProjectCheckout(ctx, nil, scope, repository, now)
		},
		ReadItem: func(ctx context.Context, item string) error {
			_, _, err := s.readWorkItemRecord(ctx, scope, item)
			return err
		},
		ChangeDetail: func(ctx context.Context, item, id string, now time.Time) (tracker.ChangeDetail, error) {
			return s.selectedChangeDetail(ctx, scope, item, id, now)
		},
		ReleaseDelivery: func(ctx context.Context, item string) (tracker.ReleaseDelivery, bool, error) {
			return s.readReleaseDeliveryRecord(ctx, scope, item)
		},
		AuthorizeItem: func(ctx context.Context, r tenantstore.Reader, item tracker.NativeWorkItemID) error {
			return s.authorizeBulkExportItem(ctx, r, scope, item)
		},
		Changes: func(ctx context.Context, item string) ([]tracker.ChangeRequest, error) {
			return s.listChangeRecords(ctx, scope, item)
		},
	})
}
func pullRequestRefreshLimited() error { return fromFeatureError(pullrequests.RefreshLimited()) }
func (s *Service) projectRepositoryBinding(ctx context.Context, _ nativeQueryer, scope nativeScope) (*pullRequestConnector, int64, bool, error) {
	result, id, enabled, err := s.pullRequestService(scope).RepositoryBinding(ctx, s.featureScope(scope))
	return result, id, enabled, fromFeatureError(err)
}
func (s *Service) changePullRequestView(ctx context.Context, scope nativeScope, item string, change tracker.ChangeRequest, connector *pullRequestConnector, repositoryID int64, now time.Time) (pullRequestView, error) {
	result, _, err := s.pullRequestService(scope).ChangeViewSource(ctx, s.featureScope(scope), item, change, connector, repositoryID, now)
	return result, fromFeatureError(err)
}
func (s *Service) readWorkItemPullRequests(ctx context.Context, scope nativeScope, item, refresh string) ([]pullRequestView, error) {
	result, err := s.pullRequestService(scope).Read(ctx, s.featureScope(scope), item, refresh)
	return result, fromFeatureError(err)
}
