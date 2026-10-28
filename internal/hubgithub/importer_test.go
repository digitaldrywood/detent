package hubgithub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	connectorgithub "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/tracker"
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
	pageClient *scriptedRESTClient
	response   json.RawMessage
	err        error
}

func (c editFixtureClient) REST(context.Context, string, string, any, any) error {
	return errors.New("unexpected REST")
}
func (c editFixtureClient) RESTPage(ctx context.Context, path string, output any) (string, error) {
	if c.pageClient != nil {
		return c.pageClient.RESTPage(ctx, path, output)
	}
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

func TestImportScheduledOccurrence(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		changeRun func(map[string]any)
		changeJob func(map[string]any)
		status    int
		valid     bool
	}{
		{name: "failed job while workflow is running", valid: true},
		{name: "wrong run", changeRun: func(r map[string]any) { r["id"] = 124 }},
		{name: "wrong attempt", changeRun: func(r map[string]any) { r["run_attempt"] = 2 }},
		{name: "unproven tested SHA", changeRun: func(r map[string]any) { r["head_sha"] = strings.Repeat("b", 40) }},
		{name: "other workflow", changeRun: func(r map[string]any) { r["path"] = ".github/workflows/release.yml" }},
		{name: "other repository", changeRun: func(r map[string]any) { r["repository"] = map[string]any{"full_name": "elsewhere/repo"} }},
		{name: "fork", changeRun: func(r map[string]any) { r["head_repository"] = map[string]any{"full_name": "fork/repo"} }},
		{name: "feature branch", changeRun: func(r map[string]any) { r["head_branch"] = "feature" }},
		{name: "pull request event", changeRun: func(r map[string]any) { r["event"] = "pull_request" }},
		{name: "job absent from attempt", changeJob: func(j map[string]any) { j["id"] = 457 }},
		{name: "job from other run", changeJob: func(j map[string]any) { j["run_id"] = 124 }},
		{name: "job head mismatch", changeJob: func(j map[string]any) { j["head_sha"] = strings.Repeat("b", 40) }},
		{name: "job name mismatch", changeJob: func(j map[string]any) { j["name"] = "Deploy" }},
		{name: "running job", changeJob: func(j map[string]any) { j["status"] = "in_progress" }},
		{name: "green job", changeJob: func(j map[string]any) { j["conclusion"] = "success" }},
		{name: "403", status: http.StatusForbidden},
		{name: "404", status: http.StatusNotFound},
		{name: "429", status: http.StatusTooManyRequests},
	} {
		t.Run(test.name, func(t *testing.T) {
			sha := strings.Repeat("a", 40)
			evidence := gate.ScheduledEvidence{Schema: 1, Repository: "org/repo", OccurrenceKey: strings.Repeat("a", 64), RunID: "123", RunAttempt: "1", JobID: "456", JobName: "Coverage", RunURL: "https://github.com/org/repo/actions/runs/123/attempts/1", JobURL: "https://github.com/org/repo/actions/jobs/456", HeadSHA: sha, Conclusion: "failure"}
			run := map[string]any{"id": 123, "run_attempt": 1, "path": ".github/workflows/ci.yml", "head_sha": sha, "head_branch": "develop", "event": "schedule", "status": "in_progress", "repository": map[string]any{"full_name": "org/repo"}, "head_repository": map[string]any{"full_name": "org/repo"}}
			job := map[string]any{"id": 456, "run_id": 123, "name": "Coverage", "head_sha": sha, "status": "completed", "conclusion": "failure", "started_at": "2026-10-11T12:00:00Z", "completed_at": "2026-10-11T12:01:00Z"}
			if test.changeRun != nil {
				test.changeRun(run)
			}
			if test.changeJob != nil {
				test.changeJob(job)
			}
			path := "/repos/org/repo/actions/runs/123/attempts/1"
			steps := []restStep{{method: http.MethodGet, path: path, response: run}}
			if test.status != 0 {
				steps[0].err = &connectorgithub.StatusError{StatusCode: test.status}
			}
			if test.changeRun == nil && test.status == 0 {
				steps = append(steps, restStep{method: http.MethodGet, path: path + "/jobs?per_page=100", response: map[string]any{"jobs": []map[string]any{job}}})
			}
			client := &scriptedRESTClient{t: t, steps: steps}
			page, err := NewImporter(client).FetchImportPage(t.Context(), hubserver.GitHubImportRequest{Profile: "native", Repository: "org/repo", Stage: "scheduled", Scheduled: &evidence})
			if (err == nil) != test.valid || (page.Scheduled != nil) != test.valid {
				t.Fatalf("page=%+v err=%v", page, err)
			}
			if test.valid && (page.Scheduled.HeadSHA != sha || page.Scheduled.StartedAt.Format(time.RFC3339) != "2026-10-11T12:00:00Z" || page.Scheduled.FinishedAt.Format(time.RFC3339) != "2026-10-11T12:01:00Z") {
				t.Fatalf("verified evidence = %+v", page.Scheduled)
			}
			client.assertDone()
		})
	}
}

