package github

import (
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

type pullRequestStatusCache struct {
	mu      sync.RWMutex
	ttl     time.Duration
	now     func() time.Time
	entries map[pullRequestStatusCacheKey]pullRequestStatusCacheEntry
}

type pullRequestStatusCacheKey struct {
	repository string
	number     int
	headSHA    string
}

type pullRequestStatusCacheEntry struct {
	status   pullRequestStatus
	cachedAt time.Time
}

type pullRequestStatus struct {
	ci      pullRequestCI
	reviews pullRequestCodexReviews
}

func newPullRequestStatusCache(ttl time.Duration, now func() time.Time) *pullRequestStatusCache {
	if now == nil {
		now = time.Now
	}
	return &pullRequestStatusCache{
		ttl:     ttl,
		now:     now,
		entries: map[pullRequestStatusCacheKey]pullRequestStatusCacheEntry{},
	}
}

func (c *pullRequestStatusCache) Get(repo pullRequestRepo, number int, headSHA string) (pullRequestStatus, bool) {
	key, ok := newPullRequestStatusCacheKey(repo, number, headSHA)
	if !ok {
		return pullRequestStatus{}, false
	}

	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok {
		return pullRequestStatus{}, false
	}
	if c.fresh(entry.cachedAt) {
		return clonePullRequestStatus(entry.status), true
	}

	c.mu.Lock()
	if current, ok := c.entries[key]; ok && c.fresh(current.cachedAt) {
		entry = current
	} else if ok {
		delete(c.entries, key)
	}
	c.mu.Unlock()

	if c.fresh(entry.cachedAt) {
		return clonePullRequestStatus(entry.status), true
	}
	return pullRequestStatus{}, false
}

func (c *pullRequestStatusCache) Set(repo pullRequestRepo, number int, headSHA string, status pullRequestStatus) {
	key, ok := newPullRequestStatusCacheKey(repo, number, headSHA)
	if !ok {
		return
	}

	c.mu.Lock()
	c.entries[key] = pullRequestStatusCacheEntry{
		status:   clonePullRequestStatus(status),
		cachedAt: c.now(),
	}
	c.mu.Unlock()
}

func (c *pullRequestStatusCache) fresh(cachedAt time.Time) bool {
	return c.ttl > 0 && c.now().Sub(cachedAt) < c.ttl
}

func newPullRequestStatusCacheKey(repo pullRequestRepo, number int, headSHA string) (pullRequestStatusCacheKey, bool) {
	repository := pullRequestRepoName(repo)
	headSHA = strings.TrimSpace(headSHA)
	if repository == "" || number <= 0 || headSHA == "" {
		return pullRequestStatusCacheKey{}, false
	}
	return pullRequestStatusCacheKey{
		repository: repository,
		number:     number,
		headSHA:    headSHA,
	}, true
}

func clonePullRequestStatus(status pullRequestStatus) pullRequestStatus {
	return pullRequestStatus{
		ci:      clonePullRequestCI(status.ci),
		reviews: clonePullRequestCodexReviews(status.reviews),
	}
}

func clonePullRequestCI(ci pullRequestCI) pullRequestCI {
	return pullRequestCI{
		State:              ci.State,
		CheckRunCount:      ci.CheckRunCount,
		StatusContextCount: ci.StatusContextCount,
		CIDurationSeconds:  ci.CIDurationSeconds,
		SlowChecks:         append([]connector.PullRequestCheck(nil), ci.SlowChecks...),
		RunningChecks:      append([]string(nil), ci.RunningChecks...),
	}
}

func clonePullRequestCodexReviews(reviews pullRequestCodexReviews) pullRequestCodexReviews {
	return pullRequestCodexReviews{
		CurrentHead: clonePullRequestReviews(reviews.CurrentHead),
		Latest:      clonePullRequestReviews(reviews.Latest),
	}
}

func clonePullRequestReviews(reviews []pullRequestReview) []pullRequestReview {
	out := make([]pullRequestReview, 0, len(reviews))
	for _, review := range reviews {
		out = append(out, clonePullRequestReview(review))
	}
	return out
}

func clonePullRequestReview(review pullRequestReview) pullRequestReview {
	cloned := review
	if review.Author != nil {
		author := *review.Author
		cloned.Author = &author
	}
	if review.SubmittedAt != nil {
		submittedAt := *review.SubmittedAt
		cloned.SubmittedAt = &submittedAt
	}
	return cloned
}
