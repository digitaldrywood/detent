package hubgithub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	connectorgithub "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hubserver"
)

func TestImportPagesPreserveSourceRecords(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		stage, path, kind, next string
		raw                     string
	}{
		{"comments", "/comments", "comment", "/repos/org/repo/issues/1/comments?per_page=100&page=2", `[{"id":123,"node_id":"IC_comment","body":"Full comment","user":{"login":"alice","node_id":"U_alice"},"created_at":"2020-01-01T00:00:00Z","updated_at":"2021-01-01T00:00:00Z"}]`},
		{"timeline", "/timeline", "timeline", "", `[{"id":456,"event":"closed","actor":{"login":"bob","node_id":"U_bob"},"created_at":"2022-01-01T00:00:00Z"}]`},
		{"dependencies", "/dependencies/blocked_by", "dependency", "", `[{"id":789,"node_id":"I_blocker","number":2,"user":{"login":"alice"}}]`},
	} {
		t.Run(test.stage, func(t *testing.T) {
			client := &scriptedRESTClient{t: t, steps: []restStep{{method: http.MethodGet, path: "/repos/org/repo/issues/1" + test.path + "?per_page=100", response: json.RawMessage(test.raw), next: test.next}}}
			page, err := NewImporter(client).FetchImportPage(t.Context(), hubserver.GitHubImportRequest{Repository: "org/repo", IssueNumber: 1, Stage: test.stage})
			if err != nil || len(page.Records) != 1 || page.NextCursor != test.next {
				t.Fatalf("page = %+v, %v", page, err)
			}
			record := page.Records[0]
			if record.Kind != test.kind || record.SourceKey == "" || len(record.Data) < 40 || record.Provenance.AuthorID == "" || record.Provenance.ObservedAt.IsZero() {
				t.Fatalf("provenance = %+v", record)
			}
			if test.kind == "comment" && (record.Body != "Full comment" || record.Provenance.AuthorID != "U_alice") {
				t.Fatalf("comment = %+v", record)
			}
		})
	}
}

func TestImporterRejectsChangedCursorAndPartialResponses(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		request hubserver.GitHubImportRequest
		client  *scriptedRESTClient
	}{
		{name: "changed endpoint", request: hubserver.GitHubImportRequest{Repository: "org/repo", IssueNumber: 1, Stage: "comments", Cursor: "/repos/elsewhere/private/issues/1/comments?page=2"}},
		{name: "missing repository", request: hubserver.GitHubImportRequest{Repository: "bad", IssueNumber: 1, Stage: "comments"}},
		{name: "unknown stage", request: hubserver.GitHubImportRequest{Repository: "org/repo", IssueNumber: 1, Stage: "unknown"}},
		{name: "issue excerpt", request: hubserver.GitHubImportRequest{Repository: "org/repo", IssueNumber: 1, Stage: "issue"}, client: &scriptedRESTClient{t: t, steps: []restStep{{method: http.MethodGet, path: "/repos/org/repo/issues/1", response: json.RawMessage(`{"id":123,"node_id":"I_issue","number":1,"title":"Excerpt"}`)}}}},
		{name: "403", request: hubserver.GitHubImportRequest{Repository: "org/repo", IssueNumber: 1, Stage: "comments"}, client: &scriptedRESTClient{t: t, steps: []restStep{{method: http.MethodGet, path: "/repos/org/repo/issues/1/comments?per_page=100", err: &connectorgithub.StatusError{StatusCode: 403}}}}},
		{name: "429", request: hubserver.GitHubImportRequest{Repository: "org/repo", IssueNumber: 1, Stage: "comments"}, client: &scriptedRESTClient{t: t, steps: []restStep{{method: http.MethodGet, path: "/repos/org/repo/issues/1/comments?per_page=100", err: &connectorgithub.StatusError{StatusCode: 429}}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := test.client
			if client == nil {
				client = &scriptedRESTClient{t: t}
			}
			if _, err := NewImporter(client).FetchImportPage(t.Context(), test.request); err == nil {
				t.Fatal("invalid or incomplete import succeeded")
			}
		})
	}
}

type editFixtureClient struct {
	response json.RawMessage
	err      error
}

