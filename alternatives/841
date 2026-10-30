package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestPullRequestCodexReviewStateFromReviewsUsesExplicitBodySeverity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		api  string
		want string
	}{
		{
			name: "approved narrative p1 remains approved",
			body: "No P1 issues found — approved.",
			api:  "APPROVED",
			want: "APPROVED",
		},
		{
			name: "approved bracket p1 escalates",
			body: "[P1] Missing acceptance coverage.",
			api:  "APPROVED",
			want: "P1",
		},
		{
			name: "commented line anchored p2 escalates",
			body: "P2: Minor follow-up.",
			api:  "COMMENTED",
			want: "P2",
		},
		{
			name: "mid sentence p1 remains api state",
			body: "The P1 fix from last week is already present.",
			api:  "APPROVED",
			want: "APPROVED",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := pullRequestCodexReviewStateFromReviews([]pullRequestReview{{
				Body:  tt.body,
				State: tt.api,
			}})
			if got != tt.want {
				t.Fatalf("pullRequestCodexReviewStateFromReviews() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPullRequestCodexReviewStateInputsFromReviews(t *testing.T) {
	t.Parallel()

	apiState, bodySeverity := pullRequestCodexReviewStateInputsFromReviews([]pullRequestReview{{
		Body:  "[P1] Missing acceptance coverage.",
		State: "APPROVED",
	}})
	if apiState != "APPROVED" || bodySeverity != "P1" {
		t.Fatalf("state inputs = API %q body %q, want APPROVED/P1", apiState, bodySeverity)
	}
}

func TestPullRequestCodexReviewFindingsUseExplicitBodySeverity(t *testing.T) {
	t.Parallel()

	pullRequest := pullRequestNode{
		LatestReviews: nodeConnection[pullRequestReview]{Nodes: []pullRequestReview{
			{
				Body: "No P1 issues found — approved.",
				URL:  "https://github.test/review/narrative",
			},
			{
				Body: "[P1] Missing acceptance coverage.",
				URL:  "https://github.test/review/finding",
			},
		}},
	}

	got := pullRequestCodexReviewFindings(pullRequest)
	if len(got) != 1 || got[0].Body != "[P1] Missing acceptance coverage." || got[0].URL != "https://github.test/review/finding" {
		t.Fatalf("pullRequestCodexReviewFindings() = %#v, want only explicit P1 finding", got)
	}
}

func TestLatestCodexReviewRequiresExactCurrentHead(t *testing.T) {
	t.Parallel()

	headSHA := strings.Repeat("a", 40)
	tests := []struct {
		name     string
		commitID string
		want     bool
	}{
		{name: "exact head", commitID: headSHA, want: true},
		{name: "missing commit"},
		{name: "abbreviated commit", commitID: headSHA[:10]},
		{name: "stale commit", commitID: strings.Repeat("b", 40)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, got := latestCodexReview([]restReview{{
				State:    "COMMENTED",
				User:     &actor{Login: "chatgpt-codex-connector[bot]", Type: "Bot"},
				CommitID: tt.commitID,
			}}, headSHA)
			if got != tt.want {
				t.Fatalf("latestCodexReview() ok = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConnectorFetchPullRequestReviewsUsesTrustedCurrentHeadSummary(t *testing.T) {
	t.Parallel()

	const headSHA = "79f9eb2d4ad5317af5ff46f29aba3b91f4b413a0"
	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/repos/gopherguides/gopher-ai/pulls/389/reviews?per_page=100",
			body:   `[{"body":"[P1] Prior-head finding.","html_url":"https://github.com/gopherguides/gopher-ai/pull/389#pullrequestreview-1","state":"COMMENTED","user":{"login":"chatgpt-codex-connector[bot]","type":"Bot"},"commit_id":"f236cc46fbb1a0e6821f16001e97ce07e0d483cd","submitted_at":"2026-09-02T22:18:56Z"}]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/gopherguides/gopher-ai/issues/389/comments?per_page=100",
			body:   `[{"node_id":"IC_summary","body":"<!-- codex-pull-request-review-summary -->\n\n## Codex Review Summary\n\n| Review | Status | Commit | Review trigger |\n| --- | --- | --- | --- |\n| 📝 **Code Review** | ✅ **Completed** <relative-time datetime=\"2026-09-02T22:35:58.561318Z\">2026-09-02T22:35:58.561318Z</relative-time> | ` + "`79f9eb2`" + ` | Manual request |","html_url":"https://github.com/gopherguides/gopher-ai/pull/389#issuecomment-1","user":{"login":"chatgpt-codex-connector[bot]","type":"Bot"},"created_at":"2026-09-02T16:27:27Z","updated_at":"2026-09-02T22:35:59Z"}]`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{})
	reviews, err := c.fetchPullRequestReviews(context.Background(), pullRequestRepo{Owner: "gopherguides", Name: "gopher-ai"}, 389, headSHA)
	if err != nil {
		t.Fatalf("fetchPullRequestReviews() error = %v", err)
	}
	if len(reviews.CurrentHead) != 1 {
		t.Fatalf("CurrentHead = %#v, want trusted summary review", reviews.CurrentHead)
	}
	if got := pullRequestCodexReviewStateFromReviews(reviews.CurrentHead); got != "COMMENTED" {
		t.Fatalf("current-head state = %q, want COMMENTED", got)
	}
	if got := reviews.CurrentHead[0].Source; got != connector.PullRequestReviewSourceSummaryComment {
		t.Fatalf("current-head source = %q, want %q", got, connector.PullRequestReviewSourceSummaryComment)
	}
	if got := reviewBodySeverity(reviews.CurrentHead[0].Body); got != "" {
		t.Fatalf("current-head severity = %q, want clean", got)
	}
	wantSubmittedAt := time.Date(2026, 9, 2, 22, 35, 58, 561318000, time.UTC)
	if got := reviews.CurrentHead[0].SubmittedAt; got == nil || !got.Equal(wantSubmittedAt) {
		t.Fatalf("current-head submitted at = %v, want %v", got, wantSubmittedAt)
	}
	if len(reviews.Latest) != 1 || reviews.Latest[0].CommitID != "f236cc46fbb1a0e6821f16001e97ce07e0d483cd" {
		t.Fatalf("Latest = %#v, want prior-head formal review preserved", reviews.Latest)
	}
	if got := pullRequestCodexReviewStateFromReviews(reviews.Latest); got != "P1" {
		t.Fatalf("latest formal state = %q, want stale P1 to remain observable", got)
	}
}

func TestLatestCodexSummaryReviewFailsClosed(t *testing.T) {
	t.Parallel()

	completedAt := time.Date(2026, 9, 2, 22, 35, 58, 561318000, time.UTC)
	createdAt := completedAt.Add(-time.Hour)
	updatedAt := completedAt.Add(time.Second)
	headSHA := strings.Repeat("a", 40)
	trustedAuthor := &actor{Login: "chatgpt-codex-connector[bot]", Type: "Bot"}
	validComment := restComment{
		ID:        1,
		Body:      testCodexReviewSummaryBody("✅ **Completed** <relative-time datetime=\"2026-09-02T22:35:58.561318Z\">2026-09-02T22:35:58.561318Z</relative-time>", headSHA[:7]),
		HTMLURL:   "https://github.test/pull/1#issuecomment-1",
		User:      trustedAuthor,
		CreatedAt: &createdAt,
		UpdatedAt: &updatedAt,
	}

	tests := []struct {
		name          string
		comments      []restComment
		formalReviews []restReview
		want          bool
	}{
		{name: "trusted completed current head", comments: []restComment{validComment}, want: true},
		{
			name: "untrusted user",
			comments: []restComment{func() restComment {
				comment := validComment
				comment.User = &actor{Login: "chatgpt-codex-connector[bot]", Type: "User"}
				return comment
			}()},
		},
		{
			name: "untrusted bot",
			comments: []restComment{func() restComment {
				comment := validComment
				comment.User = &actor{Login: "codex-helper[bot]", Type: "Bot"}
				return comment
			}()},
		},
		{
			name: "stale head",
			comments: []restComment{func() restComment {
				comment := validComment
				comment.Body = testCodexReviewSummaryBody("✅ **Completed** <relative-time datetime=\"2026-09-02T22:35:58.561318Z\">2026-09-02T22:35:58.561318Z</relative-time>", strings.Repeat("b", 7))
				return comment
			}()},
		},
		{
			name: "incomplete review",
			comments: []restComment{func() restComment {
				comment := validComment
				comment.Body = testCodexReviewSummaryBody("🔄 **In progress**", headSHA[:7])
				return comment
			}()},
		},
		{
			name: "malformed table",
			comments: []restComment{func() restComment {
				comment := validComment
				comment.Body = strings.Replace(comment.Body, codexReviewSummaryTableSeparator, "| --- | --- |", 1)
				return comment
			}()},
		},
		{
			name: "short commit prefix",
			comments: []restComment{func() restComment {
				comment := validComment
				comment.Body = testCodexReviewSummaryBody("✅ **Completed** <relative-time datetime=\"2026-09-02T22:35:58.561318Z\">2026-09-02T22:35:58.561318Z</relative-time>", headSHA[:6])
				return comment
			}()},
		},
		{
			name:     "ambiguous commit prefix",
			comments: []restComment{validComment},
			formalReviews: []restReview{{
				CommitID: headSHA[:7] + strings.Repeat("b", 33),
			}},
		},
		{
			name: "newer incomplete edit supersedes completed summary",
			comments: []restComment{validComment, func() restComment {
				comment := validComment
				comment.ID = 2
				comment.UpdatedAt = new(updatedAt.Add(time.Minute))
				comment.Body = testCodexReviewSummaryBody("🔄 **In progress**", headSHA[:7])
				return comment
			}()},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, got := latestCodexSummaryReview(tt.comments, tt.formalReviews, headSHA)
			if got != tt.want {
				t.Fatalf("latestCodexSummaryReview() ok = %v, want %v", got, tt.want)
			}
		})
	}
}

func testCodexReviewSummaryBody(status string, commit string) string {
	return codexReviewSummaryMarker + "\n\n" + codexReviewSummaryHeading + "\n\n" +
		codexReviewSummaryTableHeader + "\n" + codexReviewSummaryTableSeparator + "\n" +
		"| 📝 **Code Review** | " + status + " | `" + commit + "` | Manual request |"
}

func TestConnectorFreshPullRequestStatusReconcilesDeletedChecks(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	t.Parallel()

	tests := []struct {
		name             string
		secondCheckState int
		wantError        bool
		wantCIStatus     string
		wantPublishedCI  string
		wantRunning      []string
	}{
		{
			name:            "authoritative snapshot removes deleted pending check",
			wantCIStatus:    "success",
			wantPublishedCI: "pass",
		},
		{
			name:             "failed snapshot retains pending check",
			secondCheckState: http.StatusServiceUnavailable,
			wantError:        true,
			wantCIStatus:     "pending",
			wantPublishedCI:  "pending",
			wantRunning:      []string{"Request Codex security review"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var checkRunPageOneRequests atomic.Int64
			var checkRunPageTwoRequests atomic.Int64
			var logs bytes.Buffer
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.RequestURI() {
				case "/repos/digitaldrywood/detent/commits/head-current/check-runs?per_page=100":
					if checkRunPageOneRequests.Add(1) == 1 {
						w.Header().Set("ETag", `"checks-page-1-v1"`)
						w.Header().Set("Link", `<`+server.URL+`/repos/digitaldrywood/detent/commits/head-current/check-runs?per_page=100&page=2>; rel="next"`)
						_, _ = w.Write([]byte(`{"check_runs":[{"id":2,"name":"Unit tests","status":"completed","conclusion":"success"}]}`))
						return
					}
					if tt.secondCheckState != 0 {
						w.WriteHeader(tt.secondCheckState)
						_, _ = w.Write([]byte(`{"message":"temporary upstream failure"}`))
						return
					}
					if r.Header.Get("If-None-Match") != "" {
						w.WriteHeader(http.StatusNotModified)
						return
					}
					w.Header().Set("ETag", `"checks-page-1-v2"`)
					w.Header().Set("Link", `<`+server.URL+`/repos/digitaldrywood/detent/commits/head-current/check-runs?per_page=100&page=2>; rel="next"`)
					_, _ = w.Write([]byte(`{"check_runs":[{"id":2,"name":"Unit tests","status":"completed","conclusion":"success"}]}`))
				case "/repos/digitaldrywood/detent/commits/head-current/check-runs?per_page=100&page=2":
					if checkRunPageTwoRequests.Add(1) == 1 {
						w.Header().Set("ETag", `"checks-page-2-v1"`)
						_, _ = w.Write([]byte(`{"check_runs":[{"id":1,"name":"Request Codex security review","status":"queued"}]}`))
						return
					}
					if r.Header.Get("If-None-Match") != "" {
						w.WriteHeader(http.StatusNotModified)
						return
					}
					w.Header().Set("ETag", `"checks-page-2-v2"`)
					_, _ = w.Write([]byte(`{"check_runs":[]}`))
				case "/repos/digitaldrywood/detent/commits/head-current/statuses?per_page=100",
					"/repos/digitaldrywood/detent/pulls/2028/reviews?per_page=100",
					"/repos/digitaldrywood/detent/issues/2028/comments?per_page=100":
					_, _ = w.Write([]byte(`[]`))
				default:
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"message":"not found"}`))
				}
			}))
			t.Cleanup(server.Close)

			githubConnector, err := NewConnector(Config{
				Endpoint:   server.URL,
				APIKey:     "token",
				HTTPClient: server.Client(),
				Logger:     slog.New(slog.NewTextHandler(&logs, nil)),
			})
			if err != nil {
				t.Fatalf("NewConnector() error = %v", err)
			}
			pullRequest := pullRequestNode{Number: 2028, HeadSHA: "head-current"}
			repo := pullRequestRepo{Owner: "digitaldrywood", Name: "detent"}

			if err := githubConnector.populatePullRequestStatus(context.Background(), repo, &pullRequest, false); err != nil {
				t.Fatalf("populatePullRequestStatus() first error = %v", err)
			}
			if pullRequest.CI.State != "pending" || !slices.Equal(pullRequest.CI.RunningChecks, []string{"Request Codex security review"}) {
				t.Fatalf("first CI = %#v, want pending security review", pullRequest.CI)
			}

			err = githubConnector.populatePullRequestStatus(context.Background(), repo, &pullRequest, false)
			if tt.wantError && err == nil {
				t.Fatal("populatePullRequestStatus() second error = nil, want error")
			}
			if !tt.wantError && err != nil {
				t.Fatalf("populatePullRequestStatus() second error = %v", err)
			}
			if pullRequest.CI.State != tt.wantCIStatus {
				t.Fatalf("second CI state = %q, want %q", pullRequest.CI.State, tt.wantCIStatus)
			}
			if !slices.Equal(pullRequest.CI.RunningChecks, tt.wantRunning) {
				t.Fatalf("second RunningChecks = %#v, want %#v", pullRequest.CI.RunningChecks, tt.wantRunning)
			}
			issue := connector.Issue{}
			attachPullRequestToIssue(&issue, repo, pullRequest)
			if issue.PullRequest.CIStatus != tt.wantPublishedCI || !slices.Equal(issue.PullRequest.RunningChecks, tt.wantRunning) {
				t.Fatalf("published PullRequest = %#v, want CI %q and running %#v", issue.PullRequest, tt.wantPublishedCI, tt.wantRunning)
			}
			if tt.secondCheckState == 0 {
				for _, fragment := range []string{
					"github pull request checks reconciled",
					"request_purpose=reconcile_current_head_checks",
					`removed_running_checks="[Request Codex security review]"`,
				} {
					if !strings.Contains(logs.String(), fragment) {
						t.Fatalf("logs missing %q:\n%s", fragment, logs.String())
					}
				}
			}
		})
	}
}

