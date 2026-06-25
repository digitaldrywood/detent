package github

import (
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

const (
	defaultPullRequestHydrationCooldown = time.Minute
	maxPullRequestHydrationJitter       = 30 * time.Second
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

type pullRequestHydrationState struct {
	Reason      string
	NextRetryAt *time.Time
}

type pullRequestHydrationCircuitBreaker struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]pullRequestHydrationCircuitEntry
}

type pullRequestHydrationCircuitEntry struct {
	reason      string
	nextRetryAt time.Time
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

func newPullRequestHydrationCircuitBreaker(now func() time.Time) *pullRequestHydrationCircuitBreaker {
	if now == nil {
		now = time.Now
	}
	return &pullRequestHydrationCircuitBreaker{
		now:     now,
		entries: map[string]pullRequestHydrationCircuitEntry{},
	}
}

func (c *pullRequestHydrationCircuitBreaker) Current(repo pullRequestRepo) (pullRequestHydrationState, bool) {
	if c == nil {
		return pullRequestHydrationState{}, false
	}
	key := pullRequestHydrationCircuitKey(repo)
	if key == "" {
		return pullRequestHydrationState{}, false
	}
	now := c.now()

	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return pullRequestHydrationState{}, false
	}
	if !entry.nextRetryAt.After(now) {
		delete(c.entries, key)
		return pullRequestHydrationState{}, false
	}
	return newPullRequestHydrationState(entry.reason, entry.nextRetryAt), true
}

func (c *pullRequestHydrationCircuitBreaker) Trip(repo pullRequestRepo, reason string, retryAfter time.Duration) pullRequestHydrationState {
	if c == nil {
		return pullRequestHydrationState{}
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return pullRequestHydrationState{}
	}
	key := pullRequestHydrationCircuitKey(repo)
	if key == "" {
		return pullRequestHydrationState{}
	}
	if retryAfter <= 0 {
		retryAfter = defaultPullRequestHydrationCooldown
	}

	now := c.now()
	nextRetryAt := now.Add(retryAfter + pullRequestHydrationJitter(key, reason))

	c.mu.Lock()
	defer c.mu.Unlock()

	entry := c.entries[key]
	if entry.nextRetryAt.After(nextRetryAt) {
		return newPullRequestHydrationState(entry.reason, entry.nextRetryAt)
	}
	c.entries[key] = pullRequestHydrationCircuitEntry{
		reason:      reason,
		nextRetryAt: nextRetryAt,
	}
	return newPullRequestHydrationState(reason, nextRetryAt)
}

func newPullRequestHydrationState(reason string, nextRetryAt time.Time) pullRequestHydrationState {
	if nextRetryAt.IsZero() {
		return pullRequestHydrationState{Reason: strings.TrimSpace(reason)}
	}
	next := nextRetryAt.UTC()
	return pullRequestHydrationState{
		Reason:      strings.TrimSpace(reason),
		NextRetryAt: &next,
	}
}

func pullRequestHydrationCircuitKey(repo pullRequestRepo) string {
	return strings.TrimSpace(pullRequestRepoName(repo))
}

func pullRequestHydrationJitter(key string, reason string) time.Duration {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(strings.TrimSpace(key)))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(strings.TrimSpace(reason)))
	seconds := hash.Sum32()%uint32(maxPullRequestHydrationJitter/time.Second) + 1
	return time.Duration(seconds) * time.Second
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
		CIQueueSeconds:     ci.CIQueueSeconds,
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
