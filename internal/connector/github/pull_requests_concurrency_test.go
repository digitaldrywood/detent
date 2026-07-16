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