func TestCheckRunTelemetryReportsQueueAndCompletedSpan(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 6, 5, 10, 58, 0, 0, time.UTC)
	start := time.Date(2026, 6, 5, 11, 0, 0, 0, time.UTC)
	verifyDone := start.Add(3 * time.Minute)
	snapshotCreated := start.Add(time.Minute)
	snapshotStart := verifyDone.Add(30 * time.Second)
	snapshotDone := snapshotStart.Add(8 * time.Minute)

	summary := checkRunTelemetry([]restCheckRun{
		{Name: "Verify (ubuntu-latest)", Status: "completed", Conclusion: "success", CreatedAt: &created, StartedAt: &start, CompletedAt: &verifyDone},
		{Name: "GoReleaser Snapshot", Status: "completed", Conclusion: "success", CreatedAt: &snapshotCreated, StartedAt: &snapshotStart, CompletedAt: &snapshotDone},
	}, nil, time.Time{}, defaultUnstartedCheckThreshold)

	if summary.QueueSeconds != 120 {
		t.Fatalf("QueueSeconds = %d, want 120", summary.QueueSeconds)
	}
	if summary.DurationSeconds != 690 {
		t.Fatalf("DurationSeconds = %d, want 690", summary.DurationSeconds)
	}
	if len(summary.SlowChecks) != 2 || summary.SlowChecks[0].Name != "GoReleaser Snapshot" || summary.SlowChecks[0].QueueSeconds != 150 {
		t.Fatalf("SlowChecks = %#v, want snapshot first with queued runtime", summary.SlowChecks)
	}
	if len(summary.RunningChecks) != 0 {
		t.Fatalf("RunningChecks = %#v, want none", summary.RunningChecks)
	}
}

func TestCheckRunTelemetryDistinguishesUnstartedChecks(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 8, 20, 0, 0, 0, time.UTC)
	threshold := 15 * time.Minute
	oldQueuedAt := now.Add(-47 * time.Minute)
	recentQueuedAt := now.Add(-5 * time.Minute)
	oldStartedAt := now.Add(-2 * time.Hour)
	recentStartedAt := now.Add(-time.Minute)
	tests := []struct {
		name          string
		checkRuns     []restCheckRun
		wantRunning   []string
		wantUnstarted []connector.PullRequestCheck
		wantCount     int
	}{
		{
			name: "all checks in progress",
			checkRuns: []restCheckRun{
				{Name: "Portability Verify", Status: "in_progress", CreatedAt: &oldQueuedAt, StartedAt: &oldStartedAt},
				{Name: "Test", Status: "in_progress", CreatedAt: &oldQueuedAt, StartedAt: &recentStartedAt},
			},
			wantRunning: []string{"Portability Verify", "Test"},
		},
		{
			name: "all checks queued past threshold",
			checkRuns: []restCheckRun{
				{ID: 1, Name: "Portability Verify", Status: "queued", CreatedAt: &oldQueuedAt},
				{ID: 2, Name: "Test", Status: "queued", CreatedAt: &oldQueuedAt},
			},
			wantUnstarted: []connector.PullRequestCheck{
				{ID: 1, Name: "Portability Verify", Status: "queued", QueueSeconds: 47 * 60},
				{ID: 2, Name: "Test", Status: "queued", QueueSeconds: 47 * 60},
			},
		},
		{
			name:        "queued under threshold",
			checkRuns:   []restCheckRun{{Name: "Portability Verify", Status: "queued", CreatedAt: &recentQueuedAt}},
			wantRunning: []string{"Portability Verify"},
		},
		{
			name: "mixed queued and running",
			checkRuns: []restCheckRun{
				{ID: 1, Name: "Portability Verify", Status: "queued", CreatedAt: &oldQueuedAt},
				{Name: "Test", Status: "in_progress", CreatedAt: &oldQueuedAt, StartedAt: &recentStartedAt},
			},
			wantRunning: []string{"Test"},
			wantUnstarted: []connector.PullRequestCheck{
				{ID: 1, Name: "Portability Verify", Status: "queued", QueueSeconds: 47 * 60},
			},
		},
		{
			name:        "queued check that later starts",
			checkRuns:   []restCheckRun{{Name: "Portability Verify", Status: "in_progress", CreatedAt: &oldQueuedAt, StartedAt: &recentStartedAt}},
			wantRunning: []string{"Portability Verify"},
		},
		{
			name: "unstarted checks are sorted and limited",
			checkRuns: []restCheckRun{
				{ID: 6, Name: "F", Status: "queued", CreatedAt: &oldQueuedAt},
				{ID: 1, Name: "A", Status: "queued", CreatedAt: &oldQueuedAt},
				{ID: 5, Name: "E", Status: "queued", CreatedAt: &oldQueuedAt},
				{ID: 2, Name: "B", Status: "queued", CreatedAt: &oldQueuedAt},
				{ID: 4, Name: "D", Status: "queued", CreatedAt: &oldQueuedAt},
				{ID: 3, Name: "C", Status: "queued", CreatedAt: &oldQueuedAt},
			},
			wantUnstarted: []connector.PullRequestCheck{
				{ID: 1, Name: "A", Status: "queued", QueueSeconds: 47 * 60},
				{ID: 2, Name: "B", Status: "queued", QueueSeconds: 47 * 60},
				{ID: 3, Name: "C", Status: "queued", QueueSeconds: 47 * 60},
				{ID: 4, Name: "D", Status: "queued", QueueSeconds: 47 * 60},
				{ID: 5, Name: "E", Status: "queued", QueueSeconds: 47 * 60},
			},
			wantCount: 6,
		},
		{name: "empty check run list"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			summary := checkRunTelemetry(tt.checkRuns, nil, now, threshold)
			if !slices.Equal(summary.RunningChecks, tt.wantRunning) {
				t.Fatalf("RunningChecks = %#v, want %#v", summary.RunningChecks, tt.wantRunning)
			}
			if !slices.Equal(summary.UnstartedChecks, tt.wantUnstarted) {
				t.Fatalf("UnstartedChecks = %#v, want %#v", summary.UnstartedChecks, tt.wantUnstarted)
			}
			wantCount := tt.wantCount
			if wantCount == 0 {
				wantCount = len(tt.wantUnstarted)
			}
			if summary.UnstartedCount != wantCount {
				t.Fatalf("UnstartedCount = %d, want %d", summary.UnstartedCount, wantCount)
			}
		})
	}
}

func TestCheckRunsStateTreatsStaleSuccessfulCheckRunAsSuccess(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 7, 7, 0, 45, 0, 0, time.UTC)
	completed := time.Date(2026, 7, 7, 0, 49, 24, 0, time.UTC)

	checkRuns := []restCheckRun{{
		Name:        "Installer Smoke (ubuntu-latest)",
		Status:      "in_progress",
		Conclusion:  "success",
		StartedAt:   &started,
		CompletedAt: &completed,
	}}

	if got := checkRunsState(checkRuns); got != "success" {
		t.Fatalf("checkRunsState() = %q, want success", got)
	}
	telemetry := checkRunTelemetry(checkRuns, nil, time.Time{}, defaultUnstartedCheckThreshold)
	if len(telemetry.RunningChecks) != 0 {
		t.Fatalf("RunningChecks = %#v, want none", telemetry.RunningChecks)
	}
	if telemetry.DurationSeconds != 264 {
		t.Fatalf("DurationSeconds = %d, want 264", telemetry.DurationSeconds)
	}
}

