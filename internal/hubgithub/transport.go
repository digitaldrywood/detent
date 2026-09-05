package hubgithub

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	connectorgithub "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hubserver"
)

type requestScopeKey struct{}
type requestScope struct {
	profile   string
	operation string
	remaining int
}

func scopedRequests(ctx context.Context, profile, operation string) context.Context {
	if profile == "" {
		profile = "github_compatible"
	}
	return context.WithValue(ctx, requestScopeKey{}, &requestScope{profile: profile, operation: operation, remaining: 500})
}

type Transport struct {
	client       restClient
	queue        chan struct{}
	mu           sync.Mutex
	counts       map[string]hubserver.GitHubRequestCount
	backoff      time.Time
	lastMutation time.Time
	now          func() time.Time
}

func NewTransport(client *connectorgithub.Client) *Transport {
	return &Transport{client: client, queue: make(chan struct{}, 1), counts: make(map[string]hubserver.GitHubRequestCount), now: time.Now}
}

func (t *Transport) Counts() []hubserver.GitHubRequestCount {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make([]hubserver.GitHubRequestCount, 0, len(t.counts))
	for _, count := range t.counts {
		result = append(result, count)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Profile+result[i].Operation < result[j].Profile+result[j].Operation
	})
	return result
}

func (t *Transport) REST(ctx context.Context, method, path string, body, output any) error {
	_, err := t.request(ctx, method, path, func() (string, error) { return "", t.client.REST(ctx, method, path, body, output) })
	return err
}

func (t *Transport) RESTPage(ctx context.Context, path string, output any) (string, error) {
	return t.request(ctx, http.MethodGet, path, func() (string, error) { return t.client.RESTPage(ctx, path, output) })
}

func (t *Transport) request(ctx context.Context, method, path string, execute func() (string, error)) (string, error) {
	select {
	case t.queue <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-t.queue }()
	scope, ok := ctx.Value(requestScopeKey{}).(*requestScope)
	if !ok {
		return "", errors.New("github hub request has no operation scope")
	}
	if scope.remaining == 0 {
		return "", errors.New("github hub operation reached its 500-request bound; checkpoint remains stale")
	}
	if t.now().Before(t.backoff) {
		return "", &connectorgithub.StatusError{StatusCode: http.StatusTooManyRequests, RetryAfter: t.backoff.Sub(t.now()), Err: connectorgithub.ErrRateLimited}
	}
	if method != http.MethodGet {
		delay := t.lastMutation.Add(time.Second).Sub(t.now())
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		t.lastMutation = t.now()
	}
	scope.remaining--
	next, err := execute()
	operation := scope.operation + "." + requestFamily(path)
	key := scope.profile + ":" + operation
	t.mu.Lock()
	count := t.counts[key]
	count.Profile, count.Operation = scope.profile, operation
	count.Requests++
	if err != nil {
		count.Errors++
	}
	count.LastRequestAt = t.now().UTC()
	t.counts[key] = count
	t.mu.Unlock()
	var status *connectorgithub.StatusError
	if errors.As(err, &status) && (status.StatusCode == http.StatusForbidden || status.StatusCode == http.StatusTooManyRequests) {
		delay := max(time.Minute, status.RetryAfter)
		t.backoff = t.now().Add(delay)
		if status.ResetAt.After(t.backoff) {
			t.backoff = status.ResetAt
		}
	}
	return next, err
}

func requestFamily(path string) string {
	switch {
	case strings.Contains(path, "/issues"):
		return "issue"
	case strings.Contains(path, "/commits/"):
		return "ci"
	case strings.Contains(path, "/pulls"):
		return "pull_request"
	default:
		return "repository"
	}
}
