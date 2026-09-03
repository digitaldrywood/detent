package hubgithub

import (
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver"
)

func TestReconcilerUsesCursorAndRepositoryIdentity(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	updatedAt := since.Add(time.Hour)
	issuesPath := "/repos/new-owner/renamed/issues?" + url.Values{
		"direction": {"asc"},
		"per_page":  {"100"},
		"since":     {since.Format(time.RFC3339Nano)},
		"sort":      {"updated"},
		"state":     {"all"},
	}.Encode()
	pullsPath := "/repos/new-owner/renamed/pulls?direction=asc&per_page=100&sort=updated&state=all"
	body := "Issue body"
	client := &scriptedRESTClient{t: t, steps: []restStep{
		{
			method: http.MethodGet,
			path:   "/repositories/100",
			response: reconcileRepository{
				ID: 100, NodeID: "R_repo", Name: "renamed", Owner: restActor{Login: "new-owner"}, UpdatedAt: updatedAt,
			},
		},
		{
			method: http.MethodGet,
			path:   issuesPath,
			response: []reconcileIssue{
				{
					ID: 200, NodeID: "I_issue", Number: 17, Title: "Issue", Body: &body,
					HTMLURL: "https://github.test/new-owner/renamed/issues/17", State: "open",
					Labels: []restLabel{{Name: "feature"}}, Assignees: []restActor{{Login: "alice"}},
					CreatedAt: since.Add(-time.Hour), UpdatedAt: updatedAt,
				},
				{
					ID: 201, NodeID: "PR_issue", Number: 18, Title: "Pull request issue", Body: &body,
					HTMLURL: "https://github.test/new-owner/renamed/pull/18", State: "open", PullRequest: &struct{}{},
					CreatedAt: since.Add(-time.Hour), UpdatedAt: updatedAt,
				},
			},
		},
		{
			method: http.MethodGet,
			path:   pullsPath,
			response: []reconcilePullRequest{{
				ID: 201, NodeID: "PR_issue", Number: 18, Title: "Pull request",
				HTMLURL: "https://github.test/new-owner/renamed/pull/18", State: "open",
				Head: restRef{Ref: "feature", SHA: "head"}, Base: restRef{Ref: "main", SHA: "base"},
				CreatedAt: since.Add(-time.Hour), UpdatedAt: updatedAt,
			}},
		},
	}}
	databaseID := int64(100)
	snapshot, err := (&Reconciler{client: client}).Reconcile(t.Context(), hubserver.ReconcileRequest{
		Repository: hubserver.RepositoryTarget{ID: 1, NodeID: "R_repo", DatabaseID: &databaseID, Owner: "old-owner", Name: "old-name"},
		Mode:       hubserver.ReconcileIncremental,
		Since:      &since,
		Through:    updatedAt,
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	client.assertDone()
	wantRepository := hubserver.RepositorySource{NodeID: "R_repo", DatabaseID: &databaseID, Owner: "new-owner", Name: "renamed", UpdatedAt: updatedAt}
	if !reflect.DeepEqual(snapshot.Repository, wantRepository) {
		t.Fatalf("repository = %#v, want %#v", snapshot.Repository, wantRepository)
	}
	if len(snapshot.Issues) != 1 || snapshot.Issues[0].Number != 17 || !reflect.DeepEqual(snapshot.Issues[0].Labels, []string{"feature"}) || !reflect.DeepEqual(snapshot.Issues[0].Assignees, []string{"alice"}) {
		t.Fatalf("issues = %#v, want one normalized issue", snapshot.Issues)
	}
	if len(snapshot.PullRequests) != 1 || snapshot.PullRequests[0].Number != 18 || snapshot.PullRequests[0].HeadSHA != "head" {
		t.Fatalf("pull requests = %#v, want one normalized pull request", snapshot.PullRequests)
	}
}

func TestReconcilerFullRepairOmitsIncrementalCursor(t *testing.T) {
	t.Parallel()
	updatedAt := time.Date(2026, 9, 2, 13, 0, 0, 0, time.UTC)
	pullRequest := reconcilePullRequest{
		ID: 201, NodeID: "PR_issue", Number: 18, Title: "Pull request", HTMLURL: "https://github.test/digitaldrywood/detent/pull/18", State: "open",
		Head: restRef{Ref: "feature", SHA: "head"}, Base: restRef{Ref: "main", SHA: "base"}, CreatedAt: updatedAt.Add(-time.Hour), UpdatedAt: updatedAt,
	}
	detailedPullRequest := pullRequest
	detailedPullRequest.MergeableState = "clean"
	client := &scriptedRESTClient{t: t, steps: []restStep{
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent", response: reconcileRepository{ID: 100, NodeID: "R_repo", Name: "detent", Owner: restActor{Login: "digitaldrywood"}, UpdatedAt: updatedAt}},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/issues?direction=asc&per_page=100&sort=updated&state=all", response: []reconcileIssue{}},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/pulls?direction=asc&per_page=100&sort=updated&state=all", response: []reconcilePullRequest{pullRequest}},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/pulls/18", response: detailedPullRequest},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/commits/head/check-runs?per_page=100", response: restCheckRuns{}},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/commits/head/statuses?per_page=100", response: []restStatus{}},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/pulls/18/reviews?per_page=100", response: []restReview{}},
	}}
	snapshot, err := (&Reconciler{client: client}).Reconcile(t.Context(), hubserver.ReconcileRequest{
		Repository: hubserver.RepositoryTarget{ID: 1, NodeID: "R_repo", Owner: "digitaldrywood", Name: "detent"},
		Mode:       hubserver.ReconcileFullRepair,
		Since:      &updatedAt,
		Through:    updatedAt,
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	client.assertDone()
	if len(snapshot.PullRequestDetails) != 1 || snapshot.PullRequestDetails[0].Checks == nil || snapshot.PullRequestDetails[0].Reviews == nil {
		t.Fatalf("full repair details = %#v, want every open pull request hydrated", snapshot.PullRequestDetails)
	}
}

func TestReconcilerHydratesRequestedPullRequestDetails(t *testing.T) {
	t.Parallel()
	updatedAt := time.Date(2026, 9, 2, 13, 0, 0, 0, time.UTC)
	reviewedAt := updatedAt.Add(time.Minute)
	approvedAt := reviewedAt.Add(time.Minute)
	commentedAt := approvedAt.Add(time.Minute)
	changesRequested := restReview{State: "CHANGES_REQUESTED", SubmittedAt: &reviewedAt}
	changesRequested.User.Login = "alice"
	approved := restReview{State: "APPROVED", SubmittedAt: &approvedAt}
	approved.User.Login = "alice"
	commented := restReview{State: "COMMENTED", Body: "[P1] Blocking finding.", SubmittedAt: &commentedAt}
	commented.User.Login = "codex"
	pullRequest := reconcilePullRequest{
		ID: 201, NodeID: "PR_issue", Number: 18, Title: "Pull request",
		HTMLURL: "https://github.test/new-owner/renamed/pull/18", State: "open",
		Head: restRef{Ref: "feature", SHA: "head"}, Base: restRef{Ref: "main", SHA: "base"},
		CreatedAt: updatedAt.Add(-time.Hour), UpdatedAt: updatedAt,
	}
	detailedPullRequest := pullRequest
	detailedPullRequest.MergeableState = "clean"
	client := &scriptedRESTClient{t: t, steps: []restStep{
		{method: http.MethodGet, path: "/repositories/100", response: reconcileRepository{ID: 100, NodeID: "R_repo", Name: "renamed", Owner: restActor{Login: "new-owner"}, UpdatedAt: updatedAt}},
		{method: http.MethodGet, path: "/repos/new-owner/renamed/issues?direction=asc&per_page=100&sort=updated&state=all", response: []reconcileIssue{}},
		{method: http.MethodGet, path: "/repos/new-owner/renamed/pulls?direction=asc&per_page=100&sort=updated&state=all", response: []reconcilePullRequest{pullRequest}},
		{method: http.MethodGet, path: "/repos/new-owner/renamed/pulls/18", response: detailedPullRequest},
		{method: http.MethodGet, path: "/repos/new-owner/renamed/commits/head/check-runs?per_page=100", response: restCheckRuns{CheckRuns: []restCheckRun{{ID: 1, Name: "CI", Status: "completed", Conclusion: "success"}}}},
		{method: http.MethodGet, path: "/repos/new-owner/renamed/commits/head/statuses?per_page=100", response: []restStatus{{Context: "deploy", State: "success"}}},
		{method: http.MethodGet, path: "/repos/new-owner/renamed/pulls/18/reviews?per_page=100", response: []restReview{changesRequested, approved, commented}},
	}}
	databaseID := int64(100)
	snapshot, err := (&Reconciler{client: client}).Reconcile(t.Context(), hubserver.ReconcileRequest{
		Repository: hubserver.RepositoryTarget{ID: 1, NodeID: "R_repo", DatabaseID: &databaseID, Owner: "old-owner", Name: "old-name"},
		Mode:       hubserver.ReconcileIncremental,
		Through:    updatedAt,
		Hydrations: []hubserver.HydrationRequest{
			{ID: 1, ObjectKind: "pull_request_checks", ObjectKey: "18", GitHubNumber: 18, RequestCount: 1},
			{ID: 2, ObjectKind: "pull_request_reviews", ObjectKey: "18", GitHubNumber: 18, RequestCount: 1},
			{ID: 3, ObjectKind: "commit_checks", ObjectKey: "head", HeadSHA: "head", RequestCount: 1},
		},
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	client.assertDone()
	if len(snapshot.PullRequestDetails) != 1 {
		t.Fatalf("pull request details = %#v, want one", snapshot.PullRequestDetails)
	}
	details := snapshot.PullRequestDetails[0]
	if details.MergeableState == nil || *details.MergeableState != "clean" || details.Checks == nil || details.Reviews == nil {
		t.Fatalf("pull request details = %#v, want merge, checks, and reviews", details)
	}
	if details.Checks.Total != 2 || details.Checks.Passed != 2 || details.Checks.Conclusion != "success" {
		t.Fatalf("checks = %#v, want two passing contexts", details.Checks)
	}
	if details.Reviews.Approvals != 1 || details.Reviews.ChangesRequested != 0 || details.Reviews.Comments != 1 || details.Reviews.Decision != "changes_requested" || details.Reviews.State != "p1" {
		t.Fatalf("reviews = %#v, want current P1 to block the effective review summary", details.Reviews)
	}
}