func TestCheckRunsStateUsesSettledContextResults(t *testing.T) {
	t.Parallel()

	older := time.Date(2026, 7, 16, 18, 0, 0, 0, time.UTC)
	newer := older.Add(time.Minute)
	tests := []struct {
		name      string
		checkRuns []restCheckRun
		want      string
	}{
		{
			name: "pending current cycle supersedes settled failure",
			checkRuns: []restCheckRun{
				{ID: 1, Name: "Checks", Status: "completed", Conclusion: "failure", StartedAt: &older},
				{ID: 2, Name: "Checks", Status: "in_progress", StartedAt: &newer},
			},
			want: "pending",
		},
		{
			name: "newer settled success supersedes pending run",
			checkRuns: []restCheckRun{
				{ID: 1, Name: "Checks", Status: "in_progress", StartedAt: &older},
				{ID: 2, Name: "Checks", Status: "completed", Conclusion: "success", StartedAt: &newer},
			},
			want: "success",
		},
		{
			name: "newer settled failure supersedes pending run",
			checkRuns: []restCheckRun{
				{ID: 2, Name: "Checks", Status: "completed", Conclusion: "failure", StartedAt: &newer},
				{ID: 1, Name: "Checks", Status: "queued", StartedAt: &older},
			},
			want: "failure",
		},
		{
			name: "settled failure wins over a different pending context",
			checkRuns: []restCheckRun{
				{Name: "Checks", Status: "completed", Conclusion: "failure"},
				{Name: "Test", Status: "queued"},
			},
			want: "failure",
		},
		{
			name: "cancelled artifact does not shadow success",
			checkRuns: []restCheckRun{
				{ID: 1, Name: "Changed-file coverage", Status: "completed", Conclusion: "success", StartedAt: &older},
				{ID: 2, Name: "Changed-file coverage", Status: "completed", Conclusion: "cancelled", StartedAt: &newer},
			},
			want: "success",
		},
		{
			name: "skipped artifact does not shadow success",
			checkRuns: []restCheckRun{
				{ID: 1, Name: "Test", Status: "completed", Conclusion: "success", StartedAt: &older},
				{ID: 2, Name: "Test", Status: "completed", Conclusion: "skipped", StartedAt: &newer},
			},
			want: "success",
		},
		{
			name: "newer success supersedes ignored workflow runs",
			checkRuns: []restCheckRun{
				{ID: 1, Name: "Checks", Status: "completed", Conclusion: "cancelled", StartedAt: &older},
				{ID: 2, Name: "Checks", Status: "completed", Conclusion: "skipped", StartedAt: &older},
				{ID: 3, Name: "Checks", Status: "completed", Conclusion: "success", StartedAt: &newer},
			},
			want: "success",
		},
		{
			name: "newer settled success supersedes failure",
			checkRuns: []restCheckRun{
				{ID: 1, Name: "Checks", Status: "completed", Conclusion: "failure", StartedAt: &older},
				{ID: 2, Name: "Checks", Status: "completed", Conclusion: "success", StartedAt: &newer},
			},
			want: "success",
		},
		{
			name: "newer settled failure supersedes success",
			checkRuns: []restCheckRun{
				{ID: 1, Name: "Checks", Status: "completed", Conclusion: "success", StartedAt: &older},
				{ID: 2, Name: "Checks", Status: "completed", Conclusion: "failure", StartedAt: &newer},
			},
			want: "failure",
		},
		{
			name: "only ignored conclusions are non-blocking",
			checkRuns: []restCheckRun{
				{Name: "Checks", Status: "completed", Conclusion: "cancelled"},
				{Name: "Test", Status: "completed", Conclusion: "skipped"},
			},
			want: "success",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := checkRunsState(tt.checkRuns); got != tt.want {
				t.Fatalf("checkRunsState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCompletedFailedCheckRunIgnoresNonFailureArtifacts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		conclusion string
		want       bool
	}{
		{conclusion: "failure", want: true},
		{conclusion: "timed_out", want: true},
		{conclusion: "cancelled"},
		{conclusion: "canceled"},
		{conclusion: "skipped"},
	}

	for _, tt := range tests {
		t.Run(tt.conclusion, func(t *testing.T) {
			t.Parallel()

			checkRun := restCheckRun{Status: "completed", Conclusion: tt.conclusion}
			if got := completedFailedCheckRun(checkRun); got != tt.want {
				t.Fatalf("completedFailedCheckRun() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestCIStateReportsFailureWhileOtherContextsWait(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		checks       string
		statuses     []restCommitStatus
		wantStatuses string
		want         string
	}{
		{name: "mixed statuses", statuses: []restCommitStatus{{Context: "Checks", State: "failure"}, {Context: "Test", State: "pending"}}, wantStatuses: "failure", want: "failure"},
		{name: "failed check pending status", checks: "failure", statuses: []restCommitStatus{{Context: "Test", State: "pending"}}, wantStatuses: "pending", want: "failure"},
		{name: "pending check failed status", checks: "pending", statuses: []restCommitStatus{{Context: "Test", State: "error"}}, wantStatuses: "failure", want: "failure"},
		{name: "pending only", checks: "success", statuses: []restCommitStatus{{Context: "Test", State: "pending"}}, wantStatuses: "pending", want: "pending"},
		{name: "superseded failure", checks: "pending", statuses: []restCommitStatus{{Context: "Test", State: "pending", CreatedAt: new(time.Date(2026, 9, 8, 15, 0, 0, 0, time.UTC))}, {Context: "Test", State: "failure"}}, wantStatuses: "pending", want: "pending"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			statuses := commitStatusesState(tt.statuses)
			if statuses != tt.wantStatuses {
				t.Fatalf("commitStatusesState() = %q, want %q", statuses, tt.wantStatuses)
			}
			if got := combinedCIState(tt.checks, statuses); got != tt.want {
				t.Fatalf("combinedCIState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConnectorHydratePullRequestMixedCurrentHeadChecks(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		conclusion string
		required   []string
		statuses   string
		want       string
	}{
		{name: "failed check and running checks", conclusion: "failure", want: "fail"},
		{name: "failed check and missing required check", conclusion: "failure", required: []string{"Missing"}, want: "fail"},
		{name: "required failure and running required check", conclusion: "failure", required: []string{"GoReleaser Snapshot", "Windows Core"}, want: "fail"},
		{name: "failed legacy status and running check", conclusion: "success", statuses: `[{"context":"external","state":"failure"},{"context":"another","state":"pending"}]`, want: "fail"},
		{name: "old head failure replaced by pending head", conclusion: "success", want: "pending"},
		{name: "optional neutral check", conclusion: "neutral", want: "pending"},
		{name: "optional skipped check", conclusion: "skipped", want: "pending"},
		{name: "optional cancelled check", conclusion: "cancelled", want: "pending"},
		{name: "missing required check needs startup", conclusion: "success", required: []string{"Missing"}, want: "pending"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			statuses := tt.statuses
			if statuses == "" {
				statuses = `[]`
			}
			server := newGraphQLTestServer(t, []graphqlTestResponse{
				{method: http.MethodGet, path: "/repos/example/repo/pulls/42", body: `{"number":42,"state":"open","mergeable_state":"blocked","head":{"sha":"current-head"},"base":{"sha":"base"}}`},
				{method: http.MethodGet, path: "/repos/example/repo/commits/current-head/check-runs?per_page=100", body: fmt.Sprintf(`{"check_runs":[{"name":"GoReleaser Snapshot","status":"completed","conclusion":%q,"started_at":"2026-09-08T15:40:00Z","completed_at":"2026-09-08T15:42:36Z"},{"name":"Windows Core","status":"in_progress","started_at":"2026-09-08T15:40:00Z"},{"name":"Linux","status":"queued","created_at":"2026-09-08T15:40:00Z"}]}`, tt.conclusion)},
				{method: http.MethodGet, path: "/repos/example/repo/commits/current-head/statuses?per_page=100", body: statuses},
				{method: http.MethodGet, path: "/repos/example/repo/pulls/42/reviews?per_page=100", body: `[]`},
				emptyPullRequestCommentsResponse("example/repo", 42),
			})
			c := newGitHubTestConnector(t, server, Config{RequiredStatusChecks: tt.required})
			c.now = func() time.Time { return time.Date(2026, 9, 8, 15, 54, 0, 0, time.UTC) }
			c.unstartedThreshold = time.Minute
			issue := connector.Issue{ID: "issue-1", Identifier: "example/repo#1", PRNumber: new(42), PRRepository: "example/repo", PullRequest: &connector.PullRequest{Number: 42, HeadSHA: "old-head", CIStatus: "failure", RequiredCheckFailures: []connector.PullRequestCheck{{Name: "Old", Status: "completed", Conclusion: "failure"}}}}
			got, err := c.HydratePullRequest(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			pr := got.PullRequest
			if pr == nil {
				t.Fatal("missing pull request")
			}
			if pr.HeadSHA != "current-head" || pr.CIStatus != tt.want {
				t.Fatalf("head=%q CI=%q, want current-head CI=%q", pr.HeadSHA, pr.CIStatus, tt.want)
			}
			if !reflect.DeepEqual(pr.RunningChecks, []string{"Windows Core"}) || pr.UnstartedCheckCount != 1 || len(pr.UnstartedChecks) != 1 || pr.UnstartedChecks[0].Name != "Linux" {
				t.Fatalf("pending telemetry lost: running=%v unstarted=%v", pr.RunningChecks, pr.UnstartedChecks)
			}
			if pr.CIDurationSeconds != 0 || len(pr.Checks) < 3 {
				t.Fatalf("partial CI telemetry: duration=%d checks=%v", pr.CIDurationSeconds, pr.Checks)
			}
			if tt.conclusion == "failure" && (len(pr.SlowChecks) != 1 || pr.SlowChecks[0].Conclusion != "failure") {
				t.Fatalf("failed check telemetry lost: %v", pr.SlowChecks)
			}
		})
	}
}

func TestCheckRunTelemetryExcludesSupersededFailures(t *testing.T) {
	t.Parallel()

	older := time.Date(2026, 7, 16, 18, 0, 0, 0, time.UTC)
	olderDone := older.Add(10 * time.Minute)
	newer := older.Add(time.Minute)
	newerDone := newer.Add(time.Minute)
	summary := checkRunTelemetry([]restCheckRun{
		{ID: 1, Name: "Checks", Status: "completed", Conclusion: "failure", StartedAt: &older, CompletedAt: &olderDone},
		{ID: 2, Name: "Checks", Status: "completed", Conclusion: "success", StartedAt: &newer, CompletedAt: &newerDone},
	}, nil, time.Time{}, defaultUnstartedCheckThreshold)

	if len(summary.SlowChecks) != 1 || summary.SlowChecks[0].Conclusion != "success" {
		t.Fatalf("SlowChecks = %#v, want only latest settled success", summary.SlowChecks)
	}
}

func TestTransientCheckRunFailuresExcludeSupersededFailures(t *testing.T) {
	t.Parallel()

	older := time.Date(2026, 7, 16, 18, 0, 0, 0, time.UTC)
	newer := older.Add(time.Minute)
	checkRuns := []restCheckRun{
		{ID: 1, Name: "Checks", Status: "completed", Conclusion: "failure", StartedAt: &older, Output: checkRunOutput{Text: "signal: killed"}},
		{ID: 2, Name: "Checks", Status: "completed", Conclusion: "success", StartedAt: &newer},
	}
	c := &Connector{}

	got, err := c.transientCheckRunFailures(context.Background(), pullRequestRepo{}, checkRuns)
	if err != nil {
		t.Fatalf("transientCheckRunFailures() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("transientCheckRunFailures() = %#v, want none", got)
	}
}

func TestCheckRunTelemetryUsesWorkflowRunTimingForQueue(t *testing.T) {
	t.Parallel()

	runCreated := time.Date(2026, 6, 5, 10, 58, 0, 0, time.UTC)
	runStarted := runCreated.Add(90 * time.Second)
	checkStarted := time.Date(2026, 6, 5, 11, 0, 0, 0, time.UTC)
	checkCompleted := checkStarted.Add(3 * time.Minute)

	summary := checkRunTelemetry([]restCheckRun{
		{Name: "Verify (ubuntu-latest)", Status: "completed", Conclusion: "success", StartedAt: &checkStarted, CompletedAt: &checkCompleted},
	}, []restWorkflowRun{
		{ID: 28196652213, CreatedAt: &runCreated, RunStartedAt: &runStarted},
	}, time.Time{}, defaultUnstartedCheckThreshold)

	if summary.QueueSeconds != 90 {
		t.Fatalf("QueueSeconds = %d, want 90", summary.QueueSeconds)
	}
	if summary.DurationSeconds != 180 {
		t.Fatalf("DurationSeconds = %d, want 180", summary.DurationSeconds)
	}
}

func TestRequiredStatusCheckFailures(t *testing.T) {
	t.Parallel()

	staleCompleted := time.Date(2026, 7, 7, 0, 49, 24, 0, time.UTC)
	tests := []struct {
		name        string
		localStatus string
		checkRuns   []restCheckRun
		statuses    []restCommitStatus
		required    []string
		wantState   string
		wantChecks  []connector.PullRequestCheck
	}{
		{
			name:        "missing Detent-owned status is unproduced work",
			wantState:   "success",
			localStatus: "local-gate",
			required:    []string{"local-gate"},
			wantChecks:  []connector.PullRequestCheck{{Name: "local-gate", Status: "missing", Conclusion: "missing"}},
		},
		{
			name:       "missing status without local ownership remains pending",
			required:   []string{"local-gate"},
			wantState:  "pending",
			wantChecks: []connector.PullRequestCheck{{Name: "local-gate", Status: "missing", Conclusion: "missing"}},
		},
		{
			name:        "other missing context still waits with local ownership",
			localStatus: "local-gate",
			required:    []string{"local-gate", "Verify"},
			wantState:   "pending",
			wantChecks:  []connector.PullRequestCheck{{Name: "local-gate", Status: "missing", Conclusion: "missing"}, {Name: "Verify", Status: "missing", Conclusion: "missing"}},
		},
		{
			name:        "posted local pending status still waits",
			localStatus: "local-gate",
			required:    []string{"local-gate"},
			statuses:    []restCommitStatus{{Context: "local-gate", State: "pending"}},
			wantState:   "pending",
			wantChecks:  []connector.PullRequestCheck{{Name: "local-gate", Status: "pending", Conclusion: "pending"}},
		},
		{
			name:        "posted local failure still fails",
			localStatus: "local-gate",
			required:    []string{"local-gate"},
			statuses:    []restCommitStatus{{Context: "local-gate", State: "failure"}},
			wantState:   "failure",
			wantChecks:  []connector.PullRequestCheck{{Name: "local-gate", Status: "failure", Conclusion: "failure"}},
		},
		{
			name:      "all required check runs succeeded",
			checkRuns: []restCheckRun{{Name: "Lint", Status: "completed", Conclusion: "success"}},
			required:  []string{"Lint"},
		},
		{
			name: "stale successful required check run succeeded",
			checkRuns: []restCheckRun{{
				Name:        "Installer Smoke (ubuntu-latest)",
				Status:      "in_progress",
				Conclusion:  "success",
				CompletedAt: &staleCompleted,
			}},
			required: []string{"Installer Smoke (ubuntu-latest)"},
		},
		{
			name:      "missing required check blocks as pending",
			checkRuns: []restCheckRun{{Name: "Lint", Status: "completed", Conclusion: "success"}},
			required:  []string{"Lint", "Windows Core"},
			wantState: "pending",
			wantChecks: []connector.PullRequestCheck{{
				Name:       "Windows Core",
				Status:     "missing",
				Conclusion: "missing",
			}},
		},
		{
			name:      "running required check blocks as pending",
			checkRuns: []restCheckRun{{Name: "Windows Core", Status: "in_progress"}},
			required:  []string{"Windows Core"},
			wantState: "pending",
			wantChecks: []connector.PullRequestCheck{{
				Name:   "Windows Core",
				Status: "in_progress",
			}},
		},
		{
			name:      "skipped required check remains pending",
			checkRuns: []restCheckRun{{Name: "Windows Core", Status: "completed", Conclusion: "skipped"}},
			required:  []string{"Windows Core"},
			wantState: "pending",
			wantChecks: []connector.PullRequestCheck{{
				Name:   "Windows Core",
				Status: "pending",
			}},
		},
		{
			name:      "skipped required check overrides successful legacy status",
			checkRuns: []restCheckRun{{Name: "Windows Core", Status: "completed", Conclusion: "skipped"}},
			statuses:  []restCommitStatus{{Context: "Windows Core", State: "success"}},
			required:  []string{"Windows Core"},
			wantState: "pending",
			wantChecks: []connector.PullRequestCheck{{
				Name:   "Windows Core",
				Status: "pending",
			}},
		},
		{
			name: "settled required failure wins over pending check",
			checkRuns: []restCheckRun{
				{Name: "Checks", Status: "completed", Conclusion: "failure"},
				{Name: "Test", Status: "in_progress"},
			},
			required:  []string{"Checks", "Test"},
			wantState: "failure",
			wantChecks: []connector.PullRequestCheck{
				{Name: "Checks", Status: "completed", Conclusion: "failure"},
				{Name: "Test", Status: "in_progress"},
			},
		},
		{
			name: "latest skipped required run blocks despite older success",
			checkRuns: []restCheckRun{
				{ID: 1, Name: "Checks", Status: "completed", Conclusion: "success"},
				{ID: 2, Name: "Checks", Status: "completed", Conclusion: "skipped"},
			},
			required:  []string{"Checks"},
			wantState: "pending",
			wantChecks: []connector.PullRequestCheck{{
				Name:   "Checks",
				Status: "pending",
			}},
		},
		{
			name: "latest success replaces older ignored required runs",
			checkRuns: []restCheckRun{
				{ID: 1, Name: "Checks", Status: "completed", Conclusion: "cancelled"},
				{ID: 2, Name: "Checks", Status: "completed", Conclusion: "skipped"},
				{ID: 3, Name: "Checks", Status: "completed", Conclusion: "success"},
			},
			required: []string{"Checks"},
		},
		{
			name: "latest settled result wins regardless of response order",
			checkRuns: []restCheckRun{
				{ID: 1, Name: "Checks", Status: "completed", Conclusion: "failure", StartedAt: new(time.Date(2026, 7, 16, 18, 0, 0, 0, time.UTC))},
				{ID: 2, Name: "Checks", Status: "completed", Conclusion: "success", StartedAt: new(time.Date(2026, 7, 16, 18, 1, 0, 0, time.UTC))},
			},
			required: []string{"Checks"},
		},
		{
			name:      "neutral required check fails",
			checkRuns: []restCheckRun{{Name: "GoReleaser Snapshot", Status: "completed", Conclusion: "neutral"}},
			required:  []string{"GoReleaser Snapshot"},
			wantState: "failure",
			wantChecks: []connector.PullRequestCheck{{
				Name:       "GoReleaser Snapshot",
				Status:     "completed",
				Conclusion: "neutral",
			}},
		},
		{
			name:      "required commit status context fails",
			statuses:  []restCommitStatus{{Context: "release/check", State: "failure"}},
			required:  []string{"release/check"},
			wantState: "failure",
			wantChecks: []connector.PullRequestCheck{{
				Name:       "release/check",
				Status:     "failure",
				Conclusion: "failure",
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotChecks := requiredStatusCheckFailures(tt.checkRuns, tt.statuses, tt.required)
			if !reflect.DeepEqual(gotChecks, tt.wantChecks) {
				t.Fatalf("requiredStatusCheckFailures() = %#v, want %#v", gotChecks, tt.wantChecks)
			}
			if gotState := requiredStatusCheckState(gotChecks, tt.localStatus); gotState != tt.wantState {
				t.Fatalf("requiredStatusCheckState() = %q, want %q", gotState, tt.wantState)
			}
		})
	}
}

func TestCheckRunWorkflowRunIDs(t *testing.T) {
	t.Parallel()

	got := checkRunWorkflowRunIDs([]restCheckRun{
		{DetailsURL: "https://github.com/digitaldrywood/detent/actions/runs/28196652213/job/83525095026"},
		{DetailsURL: "https://github.com/digitaldrywood/detent/actions/runs/28196652213/job/83525095027"},
		{DetailsURL: "https://github.com/digitaldrywood/detent/actions/runs/28196652214"},
		{DetailsURL: "https://example.com/not-actions"},
	})

	want := []int64{28196652213, 28196652214}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checkRunWorkflowRunIDs() = %#v, want %#v", got, want)
	}
}

func TestConnectorPullRequestStatusCacheDebugLog(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	var logs bytes.Buffer
	c := &Connector{
		pullRequests: newPullRequestStatusCache(time.Minute, func() time.Time { return now }),
		logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})),
	}
	repo := pullRequestRepo{Owner: "digitaldrywood", Name: "detent"}
	status := pullRequestStatus{
		ci: pullRequestCI{
			State:         "SUCCESS",
			CheckRunCount: 2,
		},
	}
	c.pullRequests.Set(repo, 726, "head-sha", status)
	pullRequest := pullRequestNode{Number: 726, HeadSHA: "head-sha"}

	if err := c.populatePullRequestStatus(context.Background(), repo, &pullRequest, true); err != nil {
		t.Fatalf("populatePullRequestStatus() error = %v", err)
	}

	logText := logs.String()
	for _, fragment := range []string{
		"github pull request status cache",
		"endpoint_family=pull_request_status_cache",
		"request_purpose=hydrate_pull_request_status",
		"repository=digitaldrywood/detent",
		"pr_number=726",
		"cache_hit=true",
		"avoidable_request=true",
	} {
		if !strings.Contains(logText, fragment) {
			t.Fatalf("logs missing %q:\n%s", fragment, logText)
		}
	}
	if pullRequest.CI.CheckRunCount != 2 {
		t.Fatalf("CheckRunCount = %d, want cached status", pullRequest.CI.CheckRunCount)
	}
}

func TestConnectorPullRequestStatusCacheRefreshesEditedCodexSummary(t *testing.T) {
	t.Parallel()

	const headSHA = "79f9eb2d4ad5317af5ff46f29aba3b91f4b413a0"
	completedBody := testCodexReviewSummaryBody(
		"✅ **Completed** <relative-time datetime=\"2026-09-02T22:35:58.561318Z\">2026-09-02T22:35:58.561318Z</relative-time>",
		headSHA[:7],
	)
	incompleteBody := testCodexReviewSummaryBody("🔄 **In progress**", headSHA[:7])
	commentResponse := func(body string, updatedAt string) string {
		return fmt.Sprintf(`[{"id":1,"body":%q,"html_url":"https://github.test/pull/389#issuecomment-1","user":{"login":"chatgpt-codex-connector[bot]","type":"Bot"},"created_at":"2026-09-02T16:27:27Z","updated_at":%q}]`, body, updatedAt)
	}
	statusResponses := func(commentBody string, commentETag string) []graphqlTestResponse {
		return []graphqlTestResponse{
			{method: http.MethodGet, path: "/repos/gopherguides/gopher-ai/commits/" + headSHA + "/check-runs?per_page=100", body: `{"check_runs":[{"status":"completed","conclusion":"success"}]}`},
			{method: http.MethodGet, path: "/repos/gopherguides/gopher-ai/commits/" + headSHA + "/statuses?per_page=100", body: `[]`},
			{method: http.MethodGet, path: "/repos/gopherguides/gopher-ai/pulls/389/reviews?per_page=100", body: `[]`},
			{method: http.MethodGet, path: "/repos/gopherguides/gopher-ai/issues/389/comments?per_page=100", headers: map[string]string{"ETag": commentETag}, body: commentBody},
		}
	}
	responses := statusResponses(commentResponse(completedBody, "2026-09-02T22:35:59Z"), `"summary-v1"`)
	responses = append(responses, statusResponses(commentResponse(incompleteBody, "2026-09-02T22:40:00Z"), `"summary-v2"`)...)
	server := newGraphQLTestServer(t, responses)

	now := time.Date(2026, 9, 2, 22, 36, 0, 0, time.UTC)
	c := newGitHubTestConnector(t, server, Config{Now: func() time.Time { return now }})
	repo := pullRequestRepo{Owner: "gopherguides", Name: "gopher-ai"}

	first := pullRequestNode{Number: 389, HeadSHA: headSHA}
	if err := c.populatePullRequestStatus(context.Background(), repo, &first, true); err != nil {
		t.Fatalf("first populatePullRequestStatus() error = %v", err)
	}
	if got := pullRequestCodexReviewSource(first); got != connector.PullRequestReviewSourceSummaryComment {
		t.Fatalf("first review source = %q, want summary comment", got)
	}

	cached := pullRequestNode{Number: 389, HeadSHA: headSHA}
	if err := c.populatePullRequestStatus(context.Background(), repo, &cached, true); err != nil {
		t.Fatalf("cached populatePullRequestStatus() error = %v", err)
	}
	if got := pullRequestCodexReviewState(cached); got != "COMMENTED" {
		t.Fatalf("cached review state = %q, want COMMENTED", got)
	}

	now = now.Add(githubCacheTTL + time.Second)
	refreshed := pullRequestNode{Number: 389, HeadSHA: headSHA}
	if err := c.populatePullRequestStatus(context.Background(), repo, &refreshed, true); err != nil {
		t.Fatalf("refreshed populatePullRequestStatus() error = %v", err)
	}
	if got := pullRequestCodexReviewState(refreshed); got != "" {
		t.Fatalf("refreshed review state = %q, want incomplete edit to invalidate evidence", got)
	}

	commentRequests := 0
	secondCommentETag := ""
	for _, request := range server.requests() {
		if request["path"] == "/repos/gopherguides/gopher-ai/issues/389/comments?per_page=100" {
			commentRequests++
			if commentRequests == 2 {
				secondCommentETag, _ = request["if_none_match"].(string)
			}
		}
	}
	if commentRequests != 2 {
		t.Fatalf("comment requests = %d, want one initial request and one post-expiry refresh", commentRequests)
	}
	if secondCommentETag != `"summary-v1"` {
		t.Fatalf("second comment If-None-Match = %q, want cached summary ETag", secondCommentETag)
	}
}

func TestConnectorFetchPullRequestCommentsMapsRESTMetadata(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		method: http.MethodGet,
		path:   "/repos/example/repo/issues/42/comments?per_page=100",
		body:   `[{"id":202,"node_id":"IC_pr","body":"Review note","html_url":"https://github.com/example/repo/pull/42#issuecomment-202","user":{"login":"reviewer"},"created_at":"2026-07-06T13:00:00Z","updated_at":"2026-07-06T13:10:00Z"}]`,
	}})
	c := newGitHubTestConnector(t, server, Config{})

	got, err := c.FetchPullRequestComments(context.Background(), "example/repo", 42)
	if err != nil {
		t.Fatalf("FetchPullRequestComments() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchPullRequestComments() len = %d, want 1", len(got))
	}

	comment := got[0]
	if comment.ID != "IC_pr" ||
		comment.Backend != connector.BackendGitHub.String() ||
		comment.Body != "Review note" ||
		comment.URL != "https://github.com/example/repo/pull/42#issuecomment-202" ||
		comment.AuthorLogin != "reviewer" ||
		comment.Local ||
		comment.TargetType != connector.IssueCommentTargetPullRequest {
		t.Fatalf("FetchPullRequestComments()[0] = %#v, want normalized GitHub PR metadata", comment)
	}
	createdAt := time.Date(2026, 7, 6, 13, 0, 0, 0, time.UTC)
	updatedAt := time.Date(2026, 7, 6, 13, 10, 0, 0, time.UTC)
	if comment.CreatedAt == nil || !comment.CreatedAt.Equal(createdAt) {
		t.Fatalf("CreatedAt = %v, want %v", comment.CreatedAt, createdAt)
	}
	if comment.UpdatedAt == nil || !comment.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("UpdatedAt = %v, want %v", comment.UpdatedAt, updatedAt)
	}
}

func TestConnectorCreatePullRequestCommentUsesIssueCommentsEndpoint(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		method: http.MethodPost,
		path:   "/repos/example/repo/issues/42/comments",
		body:   `{"node_id":"IC_pr"}`,
	}})
	c := newGitHubTestConnector(t, server, Config{})

	if err := c.CreatePullRequestComment(context.Background(), "example/repo", 42, "ship it"); err != nil {
		t.Fatalf("CreatePullRequestComment() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(requests))
	}
	body := requests[0]["body"].(map[string]any)
	if body["body"] != "ship it" {
		t.Fatalf("body = %v, want ship it", body["body"])
	}
}

func TestConnectorCreateDraftPullRequestUsesRepositoryDefaultBranch(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/repos/example/repo",
			body:   `{"id":1,"full_name":"example/repo","default_branch":"trunk"}`,
		},
		{
			method: http.MethodPost,
			path:   "/repos/example/repo/pulls",
			body:   `{"node_id":"PR_kw1","number":42,"html_url":"https://github.com/example/repo/pull/42","state":"open","draft":true,"head":{"ref":"detent/example_7","sha":"head-sha"},"base":{"ref":"trunk","sha":"base-sha"}}`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{})

	pullRequest, err := c.CreateDraftPullRequest(t.Context(), "example/repo", "detent/example_7", " Recover work ", " provenance ")
	if err != nil {
		t.Fatalf("CreateDraftPullRequest() error = %v", err)
	}
	if pullRequest.Number != 42 || !pullRequest.Draft || pullRequest.State != "OPEN" || pullRequest.BranchName != "detent/example_7" || pullRequest.HeadSHA != "head-sha" {
		t.Fatalf("CreateDraftPullRequest() = %#v", pullRequest)
	}

	requests := server.requests()
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	body := requests[1]["body"].(map[string]any)
	if body["title"] != "Recover work" || body["head"] != "detent/example_7" || body["base"] != "trunk" || body["body"] != "provenance" || body["draft"] != true {
		t.Fatalf("request body = %#v", body)
	}
}

func TestConnectorMergePullRequestClassifiesBaseRefusal(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		status  int
		message string
		want    bool
		body    string
		quota   bool
	}{
		{name: "strict protection", status: 405, message: "Head branch is out of date. Review and try the merge again.", want: true},
		{name: "base race", status: 405, message: "Base branch was modified. Review and try the merge again.", want: true},
		{name: "required failure", status: 405, message: "Required status check Test is failing."},
		{name: "native queue", status: 405, message: "Pull request must be merged using the merge queue."},
		{name: "conflict", status: 405, message: "Pull Request is not mergeable"},
		{name: "changed head", status: 409, message: "Head branch is out of date."},
		{name: "permission", status: 403, message: "Head branch is out of date."},
		{name: "transient", status: 502, message: "Head branch is out of date."},
		{name: "metadata cannot supply base refusal", status: 405, body: `{"message":"Required status check Test is failing.","documentation_url":"Base branch was modified"}`},
		{name: "required check mentioning base refusal", status: 405, message: "Required status check 'Base branch was modified' has not passed."},
		{name: "closure mentioning base refusal", status: 405, message: "Pull Request is closed. Base branch was modified."},
		{name: "malformed JSON cannot supply base refusal", status: 405, body: `{"message":"Base branch was modified"`},
		{name: "quota precedes base refusal", status: 403, message: "Base branch was modified", quota: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body, err := json.Marshal(map[string]string{"message": tt.message})
			if err != nil {
				t.Fatal(err)
			}
			if tt.body != "" {
				body = []byte(tt.body)
			}
			reset := time.Now().Add(time.Hour).Truncate(time.Second)
			c, err := NewConnector(Config{Endpoint: "https://merge-refusal.test/graphql", TokenSource: StaticTokenSource(tt.name), HTTPClient: recoveryHTTPClient(func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodPut || request.URL.Path != "/repos/example/repo/pulls/42/merge" {
					t.Fatalf("unexpected merge request %s %s", request.Method, request.URL)
				}
				headers := make(http.Header)
				if tt.quota {
					headers.Set("X-RateLimit-Remaining", "0")
					headers.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
				}
				return &http.Response{StatusCode: tt.status, Header: headers, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			err = c.MergePullRequest(t.Context(), "example/repo", 42, "checked-head", "squash")
			if errors.Is(err, connector.ErrPullRequestBaseOutOfDate) != tt.want {
				t.Fatalf("error = %v, want base refusal %t", err, tt.want)
			}
			var status *StatusError
			if !errors.As(err, &status) || status.StatusCode != tt.status {
				t.Fatalf("lost original status: %v", err)
			}
			if errors.Is(err, ErrRateLimited) != tt.quota || tt.quota && !status.ResetAt.Equal(reset) {
				t.Fatalf("lost quota precedence or reset: %v", err)
			}
			if tt.want && !strings.Contains(err.Error(), "PUT /repos/example/repo/pulls/42/merge") {
				t.Fatalf("lost merge endpoint identity: %v", err)
			}
		})
	}
}

func TestConnectorMergePullRequestUsesConfiguredMethod(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		method       string
		want         string
		wantErr      string
		wantRequests int
	}{
		{name: "default", want: "squash", wantRequests: 1},
		{name: "squash", method: "squash", want: "squash", wantRequests: 1},
		{name: "merge", method: "merge", want: "merge", wantRequests: 1},
		{name: "rebase", method: "rebase", want: "rebase", wantRequests: 1},
		{name: "normalizes case and whitespace", method: " ReBaSe ", want: "rebase", wantRequests: 1},
		{name: "rejects invalid method", method: "octopus", wantErr: "merge method must be one of squash, merge, rebase"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := newGraphQLTestServer(t, []graphqlTestResponse{{
				method: http.MethodPut,
				path:   "/repos/example/repo/pulls/42/merge",
				body:   `{"sha":"merge-sha","merged":true,"message":"Pull Request successfully merged"}`,
			}})
			c := newGitHubTestConnector(t, server, Config{})

			err := c.MergePullRequest(context.Background(), "example/repo", 42, "head-sha", tt.method)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("MergePullRequest() error = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("MergePullRequest() error = %v", err)
			}

			requests := server.requests()
			if got := len(requests); got != tt.wantRequests {
				t.Fatalf("request count = %d, want %d", got, tt.wantRequests)
			}
			if tt.wantRequests == 0 {
				return
			}
			if requests[0]["method"] != http.MethodPut || requests[0]["path"] != "/repos/example/repo/pulls/42/merge" {
				t.Fatalf("merge request = %#v, want REST pull request merge", requests[0])
			}
			body := requests[0]["body"].(map[string]any)
			if body["merge_method"] != tt.want || body["sha"] != "head-sha" {
				t.Fatalf("merge body = %#v, want %s merge with head sha", body, tt.want)
			}
		})
	}
}

func TestConnectorReapplyPullRequestLabelRemovesAndAddsExistingLabel(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/issues/42",
			body:   `{"node_id":"PR_42","number":42,"labels":[{"name":"bug"},{"name":"ci:ready"}]}`,
		},
		{
			method: http.MethodDelete,
			path:   "/repos/example/repo/issues/42/labels/ci:ready",
			body:   `[{"name":"bug"}]`,
		},
		{
			method: http.MethodPost,
			path:   "/repos/example/repo/issues/42/labels",
			body:   `[{"name":"bug"},{"name":"ci:ready"}]`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{})
	c.triggerLabelDir = t.TempDir()

	if err := c.ReapplyPullRequestLabel(context.Background(), "example/repo", 42, "ci:ready", 15*time.Second); err != nil {
		t.Fatalf("ReapplyPullRequestLabel() error = %v", err)
	}

	requests := server.requests()
	if got := len(requests); got != 3 {
		t.Fatalf("request count = %d, want 3", got)
	}
	if requests[1]["method"] != http.MethodDelete || requests[2]["method"] != http.MethodPost {
		t.Fatalf("requests = %#v, want GET, DELETE, POST", requests)
	}
	body := requests[2]["body"].(map[string]any)
	if got := body["labels"]; !reflect.DeepEqual(got, []any{"ci:ready"}) {
		t.Fatalf("labels body = %#v, want ci:ready", got)
	}
}

