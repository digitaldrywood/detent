package hubgithub

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	connectorgithub "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
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
				ID: 100, NodeID: "R_repo", Name: "renamed", Owner: restActor{Login: "new-owner"}, DefaultBranch: "primary", UpdatedAt: updatedAt,
			},
		},
		{
			method: http.MethodGet,
			path:   issuesPath,
			response: []reconcileIssue{
				{
					ID: 200, NodeID: "I_issue", Number: 17, Title: "Issue", Body: &body,
					HTMLURL: "https://github.test/new-owner/renamed/issues/17", State: "open",
					User:   restActor{Login: "octocat"},
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
				HTMLURL: "https://github.test/new-owner/renamed/pull/18", State: "closed", MergeSHA: "merged-commit", MergedAt: updatedAt,
				Head: restRef{Ref: "feature", SHA: "head"}, Base: restRef{Ref: "main", SHA: "base"},
				CreatedAt: since.Add(-time.Hour), UpdatedAt: updatedAt,
			}, {
				ID: 202, NodeID: "PR_older", Number: 19, Title: "Historical pull request",
				HTMLURL: "https://github.test/new-owner/renamed/pull/19", State: "closed", MergeSHA: "older-commit", MergedAt: since.Add(-time.Minute),
				Head: restRef{Ref: "older-feature", SHA: "older-head"}, Base: restRef{Ref: "main", SHA: "older-base"},
				CreatedAt: since.Add(-time.Hour), UpdatedAt: since.Add(-time.Minute),
			}},
		},
		{
			method: http.MethodGet,
			path:   "/repos/new-owner/renamed/pulls/18",
			response: reconcilePullRequest{
				ID: 201, NodeID: "PR_issue", Number: 18, Title: "Pull request",
				HTMLURL: "https://github.test/new-owner/renamed/pull/18", State: "closed",
				MergeSHA: "merged-commit", MergedAt: updatedAt, MergedBy: restActor{Login: "example-operator"},
				Head: restRef{Ref: "feature", SHA: "head"}, Base: restRef{Ref: "main", SHA: "base"},
				CreatedAt: since.Add(-time.Hour), UpdatedAt: updatedAt,
			},
		},
	}}
	databaseID := int64(100)
	snapshot, err := (&Reconciler{client: client}).Reconcile(t.Context(), githubcontract.ReconcileRequest{
		Repository: githubcontract.RepositoryTarget{ID: 1, NodeID: "R_repo", DatabaseID: &databaseID, Owner: "old-owner", Name: "old-name"},
		Mode:       githubcontract.ReconcileIncremental,
		Since:      &since,
		Through:    updatedAt,
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	client.assertDone()
	wantRepository := githubcontract.RepositorySource{DefaultBranch: "primary", NodeID: "R_repo", DatabaseID: &databaseID, Owner: "new-owner", Name: "renamed", UpdatedAt: updatedAt}
	if !reflect.DeepEqual(snapshot.Repository, wantRepository) {
		t.Fatalf("repository = %#v, want %#v", snapshot.Repository, wantRepository)
	}
	if len(snapshot.Issues) != 1 || snapshot.Issues[0].Number != 17 || snapshot.Issues[0].AuthorID != "octocat" || !reflect.DeepEqual(snapshot.Issues[0].Labels, []string{"feature"}) || !reflect.DeepEqual(snapshot.Issues[0].Assignees, []string{"alice"}) {
		t.Fatalf("issues = %#v, want one normalized issue", snapshot.Issues)
	}
	if len(snapshot.PullRequests) != 2 || snapshot.PullRequests[0].Number != 18 || snapshot.PullRequests[0].HeadSHA != "head" || snapshot.PullRequests[0].MergeSHA != "merged-commit" || !snapshot.PullRequests[0].MergedAt.Equal(updatedAt) || snapshot.PullRequests[0].MergedBy != "example-operator" {
		t.Fatalf("pull requests = %#v, want current and historical pull requests", snapshot.PullRequests)
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
	snapshot, err := (&Reconciler{client: client}).Reconcile(t.Context(), githubcontract.ReconcileRequest{
		Repository: githubcontract.RepositoryTarget{ID: 1, NodeID: "R_repo", Owner: "digitaldrywood", Name: "detent"},
		Mode:       githubcontract.ReconcileFullRepair,
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
		HTMLURL: "https://github.test/new-owner/renamed/pull/18", State: "closed", MergeSHA: "merged-commit", MergedAt: updatedAt,
		Head: restRef{Ref: "feature", SHA: "head"}, Base: restRef{Ref: "main", SHA: "base"},
		CreatedAt: updatedAt.Add(-time.Hour), UpdatedAt: updatedAt,
	}
	detailedPullRequest := pullRequest
	detailedPullRequest.MergeableState = "clean"
	detailedPullRequest.MergedBy.Login = "example-operator"
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
	snapshot, err := (&Reconciler{client: client}).Reconcile(t.Context(), githubcontract.ReconcileRequest{
		Repository: githubcontract.RepositoryTarget{ID: 1, NodeID: "R_repo", DatabaseID: &databaseID, Owner: "old-owner", Name: "old-name"},
		Mode:       githubcontract.ReconcileIncremental,
		Through:    updatedAt,
		Hydrations: []githubcontract.HydrationRequest{
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
	t.Run("targeted", func(t *testing.T) {
		repository := reconcileRepository{DefaultBranch: "primary", ID: 100, NodeID: "R_repo", Name: "renamed", Owner: restActor{Login: "new-owner"}, UpdatedAt: updatedAt}
		targeted := detailedPullRequest
		targeted.Base.Repository = &repository
		for _, name := range []string{"current", "wrong_repository", "wrong_number", "wrong_head", "wrong_node"} {
			t.Run(name, func(t *testing.T) {
				pull := targeted
				sourceRepository := repository
				pull.Base.Repository = &sourceRepository
				switch name {
				case "wrong_repository":
					sourceRepository.Name = "other"
				case "wrong_number":
					pull.Number++
				case "wrong_head":
					pull.Head.SHA = "other"
				case "wrong_node":
					sourceRepository.NodeID = "R_other"
				}
				steps := []restStep{{method: http.MethodGet, path: "/repos/new-owner/renamed/pulls/18", response: pull}}
				if name == "current" {
					steps = append(steps,
						restStep{method: http.MethodGet, path: "/repos/new-owner/renamed/commits/head/check-runs?per_page=100", response: restCheckRuns{CheckRuns: []restCheckRun{{ID: 1, Name: "CI", Status: "completed", Conclusion: "success"}}}},
						restStep{method: http.MethodGet, path: "/repos/new-owner/renamed/commits/head/statuses?per_page=100", response: []restStatus{{Context: "deploy", State: "success"}}},
						restStep{method: http.MethodGet, path: "/repos/new-owner/renamed/pulls/18/reviews?per_page=100", response: []restReview{changesRequested, approved, commented}})
				}
				client := &scriptedRESTClient{t: t, steps: steps}
				snapshot, err := (&Reconciler{client: client}).Reconcile(t.Context(), githubcontract.ReconcileRequest{
					Repository: githubcontract.RepositoryTarget{Owner: "new-owner", Name: "renamed", NodeID: "R_repo"}, TargetedPullRequests: true,
					Hydrations: []githubcontract.HydrationRequest{{ObjectKind: "pull_request", GitHubNumber: 18, HeadSHA: "head", RequestCount: 1}},
				})
				client.assertDone()
				if name != "current" {
					if err == nil || !githubcontract.IsPermanent(err) {
						t.Fatalf("changed provider identity was not definitely refused: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(snapshot.PullRequests) != 1 || len(snapshot.PullRequestDetails) != 1 || snapshot.PullRequests[0].HeadSHA != "head" || snapshot.Repository.NodeID != "R_repo" || snapshot.Repository.DefaultBranch != "primary" {
					t.Fatalf("targeted snapshot=%+v", snapshot)
				}
				details := snapshot.PullRequestDetails[0]
				if details.MergeableState == nil || *details.MergeableState != "clean" || details.Checks == nil || details.Checks.Total != 2 || details.Checks.Passed != 2 || details.Checks.Conclusion != "success" || details.Reviews == nil || details.Reviews.Approvals != 1 || details.Reviews.ChangesRequested != 0 || details.Reviews.Comments != 1 || details.Reviews.Decision != "changes_requested" || details.Reviews.State != "p1" {
					t.Fatalf("targeted facts=%+v", details)
				}
			})
		}
	})

	t.Run("targeted_failure_classification", func(t *testing.T) {
		for _, test := range []struct {
			name      string
			stage     string
			err       error
			permanent bool
		}{
			{name: "not_found", err: &connectorgithub.StatusError{StatusCode: http.StatusNotFound, Err: connectorgithub.ErrNotFound}, permanent: true},
			{name: "gone", err: &connectorgithub.StatusError{StatusCode: http.StatusGone, Err: connectorgithub.ErrUnexpectedStatus}, permanent: true},
			{name: "unauthorized", err: &connectorgithub.StatusError{StatusCode: http.StatusUnauthorized, Err: connectorgithub.ErrAuthenticationFailed}},
			{name: "missing_token", err: connectorgithub.ErrMissingToken},
			{name: "authentication_failed", err: connectorgithub.ErrAuthenticationFailed},
			{name: "bad_request", err: &connectorgithub.StatusError{StatusCode: http.StatusBadRequest, Err: connectorgithub.ErrUnexpectedStatus}},
			{name: "method_not_allowed", err: &connectorgithub.StatusError{StatusCode: http.StatusMethodNotAllowed, Err: connectorgithub.ErrUnexpectedStatus}},
			{name: "unprocessable", err: &connectorgithub.StatusError{StatusCode: http.StatusUnprocessableEntity, Err: connectorgithub.ErrUnexpectedStatus}},
			{name: "raw_not_found", err: connectorgithub.ErrNotFound, permanent: true},
			{name: "status_not_found", err: &connectorgithub.StatusError{StatusCode: http.StatusNotFound, Err: connectorgithub.ErrUnexpectedStatus}, permanent: true},
			{name: "throttled", err: &connectorgithub.StatusError{StatusCode: http.StatusTooManyRequests, Err: connectorgithub.ErrRateLimited}},
			{name: "forbidden_rate_limit", err: &connectorgithub.StatusError{StatusCode: http.StatusForbidden, Err: connectorgithub.ErrRateLimited}},
			{name: "ambiguous_forbidden", err: &connectorgithub.StatusError{StatusCode: http.StatusForbidden, Err: connectorgithub.ErrUnexpectedStatus}},
			{name: "request_timeout", err: &connectorgithub.StatusError{StatusCode: http.StatusRequestTimeout, Err: connectorgithub.ErrTransient}},
			{name: "upstream_failure", err: &connectorgithub.StatusError{StatusCode: http.StatusBadGateway, Err: connectorgithub.ErrTransient}},
			{name: "uncertain_transport", err: errors.New("connection interrupted")},
			{name: "caller_cancelled", err: context.Canceled},
			{name: "caller_deadline", err: context.DeadlineExceeded},
			{name: "check_runs_not_found", stage: "check_runs", err: &connectorgithub.StatusError{StatusCode: http.StatusNotFound, Err: connectorgithub.ErrNotFound}},
			{name: "check_runs_gone", stage: "check_runs", err: &connectorgithub.StatusError{StatusCode: http.StatusGone, Err: connectorgithub.ErrUnexpectedStatus}},
			{name: "check_runs_raw_not_found", stage: "check_runs", err: connectorgithub.ErrNotFound},
			{name: "statuses_not_found", stage: "statuses", err: &connectorgithub.StatusError{StatusCode: http.StatusNotFound, Err: connectorgithub.ErrNotFound}},
			{name: "statuses_gone", stage: "statuses", err: &connectorgithub.StatusError{StatusCode: http.StatusGone, Err: connectorgithub.ErrUnexpectedStatus}},
			{name: "statuses_raw_not_found", stage: "statuses", err: connectorgithub.ErrNotFound},
			{name: "reviews_not_found", stage: "reviews", err: &connectorgithub.StatusError{StatusCode: http.StatusNotFound, Err: connectorgithub.ErrNotFound}},
			{name: "reviews_gone", stage: "reviews", err: &connectorgithub.StatusError{StatusCode: http.StatusGone, Err: connectorgithub.ErrUnexpectedStatus}},
			{name: "reviews_raw_not_found", stage: "reviews", err: connectorgithub.ErrNotFound},
		} {
			t.Run(test.name, func(t *testing.T) {
				steps := []restStep{{method: http.MethodGet, path: "/repos/new-owner/renamed/pulls/18", err: test.err}}
				if test.stage != "" {
					pull := detailedPullRequest
					pull.Base.Repository = &reconcileRepository{ID: 100, NodeID: "R_repo", Name: "renamed", Owner: restActor{Login: "new-owner"}, UpdatedAt: updatedAt}
					steps = []restStep{{method: http.MethodGet, path: "/repos/new-owner/renamed/pulls/18", response: pull}}
					checkStep := restStep{method: http.MethodGet, path: "/repos/new-owner/renamed/commits/head/check-runs?per_page=100", response: restCheckRuns{CheckRuns: []restCheckRun{}}}
					statusStep := restStep{method: http.MethodGet, path: "/repos/new-owner/renamed/commits/head/statuses?per_page=100", response: []restStatus{}}
					reviewStep := restStep{method: http.MethodGet, path: "/repos/new-owner/renamed/pulls/18/reviews?per_page=100"}
					switch test.stage {
					case "check_runs":
						checkStep.err = test.err
						steps = append(steps, checkStep)
					case "statuses":
						statusStep.err = test.err
						steps = append(steps, checkStep, statusStep)
					case "reviews":
						reviewStep.err = test.err
						steps = append(steps, checkStep, statusStep, reviewStep)
					default:
						t.Fatalf("unknown read stage %q", test.stage)
					}
				}
				client := &scriptedRESTClient{t: t, steps: steps}
				_, err := (&Reconciler{client: client}).Reconcile(t.Context(), githubcontract.ReconcileRequest{Repository: githubcontract.RepositoryTarget{Owner: "new-owner", Name: "renamed", NodeID: "R_repo"}, TargetedPullRequests: true, Hydrations: []githubcontract.HydrationRequest{{ObjectKind: "pull_request", GitHubNumber: 18, HeadSHA: "head", RequestCount: 1}}})
				client.assertDone()
				if !errors.Is(err, test.err) || githubcontract.IsPermanent(err) != test.permanent {
					t.Fatalf("targeted failure=%v permanent=%v want=%v", err, githubcontract.IsPermanent(err), test.permanent)
				}
			})
		}
		t.Run("invalid_target_request", func(t *testing.T) {
			client := &scriptedRESTClient{t: t}
			_, err := (&Reconciler{client: client}).Reconcile(t.Context(), githubcontract.ReconcileRequest{Repository: githubcontract.RepositoryTarget{Owner: "new-owner", Name: "renamed"}, TargetedPullRequests: true})
			client.assertDone()
			if err == nil || githubcontract.IsPermanent(err) {
				t.Fatalf("request-shape fault became definite PR refusal: %v", err)
			}
		})
		incomplete := detailedPullRequest
		incomplete.Base.Repository = nil
		client := &scriptedRESTClient{t: t, steps: []restStep{{method: http.MethodGet, path: "/repos/new-owner/renamed/pulls/18", response: incomplete}}}
		_, err := (&Reconciler{client: client}).Reconcile(t.Context(), githubcontract.ReconcileRequest{Repository: githubcontract.RepositoryTarget{Owner: "new-owner", Name: "renamed"}, TargetedPullRequests: true, Hydrations: []githubcontract.HydrationRequest{{ObjectKind: "pull_request", GitHubNumber: 18, HeadSHA: "head", RequestCount: 1}}})
		client.assertDone()
		if err == nil || githubcontract.IsPermanent(err) {
			t.Fatalf("incomplete source falsely classified definite: %v", err)
		}
	})

}

func TestReconcilerPagedPullRequestsStayBounded(t *testing.T) {
	t.Parallel()
	updatedAt := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	repository := reconcileRepository{ID: 100, NodeID: "R_repo", Name: "example-project", Owner: restActor{Login: "example-org"}, DefaultBranch: "main", UpdatedAt: updatedAt}
	merged := reconcilePullRequest{ID: 201, NodeID: "PR_merged", Number: 18, Title: "Merged without merged_by", HTMLURL: "https://github.test/example-org/example-project/pull/18", State: "closed", MergeSHA: "merged-commit", MergedAt: updatedAt,
		Head: restRef{Ref: "feature", SHA: "head"}, Base: restRef{Ref: "main", SHA: "base"}, CreatedAt: updatedAt.Add(-time.Hour), UpdatedAt: updatedAt}
	for _, test := range []struct {
		name string
		next string
		more bool
	}{{"page with a successor", "/repos/example-org/example-project/pulls?page=3", true}, {"last page", "", false}} {
		t.Run(test.name, func(t *testing.T) {
			client := &scriptedRESTClient{t: t, steps: []restStep{
				{method: http.MethodGet, path: "/repos/example-org/example-project", response: repository},
				{method: http.MethodGet, path: "/repos/example-org/example-project/pulls?direction=desc&per_page=100&sort=updated&state=all&page=2", response: []reconcilePullRequest{merged}, next: test.next},
			}}
			snapshot, err := NewReconciler(client).Reconcile(t.Context(), githubcontract.ReconcileRequest{Profile: "native", Repository: githubcontract.RepositoryTarget{Owner: "example-org", Name: "example-project"}, PullRequestPage: 2})
			if err != nil {
				t.Fatal(err)
			}
			if len(client.steps) != 0 || len(snapshot.PullRequests) != 1 || len(snapshot.PullRequestDetails) != 0 || snapshot.PullRequestsMore != test.more {
				t.Fatalf("paged snapshot=%+v remaining calls=%d", snapshot, len(client.steps))
			}
		})
	}
}
