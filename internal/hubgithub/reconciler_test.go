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
	client := &scriptedRESTClient{t: t, steps: []restStep{
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent", response: reconcileRepository{ID: 100, NodeID: "R_repo", Name: "detent", Owner: restActor{Login: "digitaldrywood"}, UpdatedAt: updatedAt}},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/issues?direction=asc&per_page=100&sort=updated&state=all", response: []reconcileIssue{}},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/pulls?direction=asc&per_page=100&sort=updated&state=all", response: []reconcilePullRequest{}},
	}}
	_, err := (&Reconciler{client: client}).Reconcile(t.Context(), hubserver.ReconcileRequest{
		Repository: hubserver.RepositoryTarget{ID: 1, NodeID: "R_repo", Owner: "digitaldrywood", Name: "detent"},
		Mode:       hubserver.ReconcileFullRepair,
		Since:      &updatedAt,
		Through:    updatedAt,
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	client.assertDone()
}