func TestConnectorHydratePullRequestRefreshesCurrentStatus(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/pulls/42",
			body:   `{"node_id":"PR_42","number":42,"html_url":"https://github.com/example/repo/pull/42","state":"open","mergeable_state":"clean","draft":false,"labels":[{"name":"Ready to Merge"},{"name":"bug"}],"head":{"ref":"detent/example_repo_1","sha":"head-sha"},"base":{"ref":"main","sha":"base-sha"},"updated_at":"2026-06-26T13:00:00Z"}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/commits/head-sha/check-runs?per_page=100",
			body:   `{"check_runs":[{"name":"Verify","status":"completed","conclusion":"success"}]}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/commits/head-sha/statuses?per_page=100",
			body:   `[]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/pulls/42/reviews?per_page=100",
			body:   `[]`,
		},
		emptyPullRequestCommentsResponse("example/repo", 42),
	})
	c := newGitHubTestConnector(t, server, Config{})
	prNumber := 42
	issue := connector.Issue{
		ID:         "I_kw42",
		Identifier: "example/repo#1",
		PRNumber:   &prNumber,
	}

	got, err := c.HydratePullRequest(context.Background(), issue)
	if err != nil {
		t.Fatalf("HydratePullRequest() error = %v", err)
	}

	if got.PullRequest == nil {
		t.Fatalf("HydratePullRequest().PullRequest = nil, want hydrated pull request")
	}
	pr := got.PullRequest
	if pr.NodeID != "PR_42" || pr.Number != 42 || pr.State != "OPEN" || pr.MergeableState != "clean" || pr.HeadSHA != "head-sha" || pr.BaseRef != "main" || pr.BaseSHA != "base-sha" {
		t.Fatalf("hydrated pull request = %#v, want current clean pull request details", pr)
	}
	if pr.CIStatus != "pass" || pr.CheckRunCount != 1 {
		t.Fatalf("hydrated CI = status %q check runs %d, want pass with one check run", pr.CIStatus, pr.CheckRunCount)
	}
	if !reflect.DeepEqual(pr.Labels, []string{"ready to merge", "bug"}) {
		t.Fatalf("hydrated labels = %#v, want ready to merge and bug", pr.Labels)
	}
	if got.PRRepository != "example/repo" {
		t.Fatalf("PRRepository = %q, want example/repo", got.PRRepository)
	}
}

func TestConnectorFetchPullRequestReviewThreads(t *testing.T) {
	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodPost,
			path:   "/",
			body:   `{"data":{"repository":{"pullRequest":{"headRefOid":"head-sha","reviewThreads":{"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"},"nodes":[{"isResolved":false,"path":"internal/orchestrator/autopromote.go","line":181,"originalLine":180,"comments":{"nodes":[{"body":"Preserve cancellation evidence."}]}},{"isResolved":true,"path":"internal/connector/issue.go","line":107,"originalLine":107}]}}}}}`,
		},
		{
			method: http.MethodPost,
			path:   "/",
			body:   `{"data":{"repository":{"pullRequest":{"headRefOid":"head-sha","reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"isResolved":false,"isOutdated":true,"path":"internal/connector/github/pull_requests.go","line":null,"originalLine":1092}]}}}}}`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{})

	got, err := c.fetchPullRequestReviewThreads(t.Context(), pullRequestRepo{Owner: "example", Name: "repo"}, 42, "head-sha")
	if err != nil {
		t.Fatalf("fetchPullRequestReviewThreads() error = %v", err)
	}
	want := []connector.PullRequestReviewThread{
		{Path: "internal/orchestrator/autopromote.go", Line: 181, Body: "Preserve cancellation evidence."},
		{Path: "internal/connector/github/pull_requests.go", Line: 1092},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fetchPullRequestReviewThreads() = %#v, want %#v", got, want)
	}

	requests := server.requests()
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if variables := requests[1]["variables"].(map[string]any); variables["after"] != "cursor-1" {
		t.Fatalf("second request after = %v, want cursor-1", variables["after"])
	}
}

func TestConnectorHydratePullRequestReviewThreadsRefreshesMutableState(t *testing.T) {
	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodPost,
			path:   "/",
			body:   `{"data":{"repository":{"pullRequest":{"headRefOid":"head-one","reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}}`,
		},
		{
			method: http.MethodPost,
			path:   "/",
			body:   `{"data":{"repository":{"pullRequest":{"headRefOid":"head-one","reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"isResolved":false,"path":"internal/orchestrator/autopromote.go","line":181,"originalLine":181}]}}}}}`,
		},
		{
			method: http.MethodPost,
			path:   "/",
			body:   `{"data":{"repository":{"pullRequest":{"headRefOid":"head-two","reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}}`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{})
	repo := pullRequestRepo{Owner: "example", Name: "repo"}
	c.pullRequests.Set(repo, 42, "head-one", pullRequestStatus{ci: pullRequestCI{State: "SUCCESS"}})
	c.pullRequests.Set(repo, 42, "head-two", pullRequestStatus{ci: pullRequestCI{State: "SUCCESS"}})
	issue := connector.Issue{
		Identifier:   "example/repo#41",
		PRRepository: "example/repo",
		PullRequest: &connector.PullRequest{
			Number:  42,
			State:   "OPEN",
			HeadSHA: "head-one",
		},
	}

	first, err := c.HydratePullRequestReviewThreads(t.Context(), issue)
	if err != nil {
		t.Fatalf("first HydratePullRequestReviewThreads() error = %v", err)
	}
	second, err := c.HydratePullRequestReviewThreads(t.Context(), issue)
	if err != nil {
		t.Fatalf("second HydratePullRequestReviewThreads() error = %v", err)
	}
	if len(first.PullRequest.UnresolvedReviewThreads) != 0 {
		t.Fatalf("first unresolved review threads = %#v, want none", first.PullRequest.UnresolvedReviewThreads)
	}
	want := []connector.PullRequestReviewThread{{Path: "internal/orchestrator/autopromote.go", Line: 181}}
	if !reflect.DeepEqual(second.PullRequest.UnresolvedReviewThreads, want) {
		t.Fatalf("second unresolved review threads = %#v, want %#v", second.PullRequest.UnresolvedReviewThreads, want)
	}

	changedHead := issue
	pullRequest := *issue.PullRequest
	pullRequest.HeadSHA = "head-two"
	changedHead.PullRequest = &pullRequest
	resolved, err := c.HydratePullRequestReviewThreads(t.Context(), changedHead)
	if err != nil {
		t.Fatalf("changed-head HydratePullRequestReviewThreads() error = %v", err)
	}
	if len(resolved.PullRequest.UnresolvedReviewThreads) != 0 {
		t.Fatalf("changed-head unresolved review threads = %#v, want none", resolved.PullRequest.UnresolvedReviewThreads)
	}
	if got := len(server.requests()); got != 3 {
		t.Fatalf("request count = %d, want one fresh query per hydration", got)
	}
	for _, headSHA := range []string{"head-one", "head-two"} {
		status, ok := c.pullRequests.Get(repo, 42, headSHA)
		if !ok || status.ci.State != "SUCCESS" {
			t.Fatalf("cached status for %s = %#v, %t", headSHA, status, ok)
		}
	}
}

func TestConnectorFetchPullRequestReviewThreadsRejectsChangedHead(t *testing.T) {
	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		method: http.MethodPost,
		path:   "/",
		body:   `{"data":{"repository":{"pullRequest":{"headRefOid":"new-head","reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}}`,
	}})
	c := newGitHubTestConnector(t, server, Config{})

	_, err := c.fetchPullRequestReviewThreads(t.Context(), pullRequestRepo{Owner: "example", Name: "repo"}, 42, "old-head")
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("fetchPullRequestReviewThreads() error = %v, want ErrInvalidResponse", err)
	}
}