func (c editFixtureClient) REST(context.Context, string, string, any, any) error {
	return errors.New("unexpected REST")
}
func (c editFixtureClient) RESTPage(context.Context, string, any) (string, error) {
	return "", errors.New("unexpected REST page")
}
func (c editFixtureClient) GraphQL(_ context.Context, _ string, _ map[string]any, output any) error {
	if c.err != nil {
		return c.err
	}
	return json.Unmarshal(c.response, output)
}

func TestImportEditHistoryPaginationAndRedaction(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, raw, cursor string
		wantError         bool
		records, gaps     int
	}{
		{"page", `{"node":{"userContentEdits":{"nodes":[{"id":"edit1","createdAt":"2020-01-01T00:00:00Z","updatedAt":"2020-01-02T00:00:00Z","diff":"Full diff","editor":{"id":"U_alice","login":"alice"}}],"pageInfo":{"hasNextPage":true,"endCursor":"second"}}}}`, "second", false, 1, 0},
		{"redacted", `{"node":{"userContentEdits":{"nodes":[{"id":"edit2","diff":null}],"pageInfo":{"hasNextPage":false}}}}`, "", false, 1, 1},
		{"inaccessible", `{"node":null}`, "", false, 0, 1},
		{"missing cursor", `{"node":{"userContentEdits":{"nodes":[],"pageInfo":{"hasNextPage":true}}}}`, "", true, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			page, err := NewImporter(editFixtureClient{response: json.RawMessage(test.raw)}).FetchImportPage(t.Context(), hubserver.GitHubImportRequest{Stage: "edits", SourceID: "I_issue"})
			if (err != nil) != test.wantError || page.NextCursor != test.cursor || len(page.Records) != test.records || len(page.Gaps) != test.gaps {
				t.Fatalf("edit page = %+v, %v", page, err)
			}
		})
	}
}

func TestSharedTransportBackoffAndRequestCounts(t *testing.T) {
	t.Parallel()
	for _, status := range []int{403, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			client := &scriptedRESTClient{t: t, steps: []restStep{{method: http.MethodGet, path: "/repos/org/repo/issues", err: &connectorgithub.StatusError{StatusCode: status, RetryAfter: 2 * time.Minute}}, {method: http.MethodGet, path: "/repos/org/repo/pulls", response: []restPullRequest{}}}}
			transport := NewTransport(nil)
			transport.client = client
			transport.now = func() time.Time { return now }
			ctx := scopedRequests(t.Context(), "native", "import")
			if err := transport.REST(ctx, http.MethodGet, "/repos/org/repo/issues", nil, nil); err == nil {
				t.Fatal("missing upstream error")
			}
			if err := transport.REST(ctx, http.MethodGet, "/repos/org/repo/pulls", nil, nil); err == nil {
				t.Fatal("request escaped shared backoff")
			}
			now = now.Add(2 * time.Minute)
			if err := transport.REST(scopedRequests(t.Context(), "github_compatible", "reconcile"), http.MethodGet, "/repos/org/repo/pulls", nil, nil); err != nil {
				t.Fatal(err)
			}
			counts := transport.Counts()
			if len(counts) != 2 || counts[0].Requests != 1 || counts[1].Requests != 1 || counts[1].Errors != 1 || len(client.steps) != 0 {
				t.Fatalf("counts = %+v", counts)
			}
		})
	}
}

func TestReconcileNativeProfileDoesNotFetchIssues(t *testing.T) {
	t.Parallel()
	for _, skipRepository := range []bool{false, true} {
		t.Run(strconv.FormatBool(skipRepository), func(t *testing.T) {
			client := &scriptedRESTClient{t: t, steps: []restStep{{method: http.MethodGet, path: "/repos/org/repo", response: reconcileRepository{NodeID: "R_repo", Name: "repo", Owner: restActor{Login: "org"}, UpdatedAt: time.Now().UTC()}}}}
			if !skipRepository {
				client.steps = append(client.steps, restStep{method: http.MethodGet, path: "/repos/org/repo/pulls?direction=asc&per_page=100&sort=updated&state=all", response: []reconcilePullRequest{}})
			}
			_, err := NewReconciler(client).Reconcile(t.Context(), hubserver.ReconcileRequest{Profile: "native", Repository: hubserver.RepositoryTarget{Owner: "org", Name: "repo"}, SkipRepository: skipRepository})
			if err != nil || len(client.steps) != 0 {
				t.Fatalf("native reconciliation: %v, %d calls left", err, len(client.steps))
			}
		})
	}
}
