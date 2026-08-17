package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestConnectorHydratesLinkedPullRequestsConcurrently(t *testing.T) {
	t.Parallel()

	const issueCount = 4
	allDetailsStarted := make(chan struct{})
	var detailRequests atomic.Int64
	var startedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if pullRequestNumber, ok := pullRequestDetailNumber(r.URL.Path); ok {
			if detailRequests.Add(1) == issueCount {
				startedOnce.Do(func() { close(allDetailsStarted) })
			}
			select {
			case <-allDetailsStarted:
			case <-r.Context().Done():
				return
			}
			_, _ = fmt.Fprintf(w, `{"number":%d,"html_url":"https://github.com/digitaldrywood/detent/pull/%d","state":"open","head":{"ref":"detent/issue-%d","sha":"sha-%d"}}`, pullRequestNumber, pullRequestNumber, pullRequestNumber, pullRequestNumber)
			return
		}
		writePullRequestStatusResponse(w, r.URL.Path)
	}))
	t.Cleanup(server.Close)

	githubConnector, err := NewConnector(Config{
		Endpoint:                   server.URL,
		APIKey:                     "token",
		HTTPClient:                 server.Client(),
		DisableConditionalRequests: true,
	})
	if err != nil {
		t.Fatalf("NewConnector() error = %v", err)
	}

	issues := linkedPullRequestIssues(1, issueCount)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	if err := githubConnector.attachPullRequests(ctx, issues); err != nil {
		t.Fatalf("attachPullRequests() error = %v; detail requests started = %d, want %d concurrent requests", err, detailRequests.Load(), issueCount)
	}
	if got := detailRequests.Load(); got != issueCount {
		t.Fatalf("detail requests started = %d, want %d", got, issueCount)
	}
	for index, issue := range issues {
		if issue.PullRequest == nil || issue.PullRequest.Number != index+1 {
			t.Fatalf("issues[%d].PullRequest = %#v, want PR %d", index, issue.PullRequest, index+1)
		}
	}
}

func TestConnectorKeepsHydratingCandidatesWhenChecksReferenceIsNotFound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		notFoundPath string
	}{
		{
			name:         "check runs",
			notFoundPath: "/commits/sha-1/check-runs",
		},
		{
			name:         "commit statuses",
			notFoundPath: "/commits/sha-1/statuses",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var notFoundRequests atomic.Int64
			var stalePullRequestReviews atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.Path, tt.notFoundPath) {
					notFoundRequests.Add(1)
					http.Error(w, "reference not found", http.StatusNotFound)
					return
				}
				if strings.Contains(r.URL.Path, "/commits/sha-2/check-runs") {
					_, _ = w.Write([]byte(`{"check_runs":[{"status":"completed","conclusion":"success"}]}`))
					return
				}
				if pullRequestNumber, ok := pullRequestDetailNumber(r.URL.Path); ok {
					_, _ = fmt.Fprintf(w, `{"number":%d,"html_url":"https://github.com/digitaldrywood/detent/pull/%d","state":"open","head":{"ref":"detent/issue-%d","sha":"sha-%d"}}`, pullRequestNumber, pullRequestNumber, pullRequestNumber, pullRequestNumber)
					return
				}
				if strings.Contains(r.URL.Path, "/pulls/1/reviews") {
					stalePullRequestReviews.Store(true)
				}
				writePullRequestStatusResponse(w, r.URL.Path)
			}))
			t.Cleanup(server.Close)

			githubConnector, err := NewConnector(Config{
				Endpoint:                   server.URL,
				APIKey:                     "token",
				HTTPClient:                 server.Client(),
				RESTFanoutMaxRequests:      80,
				DisableConditionalRequests: true,
			})
			if err != nil {
				t.Fatalf("NewConnector() error = %v", err)
			}

			for cycle := 1; cycle <= 2; cycle++ {
				issues := linkedPullRequestIssues(1, 2)
				if err := githubConnector.attachPullRequests(context.Background(), issues); err != nil {
					t.Fatalf("cycle %d attachPullRequests() error = %v", cycle, err)
				}
				if issues[0].PullRequest == nil || issues[0].PullRequest.HydrationUnavailableReason != connector.PullRequestHydrationReasonChecksUnavailable {
					t.Fatalf("cycle %d stale PullRequest = %#v, want checks-unavailable hydration", cycle, issues[0].PullRequest)
				}
				if issues[1].PullRequest == nil || issues[1].PullRequest.Number != 2 || issues[1].PullRequest.CIStatus != "pass" {
					t.Fatalf("cycle %d healthy PullRequest = %#v, want fully hydrated PR 2", cycle, issues[1].PullRequest)
				}
			}
			if !stalePullRequestReviews.Load() {
				t.Fatal("stale pull request reviews were not hydrated")
			}
			if got := notFoundRequests.Load(); got != 2 {
				t.Fatalf("not-found request count = %d, want 2 so incomplete checks are not cached", got)
			}
		})
	}
}