func TestConnectorLookupPullRequestByHeadUsesExactCurrentHead(t *testing.T) {
	t.Parallel()

	const (
		branch  = "detent/acme_widgets_18"
		headSHA = "current-head"
		path    = "/repos/example/repo/pulls?direction=desc&head=example%3Adetent%2Facme_widgets_18&per_page=100&sort=updated&state=all"
	)
	tests := []struct {
		name      string
		body      string
		wantFound bool
		wantState string
	}{
		{
			name:      "open exact head",
			body:      `[{"node_id":"PR_42","number":42,"html_url":"https://github.com/example/repo/pull/42","state":"open","head":{"ref":"detent/acme_widgets_18","sha":"current-head"},"base":{"ref":"main","sha":"base-head"}}]`,
			wantFound: true,
			wantState: "OPEN",
		},
		{
			name:      "merged exact head",
			body:      `[{"node_id":"PR_42","number":42,"html_url":"https://github.com/example/repo/pull/42","state":"closed","merged_at":"2026-08-12T18:00:00Z","head":{"ref":"detent/acme_widgets_18","sha":"current-head"},"base":{"ref":"main","sha":"base-head"}}]`,
			wantFound: true,
			wantState: "MERGED",
		},
		{
			name:      "closed unmerged exact head",
			body:      `[{"node_id":"PR_42","number":42,"html_url":"https://github.com/example/repo/pull/42","state":"closed","head":{"ref":"detent/acme_widgets_18","sha":"current-head"},"base":{"ref":"main","sha":"base-head"}}]`,
			wantFound: true,
			wantState: "CLOSED",
		},
		{
			name: "stale head does not match",
			body: `[{"node_id":"PR_42","number":42,"html_url":"https://github.com/example/repo/pull/42","state":"open","head":{"ref":"detent/acme_widgets_18","sha":"stale-head"},"base":{"ref":"main","sha":"base-head"}}]`,
		},
		{
			name: "no pull request",
			body: `[]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := newGraphQLTestServer(t, []graphqlTestResponse{{
				method: http.MethodGet,
				path:   path,
				body:   tt.body,
			}})
			c := newGitHubTestConnector(t, server, Config{})

			pullRequest, found, err := c.LookupPullRequestByHead(t.Context(), "example/repo", branch, headSHA)
			if err != nil {
				t.Fatalf("LookupPullRequestByHead() error = %v", err)
			}
			if found != tt.wantFound {
				t.Fatalf("LookupPullRequestByHead() = %#v, found = %v, want %v", pullRequest, found, tt.wantFound)
			}
			if !tt.wantFound {
				return
			}
			if pullRequest.Number != 42 || pullRequest.State != tt.wantState || pullRequest.BranchName != branch || pullRequest.HeadSHA != headSHA {
				t.Fatalf("LookupPullRequestByHead() = %#v, want PR 42 state %s on exact head", pullRequest, tt.wantState)
			}
		})
	}
}

func TestConnectorPullRequestDiffFingerprintIsContentStableAndCached(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		method: http.MethodGet,
		path:   "/repos/example/repo/pulls/42/files?per_page=100",
		body:   `[{"filename":"z.go","status":"modified","sha":"blob-z"},{"filename":"a.go","previous_filename":"old.go","status":"renamed","sha":"blob-a"}]`,
	}})
	c := newGitHubTestConnector(t, server, Config{})
	prNumber := 42
	issue := connector.Issue{
		Identifier:   "example/repo#1",
		PRNumber:     &prNumber,
		PRRepository: "example/repo",
		PullRequest: &connector.PullRequest{
			Number:  prNumber,
			HeadSHA: "head-sha",
			BaseSHA: "base-sha",
		},
	}

	first, err := c.PullRequestDiffFingerprint(context.Background(), issue)
	if err != nil {
		t.Fatalf("PullRequestDiffFingerprint() error = %v", err)
	}
	second, err := c.PullRequestDiffFingerprint(context.Background(), issue)
	if err != nil {
		t.Fatalf("cached PullRequestDiffFingerprint() error = %v", err)
	}
	if first == "" || second != first {
		t.Fatalf("fingerprints = %q and %q, want identical non-empty values", first, second)
	}

	reordered := pullRequestDiffFingerprint([]restPullRequestFile{
		{Filename: "a.go", PreviousFilename: "old.go", Status: "renamed", SHA: "blob-a"},
		{Filename: "z.go", Status: "modified", SHA: "blob-z"},
	})
	if reordered != first {
		t.Fatalf("reordered fingerprint = %q, want %q", reordered, first)
	}
	changed := pullRequestDiffFingerprint([]restPullRequestFile{
		{Filename: "a.go", PreviousFilename: "old.go", Status: "renamed", SHA: "blob-a-changed"},
		{Filename: "z.go", Status: "modified", SHA: "blob-z"},
	})
	if changed == first {
		t.Fatalf("changed fingerprint = %q, want a new value", changed)
	}
}

func TestPullRequestValidationDiffConcurrentPRs(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	t.Parallel()
	type fixture struct {
		base, head string
		files      []string
		contents   []string
		patch      string
	}
	fixtures := map[int]fixture{
		155: {base: "d8970cb2b42f2463a6f10f9f2b88151dd4b5d6c1", head: "236813f76fa510ff22d778fd1fd341f390cf15da", files: []string{"AGENTS.md", "README.md"}, contents: []string{"bench origin", "bench origin"}},
		156: {base: "d8970cb2b42f2463a6f10f9f2b88151dd4b5d6c1", head: "178c860585360bf7056e76ebfbc6a68fe08afc44", files: []string{"WORKFLOW.md", "detent.yaml", "docs/detent-rework-cost.md"}, contents: []string{"workflow", "agent config", "rework cost"}},
		220: {base: "1cc80a5a29fcc4269f89204a167216ae7591d2bd", head: "e5be6db5371bb1ebffeb440d8d27a9d8119026b9", files: []string{"WORKFLOW.md", "ui/build.gradle.kts", "ui/src/jvmTest/kotlin/pro/pyroapex/pos/ui/sales/ReceiptScanComposeTest.kt"}, contents: []string{"stall procedure", "timeout", "cleanup"}},
		190: {base: "9ae69513839167f50336794c361ba0db627d995a", head: "fa96813851dda0908bb27560919dc30c9a5ca96b", files: []string{"core/src/iosTest/kotlin/pro/pyroapex/pos/core/db/IosDatabaseTest.kt"}, contents: []string{"iOS database"}},
	}
	for number, f := range fixtures {
		var patch strings.Builder
		for index, path := range f.files {
			fmt.Fprintf(&patch, "diff --git a/%s b/%s\n@@ -0,0 +1 @@\n+%s\n", path, path, f.contents[index])
		}
		f.patch = patch.String()
		fixtures[number] = f
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var number int
		var filesEndpoint bool
		if strings.HasSuffix(r.URL.Path, "/files") {
			_, _ = fmt.Sscanf(r.URL.Path, "/repos/example/repo/pulls/%d/files", &number)
			filesEndpoint = true
		} else if _, err := fmt.Sscanf(r.URL.Path, "/repos/example/repo/pulls/%d", &number); err != nil {
			http.NotFound(w, r)
			return
		}
		f, ok := fixtures[number]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if filesEndpoint {
			var files []map[string]string
			for index, path := range f.files {
				files = append(files, map[string]string{"filename": path, "patch": "@@ -0,0 +1 @@\n+" + f.contents[index]})
			}
			_ = json.NewEncoder(w).Encode(files)
			return
		}
		if r.Header.Get("Accept") == "application/vnd.github.diff" {
			_, _ = io.WriteString(w, f.patch)
			return
		}
		_, _ = fmt.Fprintf(w, `{"number":%d,"base":{"sha":%q},"head":{"sha":%q}}`, number, f.base, f.head)
	}))
	t.Cleanup(server.Close)
	c, err := NewConnector(Config{Endpoint: server.URL, APIKey: "token", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		number int
		diff   connector.ValidationDiff
		err    error
	}
	results := make(chan outcome, len(fixtures))
	for number, fixture := range fixtures {
		go func() {
			issue := connector.Issue{Identifier: fmt.Sprintf("example/repo#%d", number), PRRepository: "example/repo", PullRequest: &connector.PullRequest{Number: number, BaseSHA: fixture.base, HeadSHA: fixture.head}}
			diff, err := c.PullRequestValidationDiff(t.Context(), issue)
			results <- outcome{number, diff, err}
		}()
	}
	for range fixtures {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		want := fixtures[got.number]
		if !reflect.DeepEqual(got.diff.Files, want.files) || got.diff.Patch != want.patch || got.diff.HeadSHA != want.head || got.diff.PRNumber != got.number {
			t.Fatalf("PR %d snapshot = %#v", got.number, got.diff)
		}
	}
}

func TestPullRequestValidationDiffFetchFailuresAreRetryable(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	t.Parallel()
	for _, tt := range []struct {
		name       string
		failedCall int
	}{
		{name: "initial metadata", failedCall: 1},
		{name: "files", failedCall: 2},
		{name: "patch", failedCall: 3},
		{name: "refreshed metadata", failedCall: 4},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if int(calls.Add(1)) == tt.failedCall {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/files"):
					_, _ = io.WriteString(w, `[{"filename":"AGENTS.md","patch":"@@ -0,0 +1 @@\n+bench origin"}]`)
				case r.Header.Get("Accept") == "application/vnd.github.diff":
					_, _ = io.WriteString(w, "diff --git a/AGENTS.md b/AGENTS.md\n@@ -0,0 +1 @@\n+bench origin\n")
				default:
					_, _ = io.WriteString(w, `{"number":155,"base":{"sha":"base"},"head":{"sha":"head"}}`)
				}
			}))
			t.Cleanup(server.Close)
			c, err := NewConnector(Config{Endpoint: server.URL, APIKey: "token", HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			issue := connector.Issue{Identifier: "example/repo#155", PRRepository: "example/repo",
				PullRequest: &connector.PullRequest{Number: 155, BaseSHA: "base", HeadSHA: "head"}}
			if _, err := c.PullRequestValidationDiff(t.Context(), issue); err == nil || !connector.IsRetryable(err) {
				t.Fatalf("fetch failure = %v, want retryable error", err)
			}
		})
	}
}

func TestPullRequestValidationDiffPreservesFetchFailureClassification(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	t.Parallel()
	for _, failure := range []struct {
		name      string
		status    int
		body      string
		headers   http.Header
		transport error
		want      error
		retryable bool
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, want: ErrAuthenticationFailed},
		{name: "forbidden", status: http.StatusForbidden, want: ErrAuthenticationFailed},
		{name: "not found", status: http.StatusNotFound, want: ErrNotFound},
		{name: "server error", status: http.StatusInternalServerError, want: ErrTransient, retryable: true},
		{name: "timeout", transport: context.DeadlineExceeded, want: ErrTransient, retryable: true},
		{name: "rate limited", status: http.StatusTooManyRequests, want: ErrRateLimited, retryable: true},
		{name: "forbidden rate limited", status: http.StatusForbidden, headers: http.Header{"Retry-After": []string{"60"}}, want: ErrRateLimited, retryable: true},
		{name: "invalid response", status: http.StatusOK, body: "{invalid", want: ErrInvalidResponse},
	} {
		for _, stage := range []struct {
			name string
			call int
		}{
			{name: "initial metadata", call: 1},
			{name: "files", call: 2},
			{name: "patch", call: 3},
			{name: "refreshed metadata", call: 4},
		} {
			if failure.name == "invalid response" && stage.name == "patch" {
				// The patch response is plain text, not JSON.
				continue
			}
			t.Run(failure.name+"/"+stage.name, func(t *testing.T) {
				t.Parallel()
				server := httptest.NewServer(http.NotFoundHandler())
				t.Cleanup(server.Close)
				var calls atomic.Int32
				client := staticHTTPClient{do: func(req *http.Request) (*http.Response, error) {
					if int(calls.Add(1)) == stage.call {
						if failure.transport != nil {
							return nil, failure.transport
						}
						return jsonResponse(req, failure.status, failure.body, failure.headers), nil
					}
					switch {
					case strings.HasSuffix(req.URL.Path, "/files"):
						return jsonResponse(req, http.StatusOK, `[{"filename":"AGENTS.md","patch":"@@ -0,0 +1 @@\n+safe"}]`, nil), nil
					case req.Header.Get("Accept") == "application/vnd.github.diff":
						return jsonResponse(req, http.StatusOK, "diff --git a/AGENTS.md b/AGENTS.md\n@@ -0,0 +1 @@\n+safe\n", nil), nil
					default:
						return jsonResponse(req, http.StatusOK, `{"number":155,"base":{"sha":"base"},"head":{"sha":"head"}}`, nil), nil
					}
				}}
				c, err := NewConnector(Config{Endpoint: server.URL, APIKey: "token", HTTPClient: client})
				if err != nil {
					t.Fatal(err)
				}
				c.client.restBackoffs = newRESTBackoffRegistry()
				issue := connector.Issue{Identifier: "example/repo#155", PRRepository: "example/repo",
					PullRequest: &connector.PullRequest{Number: 155, BaseSHA: "base", HeadSHA: "head"}}
				_, err = c.PullRequestValidationDiff(t.Context(), issue)
				if err == nil || connector.IsRetryable(err) != failure.retryable {
					t.Fatalf("fetch failure = %v, retryable = %t, want %t", err, connector.IsRetryable(err), failure.retryable)
				}
				if !errors.Is(err, failure.want) {
					t.Fatalf("fetch failure = %v, want wrapped %v", err, failure.want)
				}
			})
		}
	}
}

func TestValidationDiffRejectsOtherPRFiles(t *testing.T) {
	t.Parallel()
	patch := "diff --git a/WORKFLOW.md b/WORKFLOW.md\n@@ -0,0 +1 @@\n+workflow\n"
	pr220Patch := "diff --git a/WORKFLOW.md b/WORKFLOW.md\n+stall procedure\ndiff --git a/ui/build.gradle.kts b/ui/build.gradle.kts\n+timeout\ndiff --git a/ui/src/jvmTest/kotlin/pro/pyroapex/pos/ui/sales/ReceiptScanComposeTest.kt b/ui/src/jvmTest/kotlin/pro/pyroapex/pos/ui/sales/ReceiptScanComposeTest.kt\n+cleanup\n"
	for _, tt := range []struct {
		name  string
		files []string
		patch string
		want  bool
	}{
		{name: "matching workflow", files: []string{"WORKFLOW.md"}, patch: patch, want: true},
		{name: "path contains destination marker", files: []string{"docs b/readme.md"}, patch: "diff --git a/docs b/readme.md b/docs b/readme.md\n+docs\n", want: true},
		{name: "ambiguous suffix is not the destination", files: []string{"readme.md"}, patch: "diff --git a/docs b/readme.md b/docs b/readme.md\n+docs\n"},
		{name: "renamed file", files: []string{"new.md"}, patch: "diff --git a/old.md b/new.md\nsimilarity index 100%\nrename from old.md\nrename to new.md\n", want: true},
		{name: "docs PR list with workflow patch", files: []string{"AGENTS.md", "README.md"}, patch: patch},
		{name: "missing file list", files: nil, patch: patch},
		{name: "PR220 receipt files", files: []string{"WORKFLOW.md", "ui/build.gradle.kts", "ui/src/jvmTest/kotlin/pro/pyroapex/pos/ui/sales/ReceiptScanComposeTest.kt"}, patch: pr220Patch, want: true},
		{name: "PR190 iOS files against PR220 patch", files: []string{"core/src/iosTest/kotlin/pro/pyroapex/pos/core/db/IosDatabaseTest.kt"}, patch: pr220Patch},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := sameValidationFiles(tt.files, tt.patch); got != tt.want {
				t.Fatalf("sameValidationFiles(%v) = %t, want %t", tt.files, got, tt.want)
			}
		})
	}
}

func TestConnectorHydratePullRequestNormalizesStaleSuccessfulWorkflowCheckRun(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/pulls/970",
			body:   `{"number":970,"html_url":"https://github.com/example/repo/pull/970","state":"open","mergeable_state":"clean","draft":false,"head":{"ref":"detent/example_repo_970","sha":"head-sha"},"base":{"sha":"base-sha"},"updated_at":"2026-07-07T00:50:00Z"}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/commits/head-sha/check-runs?per_page=100",
			body:   `{"check_runs":[{"id":97001,"name":"Installer Smoke (ubuntu-latest)","status":"in_progress","conclusion":"success","details_url":"https://github.com/example/repo/actions/runs/28833549023/job/97001","started_at":"2026-07-07T00:45:00Z","completed_at":"2026-07-07T00:49:24Z"}]}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/actions/runs/28833549023",
			body:   `{"id":28833549023,"status":"completed","conclusion":"success","created_at":"2026-07-07T00:44:00Z","run_started_at":"2026-07-07T00:45:00Z","updated_at":"2026-07-07T00:49:24Z"}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/commits/head-sha/statuses?per_page=100",
			body:   `[]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/pulls/970/reviews?per_page=100",
			body:   `[]`,
		},
		emptyPullRequestCommentsResponse("example/repo", 970),
	})
	c := newGitHubTestConnector(t, server, Config{
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
	prNumber := 970
	issue := connector.Issue{
		ID:         "I_kw970",
		Identifier: "example/repo#970",
		PRNumber:   &prNumber,
	}

	got, err := c.HydratePullRequest(context.Background(), issue)
	if err != nil {
		t.Fatalf("HydratePullRequest() error = %v", err)
	}

	pr := got.PullRequest
	if pr == nil {
		t.Fatalf("PullRequest = nil, want hydrated pull request")
		return
	}
	if pr.CIStatus != "pass" {
		t.Fatalf("CIStatus = %q, want pass", pr.CIStatus)
	}
	if len(pr.RunningChecks) != 0 {
		t.Fatalf("RunningChecks = %#v, want none", pr.RunningChecks)
	}
	if len(pr.StaleSuccessfulChecks) != 1 || pr.StaleSuccessfulChecks[0].Name != "Installer Smoke (ubuntu-latest)" {
		t.Fatalf("StaleSuccessfulChecks = %#v, want Installer Smoke anomaly", pr.StaleSuccessfulChecks)
	}
	for _, fragment := range []string{
		"stale_successful_check_run",
		"Installer Smoke (ubuntu-latest)",
		"action=normalize_success",
	} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
		}
	}
}

func TestConnectorHydratePullRequestUsesEffectiveCheckRunsForWorkflowTelemetry(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/pulls/971",
			body:   `{"number":971,"html_url":"https://github.com/example/repo/pull/971","state":"open","mergeable_state":"clean","draft":false,"head":{"ref":"detent/example_repo_971","sha":"head-sha"},"base":{"sha":"base-sha"},"updated_at":"2026-07-16T18:10:00Z"}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/commits/head-sha/check-runs?per_page=100",
			body:   `{"check_runs":[{"id":97101,"name":"Verify","status":"completed","conclusion":"cancelled","details_url":"https://github.com/example/repo/actions/runs/7001/job/97101","created_at":"2026-07-16T18:00:00Z","started_at":"2026-07-16T18:01:00Z","completed_at":"2026-07-16T18:02:00Z"},{"id":97102,"name":"Verify","status":"completed","conclusion":"success","details_url":"https://github.com/example/repo/actions/runs/7002/job/97102","created_at":"2026-07-16T18:05:00Z","started_at":"2026-07-16T18:06:00Z","completed_at":"2026-07-16T18:10:00Z"}]}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/actions/runs/7002",
			body:   `{"id":7002,"status":"completed","conclusion":"success","created_at":"2026-07-16T18:05:00Z","run_started_at":"2026-07-16T18:06:00Z","updated_at":"2026-07-16T18:10:00Z"}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/commits/head-sha/statuses?per_page=100",
			body:   `[]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/pulls/971/reviews?per_page=100",
			body:   `[]`,
		},
		emptyPullRequestCommentsResponse("example/repo", 971),
	})
	c := newGitHubTestConnector(t, server, Config{})
	prNumber := 971
	issue := connector.Issue{
		ID:         "I_kw971",
		Identifier: "example/repo#971",
		PRNumber:   &prNumber,
	}

	got, err := c.HydratePullRequest(context.Background(), issue)
	if err != nil {
		t.Fatalf("HydratePullRequest() error = %v", err)
	}

	pr := got.PullRequest
	if pr == nil {
		t.Fatal("PullRequest = nil, want hydrated pull request")
		return
	}
	if pr.CIStatus != "pass" || pr.CIQueueSeconds != 60 || pr.CIDurationSeconds != 240 {
		t.Fatalf("PullRequest CI = %#v, want pass with 60s queue and 240s duration", pr)
	}
	for _, request := range server.requests() {
		if request["path"] == "/repos/example/repo/actions/runs/7001" {
			t.Fatalf("request path = %q, want superseded workflow run excluded", request["path"])
		}
	}
}

func TestConnectorHydratePullRequestIgnoresOptionalSkippedCheckRun(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/repos/example/pyroapex/pulls/1648",
			body:   `{"number":1648,"html_url":"https://github.com/example/pyroapex/pull/1648","state":"open","mergeable_state":"clean","draft":false,"head":{"ref":"detent/1647","sha":"head-sha"},"base":{"sha":"base-sha"}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/pyroapex/commits/head-sha/check-runs?per_page=100",
			body:   `{"check_runs":[{"id":1,"name":"Checks","status":"completed","conclusion":"success"},{"id":2,"name":"Changed-file coverage","status":"completed","conclusion":"success"},{"id":3,"name":"Race Tests","status":"completed","conclusion":"skipped"}]}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/pyroapex/commits/head-sha/statuses?per_page=100",
			body:   `[]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/pyroapex/pulls/1648/reviews?per_page=100",
			body:   `[]`,
		},
		emptyPullRequestCommentsResponse("example/pyroapex", 1648),
	})
	c := newGitHubTestConnector(t, server, Config{})
	prNumber := 1648
	issue := connector.Issue{
		ID:           "I_kw1647",
		Identifier:   "example/pyroapex#1647",
		PRNumber:     &prNumber,
		PRRepository: "example/pyroapex",
	}

	got, err := c.HydratePullRequest(context.Background(), issue)
	if err != nil {
		t.Fatalf("HydratePullRequest() error = %v", err)
	}

	pr := got.PullRequest
	if pr == nil {
		t.Fatal("PullRequest = nil, want hydrated pull request")
		return
	}
	if pr.MergeableState != "clean" || pr.CIStatus != "pass" {
		t.Fatalf("PullRequest = %#v, want clean with passing CI", pr)
	}
	if len(pr.RunningChecks) != 0 {
		t.Fatalf("RunningChecks = %#v, want none", pr.RunningChecks)
	}
}

