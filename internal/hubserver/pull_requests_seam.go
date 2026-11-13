package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/pullrequests"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type pullRequestConnector = pullrequests.Connector
type pullRequestView = pullrequests.View

func externalPullRequestNumber(reference tracker.ChangeExternalReference) int {
	return pullrequests.ExternalNumber(reference)
}
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
func (c *pullRequestCache) invalidate(key string) { c.inner().Invalidate(key) }
func (s *Service) pullRequestService(scope nativeScope) *pullrequests.Service {
	return pullrequests.New(pullrequests.Deps{
		Repository: s.config.TenantRepository, Now: s.config.now, Cache: s.pullRequests.inner(), TransportAvailable: s.config.ReconcileBackend != nil, Unavailable: hubapi.ErrPostgresFeatureUnavailable,
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
func (s *Service) projectRepositoryBinding(ctx context.Context, _ hubapi.NativeQueryer, scope nativeScope) (*pullRequestConnector, int64, bool, error) {
	result, id, enabled, err := s.pullRequestService(scope).RepositoryBinding(ctx, s.featureScope(scope))
	return result, id, enabled, hubapi.FromFeatureError(err)
}
func (s *Service) readWorkItemPullRequests(ctx context.Context, scope nativeScope, item, refresh string) ([]pullRequestView, error) {
	result, err := s.pullRequestService(scope).Read(ctx, s.featureScope(scope), item, refresh)
	return result, hubapi.FromFeatureError(err)
}