func TestImportBatchCompletesDiscussionPages(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		incomplete bool
	}{{name: "101 comments across pages"}, {name: "incomplete REST page refused", incomplete: true}} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			comment := func(number int) map[string]any {
				return map[string]any{"node_id": "C_" + strconv.Itoa(number), "body": "Complete comment", "created_at": "2020-01-01T00:00:00Z", "updated_at": "2021-01-01T00:00:00Z", "user": map[string]string{"login": "source-user"}}
			}
			var first []any
			for number := 1; number <= 100; number++ {
				first = append(first, comment(number))
			}
			raw, err := json.Marshal(first)
			if err != nil {
				t.Fatal(err)
			}
			next := "/repos/example-org/example-project/issues/12/comments?per_page=100&page=2"
			pages := &scriptedRESTClient{t: t, steps: []restStep{{method: http.MethodGet, path: "/repos/example-org/example-project/issues/12/comments?per_page=100", response: json.RawMessage(raw), next: next}}}
			last := []any{comment(101)}
			if test.incomplete {
				last = nil
			}
			raw, err = json.Marshal(last)
			if err != nil {
				t.Fatal(err)
			}
			pages.steps = append(pages.steps, restStep{method: http.MethodGet, path: next, response: json.RawMessage(raw)})
			response := json.RawMessage(`{"repository":{"nameWithOwner":"example-org/example-project","issues":{"totalCount":1,"nodes":[{"id":"I_12","number":12,"url":"https://github.com/example-org/example-project/issues/12","title":"Issue","body":"Complete body","createdAt":"2020-01-01T00:00:00Z","updatedAt":"2021-01-01T00:00:00Z","author":{"login":"source-user"},"labels":{"nodes":[{"name":"bug"}]},"comments":{"totalCount":101,"nodes":[],"pageInfo":{"hasNextPage":true}}}],"pageInfo":{"hasNextPage":false}}}}`)
			importer := NewImporter(editFixtureClient{response: response, pageClient: pages})
			discovery := tracker.GitHubDiscovery{Repository: "example-org/example-project", Snapshots: true}
			result, err := importer.FetchImportPage(t.Context(), hubserver.GitHubImportRequest{Profile: "native", Repository: discovery.Repository, Stage: "batch", Discovery: &discovery})
			if test.incomplete {
				if err == nil || result.BatchPage != nil {
					t.Fatalf("partial discussion accepted: %+v %v", result, err)
				}
			} else {
				if err != nil || result.BatchPage == nil {
					t.Fatalf("batch=%+v %v", result, err)
				}
				issue := result.BatchPage.Issues[0]
				if issue.NeedsDiscussion || issue.Snapshot == nil || len(issue.Snapshot.Comments) != 101 || issue.Snapshot.Comments[100].Provenance.ExternalID != "C_101" {
					t.Fatalf("discussion=%+v", issue)
				}
			}
		})
	}
}

func TestScheduledRangeRequiresCompleteAncestry(t *testing.T) {
	for _, test := range []struct {
		name, body string
		count      int
	}{
		{"complete", `{"status":"ahead","total_commits":2,"commits":[{"sha":"first"},{"sha":"second"}]}`, 2},
		{"truncated", `{"status":"ahead","total_commits":101,"commits":[{"sha":"first"}]}`, 0},
		{"diverged", `{"status":"diverged","total_commits":1,"commits":[{"sha":"first"}]}`, 0},
		{"same head", `{"status":"identical","total_commits":0,"commits":[]}`, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &scriptedRESTClient{t: t, steps: []restStep{{method: http.MethodGet, path: "/repos/example-org/example-project/compare/base...head?per_page=100", response: json.RawMessage(test.body)}}}
			page, err := NewImporter(client).FetchImportPage(t.Context(), hubserver.GitHubImportRequest{Repository: "example-org/example-project", Stage: "scheduled-range", CompareBase: "base", CompareHead: "head"})
			if err != nil || len(page.Commits) != test.count {
				t.Fatalf("commits=%v error=%v", page.Commits, err)
			}
		})
	}
}