func TestConnectorHydratePullRequestTreatsSkippedRequiredStatusCheckAsPending(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/pulls/42",
			body:   `{"number":42,"html_url":"https://github.com/example/repo/pull/42","state":"open","mergeable_state":"clean","draft":false,"head":{"ref":"detent/example_repo_1","sha":"head-sha"},"base":{"sha":"base-sha"}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/commits/head-sha/check-runs?per_page=100",
			body:   `{"check_runs":[{"name":"Lint","status":"completed","conclusion":"success"},{"name":"Windows Core","status":"completed","conclusion":"skipped"}]}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/commits/head-sha/statuses?per_page=100",
			body:   `[]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/pulls/42/reviews?per_page=100",
			body:   `[]`,
		},
		emptyPullRequestCommentsResponse("example/repo", 42),
	})
	c := newGitHubTestConnector(t, server, Config{RequiredStatusChecks: []string{"Lint", "Windows Core"}})
	prNumber := 42
	issue := connector.Issue{
		ID:         "I_kw42",
		Identifier: "example/repo#1",
		PRNumber:   &prNumber,
	}

	got, err := c.HydratePullRequest(context.Background(), issue)
	if err != nil {
		t.Fatalf("HydratePullRequest() error = %v", err)
	}

	pr := got.PullRequest
	if pr == nil {
		t.Fatalf("PullRequest = nil, want hydrated pull request")
		return
	}
	if pr.CIStatus != "pending" {
		t.Fatalf("CIStatus = %q, want pending for skipped required check", pr.CIStatus)
	}
	want := []connector.PullRequestCheck{{
		Name:   "Windows Core",
		Status: "pending",
	}}
	if !reflect.DeepEqual(pr.RequiredCheckFailures, want) {
		t.Fatalf("RequiredCheckFailures = %#v, want %#v", pr.RequiredCheckFailures, want)
	}
}

func TestConnectorHydratePullRequestWaitsForRunningRequiredCheckDespiteCancelledShadow(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/pulls/42",
			body:   `{"number":42,"html_url":"https://github.com/example/repo/pull/42","state":"open","mergeable_state":"clean","draft":false,"head":{"ref":"detent/example_repo_1","sha":"head-sha"},"base":{"sha":"base-sha"}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/commits/head-sha/check-runs?per_page=100",
			body:   `{"check_runs":[{"name":"Changed-file coverage","status":"completed","conclusion":"success","started_at":"2026-07-16T18:21:41Z"},{"name":"Changed-file coverage","status":"completed","conclusion":"cancelled","started_at":"2026-07-16T18:20:00Z"},{"name":"Checks","status":"completed","conclusion":"failure","started_at":"2026-07-16T18:00:00Z"},{"id":5,"name":"Checks","status":"in_progress","started_at":"2026-07-16T18:21:40Z"}]}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/commits/head-sha/statuses?per_page=100",
			body:   `[]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/pulls/42/reviews?per_page=100",
			body:   `[]`,
		},
		emptyPullRequestCommentsResponse("example/repo", 42),
	})
	c := newGitHubTestConnector(t, server, Config{RequiredStatusChecks: []string{"Changed-file coverage", "Checks"}})
	prNumber := 42
	issue := connector.Issue{
		ID:         "I_kw42",
		Identifier: "example/repo#1",
		PRNumber:   &prNumber,
	}

	got, err := c.HydratePullRequest(context.Background(), issue)
	if err != nil {
		t.Fatalf("HydratePullRequest() error = %v", err)
	}
	if got.PullRequest == nil {
		t.Fatal("PullRequest = nil, want hydrated pull request")
	}
	if got.PullRequest.CIStatus != "pending" {
		t.Fatalf("CIStatus = %q, want pending", got.PullRequest.CIStatus)
	}
	want := []connector.PullRequestCheck{{
		ID:     5,
		Name:   "Checks",
		Status: "in_progress",
	}}
	if !reflect.DeepEqual(got.PullRequest.RequiredCheckFailures, want) {
		t.Fatalf("RequiredCheckFailures = %#v, want %#v", got.PullRequest.RequiredCheckFailures, want)
	}
}

