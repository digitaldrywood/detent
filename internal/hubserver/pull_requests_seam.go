package hubserver

import (
	"github.com/digitaldrywood/detent/internal/hubserver/pullrequests"
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