func TestConnectorFiniteFanoutCompletesPriorityHydrationBeforeConcurrentTail(t *testing.T) {
	t.Parallel()

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	var firstOnce sync.Once
	var secondOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if pullRequestNumber, ok := pullRequestDetailNumber(r.URL.Path); ok {
			switch pullRequestNumber {
			case 1:
				firstOnce.Do(func() { close(firstStarted) })
				select {
				case <-releaseFirst:
				case <-r.Context().Done():
					return
				}
			case 2:
				secondOnce.Do(func() { close(secondStarted) })
			}
			_, _ = fmt.Fprintf(w, `{"number":%d,"html_url":"https://github.com/digitaldrywood/detent/pull/%d","state":"open","head":{"ref":"detent/issue-%d","sha":"sha-%d"}}`, pullRequestNumber, pullRequestNumber, pullRequestNumber, pullRequestNumber)
			return
		}
		writePullRequestStatusResponse(w, r.URL.Path)
	}))
	t.Cleanup(server.Close)

	githubConnector, err := NewConnector(Config{
		Endpoint:                   server.URL,
		APIKey:                     "token",
		HTTPClient:                 server.Client(),
		RESTFanoutMaxRequests:      80,
		DisableConditionalRequests: true,
	})
	if err != nil {
		t.Fatalf("NewConnector() error = %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- githubConnector.attachPullRequests(context.Background(), linkedPullRequestIssues(1, 2))
	}()

	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("priority hydration did not start")
	}
	select {
	case <-secondStarted:
		t.Fatal("concurrent tail started before priority hydration completed")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("attachPullRequests() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("attachPullRequests() did not complete")
	}
	select {
	case <-secondStarted:
	default:
		t.Fatal("concurrent tail did not start after priority hydration completed")
	}
}

func TestConnectorFiniteFanoutPreservesPriorityWithPaginatedHydration(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if pullRequestNumber, ok := pullRequestDetailNumber(r.URL.Path); ok {
			_, _ = fmt.Fprintf(w, `{"number":%d,"html_url":"https://github.com/digitaldrywood/detent/pull/%d","state":"open","head":{"ref":"detent/issue-%d","sha":"sha-%d"}}`, pullRequestNumber, pullRequestNumber, pullRequestNumber, pullRequestNumber)
			return
		}
		if strings.Contains(r.URL.Path, "/reviews") {
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if page == 0 {
				page = 1
			}
			if page < 3 {
				next := *r.URL
				query := next.Query()
				query.Set("page", strconv.Itoa(page+1))
				next.RawQuery = query.Encode()
				w.Header().Set("Link", "<"+next.String()+">; rel=\"next\"")
			}
			_, _ = w.Write([]byte(`[]`))
			return
		}
		writePullRequestStatusResponse(w, r.URL.Path)
	}))
	t.Cleanup(server.Close)

	githubConnector, err := NewConnector(Config{
		Endpoint:                   server.URL,
		APIKey:                     "token",
		HTTPClient:                 server.Client(),
		RESTFanoutMaxRequests:      10,
		DisableConditionalRequests: true,
	})
	if err != nil {
		t.Fatalf("NewConnector() error = %v", err)
	}

	issues := linkedPullRequestIssues(1, 2)
	if err := githubConnector.attachPullRequests(context.Background(), issues); err != nil {
		t.Fatalf("attachPullRequests() error = %v", err)
	}
	if issues[0].PullRequest == nil || issues[0].PullRequest.HydrationUnavailableReason != "" {
		t.Fatalf("priority PullRequest = %#v, want complete hydration", issues[0].PullRequest)
	}
	if issues[1].PullRequest == nil || issues[1].PullRequest.HydrationUnavailableReason != connector.PullRequestHydrationReasonRESTBudgetReserved {
		t.Fatalf("tail PullRequest = %#v, want deferred hydration", issues[1].PullRequest)
	}
}

func BenchmarkConnectorHydratesLinkedPullRequests(b *testing.B) {
	const (
		issueCount   = 8
		requestDelay = 5 * time.Millisecond
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timer := time.NewTimer(requestDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if pullRequestNumber, ok := pullRequestDetailNumber(r.URL.Path); ok {
			_, _ = fmt.Fprintf(w, `{"number":%d,"html_url":"https://github.com/digitaldrywood/detent/pull/%d","state":"open","head":{"ref":"detent/issue-%d","sha":"sha-%d"}}`, pullRequestNumber, pullRequestNumber, pullRequestNumber, pullRequestNumber)
			return
		}
		writePullRequestStatusResponse(w, r.URL.Path)
	}))
	b.Cleanup(server.Close)

	githubConnector, err := NewConnector(Config{
		Endpoint:                   server.URL,
		APIKey:                     "token",
		HTTPClient:                 server.Client(),
		RESTFanoutMaxRequests:      80,
		DisableConditionalRequests: true,
	})
	if err != nil {
		b.Fatalf("NewConnector() error = %v", err)
	}

	nextPullRequest := 1
	b.ResetTimer()
	for b.Loop() {
		issues := linkedPullRequestIssues(nextPullRequest, issueCount)
		if err := githubConnector.attachPullRequests(context.Background(), issues); err != nil {
			b.Fatalf("attachPullRequests() error = %v", err)
		}
		nextPullRequest += issueCount
	}
}

func linkedPullRequestIssues(first int, count int) []connector.Issue {
	issues := make([]connector.Issue, count)
	for index := range count {
		number := first + index
		issues[index] = connector.Issue{
			Identifier: fmt.Sprintf("digitaldrywood/detent#%d", number),
			PRNumber:   &number,
		}
	}
	return issues
}

func pullRequestDetailNumber(path string) (int, bool) {
	if !strings.Contains(path, "/pulls/") || strings.HasSuffix(path, "/reviews") {
		return 0, false
	}
	number, err := strconv.Atoi(path[strings.LastIndex(path, "/")+1:])
	return number, err == nil
}

func writePullRequestStatusResponse(w http.ResponseWriter, path string) {
	switch {
	case strings.HasSuffix(path, "/check-runs") || strings.Contains(path, "/check-runs?"):
		_, _ = w.Write([]byte(`{"check_runs":[]}`))
	case strings.HasSuffix(path, "/statuses") || strings.Contains(path, "/statuses?"):
		_, _ = w.Write([]byte(`[]`))
	case strings.HasSuffix(path, "/reviews") || strings.Contains(path, "/reviews?"):
		_, _ = w.Write([]byte(`[]`))
	default:
		http.Error(w, "unexpected path "+path, http.StatusNotFound)
	}
}