func TestConnectorHydratePullRequestBlocksMissingRequiredStatusCheck(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/pulls/42",
			body:   `{"number":42,"html_url":"https://github.com/example/repo/pull/42","state":"open","mergeable_state":"clean","draft":false,"head":{"ref":"detent/example_repo_1","sha":"head-sha"},"base":{"sha":"base-sha"}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/commits/head-sha/check-runs?per_page=100",
			body:   `{"check_runs":[{"name":"Lint","status":"completed","conclusion":"success"}]}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/commits/head-sha/statuses?per_page=100",
			body:   `[]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/pulls/42/reviews?per_page=100",
			body:   `[]`,
		},
		emptyPullRequestCommentsResponse("example/repo", 42),
	})
	c := newGitHubTestConnector(t, server, Config{RequiredStatusChecks: []string{"Lint", "Windows Core"}})
	prNumber := 42
	issue := connector.Issue{
		ID:         "I_kw42",
		Identifier: "example/repo#1",
		PRNumber:   &prNumber,
	}

	got, err := c.HydratePullRequest(context.Background(), issue)
	if err != nil {
		t.Fatalf("HydratePullRequest() error = %v", err)
	}

	pr := got.PullRequest
	if pr == nil {
		t.Fatalf("PullRequest = nil, want hydrated pull request")
		return
	}
	if pr.CIStatus != "pending" {
		t.Fatalf("CIStatus = %q, want pending for missing required check", pr.CIStatus)
	}
	want := []connector.PullRequestCheck{{
		Name:       "Windows Core",
		Status:     "missing",
		Conclusion: "missing",
	}}
	if !reflect.DeepEqual(pr.RequiredCheckFailures, want) {
		t.Fatalf("RequiredCheckFailures = %#v, want %#v", pr.RequiredCheckFailures, want)
	}
}

func TestConnectorHydratePullRequestDetectsTransientKilledCheck(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/pulls/42",
			body:   `{"number":42,"html_url":"https://github.com/example/repo/pull/42","state":"open","mergeable_state":"clean","head":{"ref":"detent/example","sha":"head-sha"},"base":{"sha":"base-sha"}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/commits/head-sha/check-runs?per_page=100",
			body:   `{"check_runs":[{"id":9001,"name":"Checks","status":"completed","conclusion":"failure","details_url":"https://github.com/example/repo/actions/runs/8001/job/9001"}]}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/actions/runs/8001",
			body:   `{"id":8001}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/commits/head-sha/statuses?per_page=100",
			body:   `[]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/check-runs/9001/annotations?per_page=100",
			body:   `[{"path":"internal/web/page_templ.go","annotation_level":"failure","message":"compile: signal: killed (typecheck)"}]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/pulls/42/reviews?per_page=100",
			body:   `[]`,
		},
		emptyPullRequestCommentsResponse("example/repo", 42),
	})
	c := newGitHubTestConnector(t, server, Config{})
	prNumber := 42
	issue := connector.Issue{
		ID:         "I_kw42",
		Identifier: "example/repo#1",
		PRNumber:   &prNumber,
	}

	got, err := c.HydratePullRequest(context.Background(), issue)
	if err != nil {
		t.Fatalf("HydratePullRequest() error = %v", err)
	}
	if got.PullRequest == nil {
		t.Fatal("PullRequest = nil, want hydrated pull request")
	}
	checks := got.PullRequest.TransientFailedChecks
	if len(checks) != 1 {
		t.Fatalf("TransientFailedChecks = %#v, want one killed check", checks)
	}
	if checks[0].ID != 9001 || checks[0].WorkflowRunID != 8001 || checks[0].Name != "Checks" {
		t.Fatalf("TransientFailedChecks[0] = %#v, want check and workflow run IDs", checks[0])
	}
}

func TestConnectorHydratePullRequestIncludesWorkflowAndAnnotationsInRESTFanoutCap(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/pulls/42",
			body:   `{"number":42,"html_url":"https://github.com/example/repo/pull/42","state":"open","mergeable_state":"clean","head":{"ref":"detent/example","sha":"head-sha"},"base":{"sha":"base-sha"}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/commits/head-sha/check-runs?per_page=100",
			body:   `{"check_runs":[{"id":9001,"name":"Checks","status":"completed","conclusion":"failure","details_url":"https://github.com/example/repo/actions/runs/8001/job/9001"}]}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/actions/runs/8001",
			body:   `{"id":8001}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/commits/head-sha/statuses?per_page=100",
			body:   `[]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/check-runs/9001/annotations?per_page=100",
			body:   `[]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/pulls/42/reviews?per_page=100",
			body:   `[]`,
		},
		emptyPullRequestCommentsResponse("example/repo", 42),
	})
	c := newGitHubTestConnector(t, server, Config{
		RESTFanoutMaxRequests:      4,
		DisableConditionalRequests: true,
	})
	prNumber := 42
	issue := connector.Issue{
		ID:         "I_kw42",
		Identifier: "example/repo#1",
		PRNumber:   &prNumber,
	}

	got, err := c.HydratePullRequest(context.Background(), issue)
	if err != nil {
		t.Fatalf("HydratePullRequest() error = %v", err)
	}
	if got.PullRequest == nil || got.PullRequest.HydrationUnavailableReason != connector.PullRequestHydrationReasonRESTFanoutDeferred {
		t.Fatalf("PullRequest = %#v, want REST fanout deferral", got.PullRequest)
	}
	if requests := server.requests(); len(requests) != 4 {
		t.Fatalf("outbound REST requests = %d, want fanout cap 4; requests = %#v", len(requests), requests)
	}

	usage := c.FlushRESTRateLimitUsage()
	if got := restEndpointUsageCount(usage.Requests, "other"); got != 0 {
		t.Fatalf("other usage count = %d, want no unclassified hydration requests; usage = %#v", got, usage.Requests)
	}
	if got := restEndpointUsageCount(usage.Requests, "workflow runs"); got != 1 {
		t.Fatalf("workflow runs usage count = %d, want 1; usage = %#v", got, usage.Requests)
	}
	if got := restEndpointUsageCount(usage.Requests, "check run annotations"); got != 1 {
		t.Fatalf("check run annotations usage count = %d, want throttled synthetic request; usage = %#v", got, usage.Requests)
	}
}

func emptyPullRequestCommentsResponse(repository string, number int) graphqlTestResponse {
	return graphqlTestResponse{
		method: http.MethodGet,
		path:   fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", repository, number),
		body:   `[]`,
	}
}

func TestConnectorHydratePullRequestPropagatesCIDetailThrottles(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 18, 13, 0, 0, 0, time.UTC)
	throttleResponse := func(path string) graphqlTestResponse {
		return graphqlTestResponse{
			method: http.MethodGet,
			path:   path,
			status: http.StatusTooManyRequests,
			headers: map[string]string{
				"Retry-After":           "120",
				"X-RateLimit-Limit":     "5000",
				"X-RateLimit-Used":      "264",
				"X-RateLimit-Remaining": "4736",
				"X-RateLimit-Resource":  "core",
			},
			body: `{"message":"secondary rate limit"}`,
		}
	}
	tests := []struct {
		name         string
		responses    []graphqlTestResponse
		wantRequests int
	}{
		{
			name: "workflow run",
			responses: []graphqlTestResponse{
				{method: http.MethodGet, path: "/repos/example/repo/pulls/42", body: `{"number":42,"html_url":"https://github.com/example/repo/pull/42","state":"open","head":{"ref":"detent/example","sha":"head-sha"}}`},
				{method: http.MethodGet, path: "/repos/example/repo/commits/head-sha/check-runs?per_page=100", body: `{"check_runs":[{"id":9001,"name":"Checks","status":"completed","conclusion":"failure","details_url":"https://github.com/example/repo/actions/runs/8001/job/9001"}]}`},
				throttleResponse("/repos/example/repo/actions/runs/8001"),
				{method: http.MethodGet, path: "/repos/example/repo/commits/head-sha/statuses?per_page=100", body: `[]`},
				{method: http.MethodGet, path: "/repos/example/repo/check-runs/9001/annotations?per_page=100", body: `[]`},
				{method: http.MethodGet, path: "/repos/example/repo/pulls/42/reviews?per_page=100", body: `[]`},
			},
			wantRequests: 3,
		},
		{
			name: "check run annotations",
			responses: []graphqlTestResponse{
				{method: http.MethodGet, path: "/repos/example/repo/pulls/42", body: `{"number":42,"html_url":"https://github.com/example/repo/pull/42","state":"open","head":{"ref":"detent/example","sha":"head-sha"}}`},
				{method: http.MethodGet, path: "/repos/example/repo/commits/head-sha/check-runs?per_page=100", body: `{"check_runs":[{"id":9001,"name":"Checks","status":"completed","conclusion":"failure"}]}`},
				{method: http.MethodGet, path: "/repos/example/repo/commits/head-sha/statuses?per_page=100", body: `[]`},
				throttleResponse("/repos/example/repo/check-runs/9001/annotations?per_page=100"),
				{method: http.MethodGet, path: "/repos/example/repo/pulls/42/reviews?per_page=100", body: `[]`},
			},
			wantRequests: 4,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server := newGraphQLTestServer(t, test.responses)
			c := newGitHubTestConnector(t, server, Config{
				Now: func() time.Time {
					return now
				},
			})
			prNumber := 42
			issue := connector.Issue{
				ID:         "I_kw42",
				Identifier: "example/repo#1",
				PRNumber:   &prNumber,
			}

			got, err := c.HydratePullRequest(context.Background(), issue)
			if err != nil {
				t.Fatalf("HydratePullRequest() error = %v", err)
			}
			if got.PullRequest == nil {
				t.Fatal("PullRequest = nil, want hydration throttle marker")
			}
			if got.PullRequest.HydrationUnavailableReason != connector.PullRequestHydrationReasonSecondaryThrottled {
				t.Fatalf("HydrationUnavailableReason = %q, want secondary_throttled", got.PullRequest.HydrationUnavailableReason)
			}
			if got.PullRequest.HydrationNextRetryAt == nil || !got.PullRequest.HydrationNextRetryAt.After(now.Add(120*time.Second)) {
				t.Fatalf("HydrationNextRetryAt = %v, want retry-after plus jitter", got.PullRequest.HydrationNextRetryAt)
			}
			if requests := server.requests(); len(requests) != test.wantRequests {
				t.Fatalf("outbound REST requests = %d, want %d; requests = %#v", len(requests), test.wantRequests, requests)
			}
		})
	}
}

func TestConnectorRerunPullRequestChecksUsesWorkflowRun(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{method: http.MethodGet, path: "/repos/example/repo/actions/runs/8001", body: `{"id":8001,"status":"completed"}`},
		{
			method: http.MethodPost,
			path:   "/repos/example/repo/actions/runs/8001/rerun-failed-jobs",
			body:   `{}`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{})
	prNumber := 42
	issue := connector.Issue{
		ID:           "I_kw42",
		Identifier:   "example/repo#1",
		PRNumber:     &prNumber,
		PRRepository: "example/repo",
	}

	err := c.RerunPullRequestChecks(context.Background(), issue, []connector.PullRequestCheck{{
		ID:            9001,
		WorkflowRunID: 8001,
		Name:          "Checks",
	}})
	if err != nil {
		t.Fatalf("RerunPullRequestChecks() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 2 {
		t.Fatalf("requests len = %d, want 2", len(requests))
	}
	if requests[1]["method"] != http.MethodPost || requests[1]["path"] != "/repos/example/repo/actions/runs/8001/rerun-failed-jobs" {
		t.Fatalf("request = %#v, want workflow rerun", requests[1])
	}
}

func TestConnectorRerunPullRequestChecksDefersActiveWorkflow(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"queued", "in_progress", "waiting", ""} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			server := newGraphQLTestServer(t, []graphqlTestResponse{
				{method: http.MethodGet, path: "/repos/example/repo/actions/runs/8001", body: `{"id":8001,"status":"completed"}`},
				{method: http.MethodGet, path: "/repos/example/repo/actions/runs/8002", body: fmt.Sprintf(`{"id":8002,"status":%q}`, status)},
			})
			c := newGitHubTestConnector(t, server, Config{})
			issue := connector.Issue{Identifier: "example/repo#1", PRNumber: new(42), PRRepository: "example/repo"}
			err := c.RerunPullRequestChecks(t.Context(), issue, []connector.PullRequestCheck{{ID: 9001, WorkflowRunID: 8001}, {ID: 9002, WorkflowRunID: 8001}, {ID: 9003, WorkflowRunID: 8002}})
			if !connector.IsRetryable(err) {
				t.Fatalf("RerunPullRequestChecks() = %v, want retryable deferral", err)
			}
			for _, request := range server.requests() {
				if request["method"] != http.MethodGet {
					t.Fatalf("rerun submitted before all workflows completed: %v", request)
				}
			}
		})
	}
}

type graphqlTestServer struct {
	*httptest.Server
	t                  *testing.T
	mu                 sync.Mutex
	unsupportedNative  bool
	candidateHydration bool
	responses          []graphqlTestResponse
	seen               []map[string]any
	requestSeen        chan struct{}
}

type graphqlTestResponse struct {
	status  int
	method  string
	path    string
	accept  string
	headers map[string]string
	body    string
	release <-chan struct{}
}

func newGraphQLTestServer(t *testing.T, responses []graphqlTestResponse) *graphqlTestServer {
	t.Helper()

	server := &graphqlTestServer{
		t:           t,
		responses:   responses,
		requestSeen: make(chan struct{}, len(responses)+1),
	}
	// Legacy fixtures model repositories without native dependency support.
	// Native tests supply explicit endpoint responses and remain strictly scripted.
	server.unsupportedNative = true
	for _, response := range responses {
		if strings.Contains(response.path, "/dependencies/blocked_by") {
			server.unsupportedNative = false
		}
	}
	server.Server = httptest.NewServer(http.HandlerFunc(server.serveHTTP))
	t.Cleanup(server.Close)
	return server
}

func (s *graphqlTestServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	payload := map[string]any{
		"method": r.Method,
		"path":   r.URL.RequestURI(),
	}
	if ifNoneMatch := r.Header.Get("If-None-Match"); ifNoneMatch != "" {
		payload["if_none_match"] = ifNoneMatch
	}
	if r.Method == http.MethodPost && r.URL.Path == "/" {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			s.t.Fatalf("Decode() error = %v", err)
		}
		payload["method"] = r.Method
		payload["path"] = r.URL.RequestURI()
	} else {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			s.t.Fatalf("ReadAll() error = %v", err)
		}
		if len(raw) > 0 {
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				s.t.Fatalf("Unmarshal() error = %v", err)
			}
			payload["body"] = body
		}
	}

	s.mu.Lock()
	// Legacy scripts predate page hydration and exercise its REST fallback.
	// Dedicated batching fixtures serve DetentGitHubCandidateHydration explicitly.
	if query, _ := payload["query"].(string); strings.Contains(query, "DetentGitHubCandidateHydration") && !s.candidateHydration {
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errors":[{"message":"candidate hydration unsupported by legacy fixture"}]}`))
		return
	}
	if s.unsupportedNative && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/dependencies/blocked_by") {
		payload["implicit_native_probe"] = true
		s.seen = append(s.seen, payload)
		s.mu.Unlock()
		http.Error(w, `{"message":"native dependencies unsupported by fixture"}`, http.StatusNotFound)
		return
	}
	s.seen = append(s.seen, payload)
	select {
	case s.requestSeen <- struct{}{}:
	default:
	}
	if len(s.responses) == 0 {
		s.mu.Unlock()
		s.t.Fatalf("unexpected GraphQL request: %v", payload)
	}
	response := s.responses[0]
	s.responses = s.responses[1:]
	s.mu.Unlock()
	if response.method != "" && response.method != r.Method {
		s.t.Fatalf("method = %s, want %s", r.Method, response.method)
	}
	if response.path != "" && response.path != r.URL.RequestURI() {
		s.t.Fatalf("path = %s, want %s", r.URL.RequestURI(), response.path)
	}
	if response.accept != "" && response.accept != r.Header.Get("Accept") {
		s.t.Fatalf("accept = %s, want %s", r.Header.Get("Accept"), response.accept)
	}

	if response.release != nil {
		<-response.release
	}

	status := response.status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	for key, value := range response.headers {
		w.Header().Set(key, value)
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(response.body))
}

func (s *graphqlTestServer) requests() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Existing script assertions count scripted requests. The complete seen log
	// retains optional native probes for tests that assert total REST fanout.
	out := make([]map[string]any, 0, len(s.seen))
	for _, request := range s.seen {
		if request["implicit_native_probe"] != true {
			out = append(out, request)
		}
	}
	return out
}

func newGitHubTestConnector(t *testing.T, server *graphqlTestServer, cfg Config) *Connector {
	t.Helper()

	cfg.Endpoint = server.URL
	cfg.APIKey = "token"
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = server.Client()
	}
	cfg.GHToken = func(context.Context, string) (string, error) {
		t.Fatal("gh token fallback should not run")
		return "", nil
	}
	c, err := NewConnector(cfg)
	if err != nil {
		t.Fatalf("NewConnector() error = %v", err)
	}
	c.client.restBackoffs = newRESTBackoffRegistry()
	return c
}

func TestPullRequestCheckInventoryRetainsSettledNames(t *testing.T) {
	t.Parallel()

	checks := pullRequestCheckInventory(
		[]restCheckRun{{ID: 1, Name: "Test", Status: "completed", Conclusion: "success"}},
		[]restCommitStatus{{Context: "Deploy", State: "pending"}, {Context: "test", State: "success"}},
	)
	if len(checks) != 2 {
		t.Fatalf("checks = %#v, want check run and distinct status context", checks)
	}
	if checks[0].Name != "Test" || checks[0].Status != "completed" || checks[0].Conclusion != "success" {
		t.Fatalf("check run = %#v", checks[0])
	}
	if checks[1].Name != "Deploy" || checks[1].Status != "pending" {
		t.Fatalf("status context = %#v", checks[1])
	}
}

func TestPullRequestCheckInventoryRetainsIgnoredNames(t *testing.T) {
	t.Parallel()

	tests := []string{"cancelled", "canceled", "skipped"}
	for _, conclusion := range tests {
		t.Run(conclusion, func(t *testing.T) {
			t.Parallel()

			checks := pullRequestCheckInventory(
				[]restCheckRun{{ID: 1, Name: "Optional", Status: "completed", Conclusion: conclusion}},
				nil,
			)
			if len(checks) != 1 || checks[0].Name != "Optional" || checks[0].Conclusion != conclusion {
				t.Fatalf("checks = %#v, want ignored check name retained", checks)
			}
		})
	}
}

func TestCheckRunFailureEvidence(t *testing.T) {
	for _, tt := range []struct {
		name, annotations, text, want string
	}{
		{name: "first assertion", annotations: `[{"annotation_level":"warning","message":"not a failure"},{"annotation_level":"failure","path":"worker_test.go","message":"want released slot"},{"annotation_level":"failure","message":"later assertion"}]`, want: "worker_test.go: want released slot"},
		{name: "output fallback", annotations: `[]`, text: "Run make check: TestWorker failed", want: "Run make check: TestWorker failed"},
		{name: "no evidence", annotations: `[]`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := newGraphQLTestServer(t, []graphqlTestResponse{{method: http.MethodGet, path: "/repos/example/repo/check-runs/1/annotations?per_page=100", body: tt.annotations}})
			c := newGitHubTestConnector(t, server, Config{})
			runs := []restCheckRun{{ID: 1, Name: "test", Status: "completed", Conclusion: "failure", Output: checkRunOutput{Text: tt.text}}}
			if _, err := c.transientCheckRunFailures(t.Context(), pullRequestRepo{Owner: "example", Name: "repo"}, runs); err != nil {
				t.Fatal(err)
			}
			inventory := pullRequestCheckInventory(runs, nil)
			if len(inventory) != 1 || inventory[0].FailureDetail != tt.want {
				t.Fatalf("failure evidence = %#v, want %q", inventory, tt.want)
			}
		})
	}
}

func TestConnectorLookupBranchHead(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, repository, branch, body, want string
		wantErr                              bool
	}{
		{name: "branch with slash", repository: "example/repo", branch: "detent/issue-2601", body: `{"data":{"repository":{"ref":{"target":{"oid":"remote-head"}}}}}`, want: "remote-head"},
		{name: "missing branch", repository: "example/repo", branch: "detent/issue-2601", body: `{"data":{"repository":{"ref":null}}}`},
		{name: "missing repository", repository: "example/repo", branch: "detent/issue-2601", body: `{"data":{"repository":null}}`, wantErr: true},
		{name: "empty head", repository: "example/repo", branch: "detent/issue-2601", body: `{"data":{"repository":{"ref":{"target":{"oid":""}}}}}`, wantErr: true},
		{name: "API error", repository: "example/repo", branch: "detent/issue-2601", body: `{"errors":[{"message":"unavailable"}]}`, wantErr: true},
		{name: "invalid repository", repository: "invalid", branch: "detent/issue-2601", wantErr: true},
		{name: "empty branch", repository: "example/repo", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var responses []graphqlTestResponse
			if tt.body != "" {
				responses = append(responses, graphqlTestResponse{body: tt.body})
			}
			server := newGraphQLTestServer(t, responses)
			c := newGitHubTestConnector(t, server, Config{})
			got, err := c.LookupBranchHead(t.Context(), tt.repository, tt.branch)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("LookupBranchHead() = %q, %v; want %q, error %v", got, err, tt.want, tt.wantErr)
			}
			requests := server.requests()
			if tt.body == "" {
				if len(requests) != 0 {
					t.Fatalf("invalid input issued %d requests", len(requests))
				}
				return
			}
			if len(requests) != 1 {
				t.Fatalf("requests = %d, want 1", len(requests))
			}
			variables := requests[0]["variables"].(map[string]any)
			if variables["owner"] != "example" || variables["name"] != "repo" || variables["ref"] != "refs/heads/detent/issue-2601" {
				t.Fatalf("variables = %#v", variables)
			}
		})
	}
}

func TestConnectorPostCommitStatusPostsSuccessForTheExactHead(t *testing.T) {
	t.Parallel()

	sha := strings.Repeat("a1", 20)
	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodPost,
			path:   "/repos/example/repo/statuses/" + sha,
			body:   `{"context":"local-gate","state":"success","created_at":"2026-09-29T00:00:00Z"}`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{})
	if err := c.PostCommitStatus(context.Background(), "example/repo", sha, "local-gate", "Detent ran the configured gate on this head"); err != nil {
		t.Fatalf("PostCommitStatus() error = %v", err)
	}
	requests := server.requests()
	if len(requests) != 1 || requests[0]["method"] != http.MethodPost {
		t.Fatalf("requests = %#v, want one POST", requests)
	}
	body := requests[0]["body"].(map[string]any)
	if body["state"] != "success" || body["context"] != "local-gate" {
		t.Fatalf("status body = %#v, want success for local-gate", body)
	}

	for _, test := range []struct{ name, repository, sha, context string }{
		{name: "short sha", repository: "example/repo", sha: "abc123", context: "local-gate"},
		{name: "branch name", repository: "example/repo", sha: "main", context: "local-gate"},
		{name: "no context", repository: "example/repo", sha: sha, context: " "},
		{name: "no repository", repository: "repo", sha: sha, context: "local-gate"},
	} {
		if err := c.PostCommitStatus(context.Background(), test.repository, test.sha, test.context, ""); err == nil {
			t.Errorf("%s: PostCommitStatus() accepted an invalid target", test.name)
		}
	}
	if got := len(server.requests()); got != 1 {
		t.Fatalf("invalid targets reached GitHub: %d requests", got)
	}
}
