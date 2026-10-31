package hubgithub

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	connectorgithub "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestGitHubAppBatchImport385IssuesEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("isolated HTTP GitHub App import through the default maintenance interval")
	}
	t.Parallel()
	const repository = "example-org/example-project"
	const total = 385
	const sourceBody = "Complete source issue body. "
	const sourceContract = "\n## Acceptance criteria\nPreserve the imported source.\n## Must not break\nExisting project work.\n## How we know it worked\nVerify imported data."
	const adminToken = "isolated-batch-import-administrator-token"
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	var graphQLRequests, installationRequests, tokenRequests atomic.Int64
	pageSizes := make(chan int, 4)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var response any
		switch request.URL.Path {
		case "/repos/" + repository + "/installation":
			installationRequests.Add(1)
			if request.Method != http.MethodGet || len(strings.Split(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "), ".")) != 3 {
				t.Error("repository discovery did not use the App JWT")
			}
			response = map[string]int{"id": 1}
		case "/app/installations/1/access_tokens":
			tokenRequests.Add(1)
			if request.Method != http.MethodPost {
				t.Error("installation token request must be POST")
			}
			response = map[string]any{"token": "fixture-installation-token", "expires_at": time.Now().Add(time.Hour)}
		case "/repos/" + repository:
			if request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer fixture-installation-token" {
				t.Error("repository binding did not use the installation token")
			}
			response = map[string]any{"id": 1, "node_id": "R_example", "name": "example-project", "owner": map[string]string{"login": "example-org"}, "updated_at": "2021-01-01T00:00:00Z"}
		case "/graphql":
			graphQLRequests.Add(1)
			if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer fixture-installation-token" {
				t.Error("issue page did not use the installation token")
			}
			var input struct {
				Query     string `json:"query"`
				Variables struct {
					Owner      string   `json:"owner"`
					Repository string   `json:"repo"`
					States     []string `json:"states"`
					Snapshots  bool     `json:"snapshots"`
					Cursor     *string  `json:"cursor"`
				} `json:"variables"`
			}
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if input.Variables.Owner != "example-org" || input.Variables.Repository != "example-project" || !input.Variables.Snapshots || !slices.Equal(input.Variables.States, []string{"OPEN"}) || !strings.Contains(input.Query, "issues(first:100,") {
				t.Error("issue page did not request complete open issues in pages of 100")
			}
			offset := 0
			if input.Variables.Cursor != nil {
				var parseErr error
				offset, parseErr = strconv.Atoi(*input.Variables.Cursor)
				if parseErr != nil {
					t.Error(parseErr)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			nodes := []any{}
			for number := offset + 1; number <= min(offset+100, total); number++ {
				lane := "detent:backlog"
				if number%3 == 0 {
					lane = "detent:todo"
				}
				nodes = append(nodes, map[string]any{
					"id": fmt.Sprintf("I_%d", number), "number": number, "url": fmt.Sprintf("https://github.com/%s/issues/%d", repository, number),
					"title": fmt.Sprintf("Source issue %d", number), "body": (strings.Repeat(sourceBody, 100) + sourceContract), "closed": false,
					"createdAt": "2020-01-01T00:00:00Z", "updatedAt": "2021-01-01T00:00:00Z", "author": map[string]string{"login": "source-user"},
					"labels":   map[string]any{"nodes": []any{map[string]string{"name": "bug"}, map[string]string{"name": "phase:one"}, map[string]string{"name": lane}}},
					"comments": map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"id": fmt.Sprintf("C_%d", number), "body": "Complete source comment", "createdAt": "2020-01-01T00:00:00Z", "updatedAt": "2021-01-01T00:00:00Z", "author": map[string]string{"login": "comment-user"}}}, "pageInfo": map[string]bool{"hasNextPage": false}},
				})
			}
			select {
			case pageSizes <- len(nodes):
			default:
				t.Error("unexpected extra issue page")
			}
			cursor := ""
			if offset+100 < total {
				cursor = strconv.Itoa(offset + 100)
			}
			response = map[string]any{"data": map[string]any{"repository": map[string]any{"nameWithOwner": repository, "issues": map[string]any{"totalCount": total, "nodes": nodes, "pageInfo": map[string]any{"hasNextPage": cursor != "", "endCursor": cursor}}}}}
		default:
			t.Errorf("unexpected source request: %s %s", request.Method, request.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(source.Close)
	transport := NewAppTransport(connectorgithub.InstallationTokenConfig{Endpoint: source.URL + "/graphql", AppID: "123", PrivateKey: privateKey, HTTPClient: source.Client()})
	service, err := hubserver.Open(t.Context(), hubserver.Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), InitialAdminToken: []byte(adminToken), ImportBackend: NewImporter(transport), ReconcileBackend: NewReconciler(transport), Content: newContentStore(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	api := func(ctx context.Context, method, path string, input, output any, status int) {
		t.Helper()
		var raw []byte
		if input != nil {
			var err error
			raw, err = json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
		}
		request, err := http.NewRequestWithContext(ctx, method, path, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+adminToken)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		service.Handler().ServeHTTP(recorder, request)
		if recorder.Code != status {
			t.Fatalf("%s %s: status=%d want=%d response=%s", method, path, recorder.Code, status, recorder.Body.String())
		}
		if output != nil {
			if err := json.Unmarshal(recorder.Body.Bytes(), output); err != nil {
				t.Fatal(err)
			}
		}
	}
	var organizations tracker.Page[struct {
		ID tracker.OrganizationID `json:"organization_id"`
	}]
	api(t.Context(), http.MethodGet, "/api/v2/organizations", nil, &organizations, http.StatusOK)
	if len(organizations.Items) != 1 {
		t.Fatalf("isolated organizations=%+v", organizations.Items)
	}
	base := "/api/v2/organizations/" + string(organizations.Items[0].ID) + "/projects"
	var project tracker.NativeProject
	api(t.Context(), http.MethodPost, base, map[string]any{"idempotency_key": "project", "name": "Import acceptance fixture", "states": []tracker.NativeState{{Name: "Backlog", Transitions: []string{"Todo"}}, {Name: "Todo", Dispatchable: true, Transitions: []string{"Backlog", "Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Backlog"}}}}, &project, http.StatusOK)
	base += "/" + string(project.ID)
	api(t.Context(), http.MethodPost, base+"/integration/repository", map[string]any{"idempotency_key": "bind", "expected_revision": "1", "repository": repository}, nil, http.StatusOK)
	var view struct {
		Batch    *tracker.GitHubBatch                            `json:"batch"`
		Progress struct{ Total, Completed, Failed, Pending int } `json:"progress"`
	}
	started := time.Now()
	api(t.Context(), http.MethodPost, base+"/onboarding/issue-intake", tracker.GitHubBatchCommand{Mutation: tracker.Mutation{IdempotencyKey: "start"}, Action: "start", Destination: "Backlog", AllowDispatch: true}, &view, http.StatusOK)
	if view.Batch == nil || !view.Batch.HubOwned || !view.Batch.Automatic || view.Batch.RunnerID != "" {
		t.Fatalf("job did not use the Hub App: %+v", view.Batch)
	}
	jobID := view.Batch.ID
	deadline, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	polling := time.NewTicker(100 * time.Millisecond)
	defer polling.Stop()
	for view.Batch.Status != "finished" {
		select {
		case <-deadline.Done():
			t.Fatalf("import did not finish: batch=%+v", view.Batch)
		case <-polling.C:
			api(deadline, http.MethodGet, base+"/onboarding/issue-intake", nil, &view, http.StatusOK)
		}
		if view.Batch.Status == "discovery_failed" {
			t.Fatalf("source page failed: %s", view.Batch.Error)
		}
	}
	elapsed := time.Since(started)
	if elapsed >= 10*time.Minute || view.Batch.ID != jobID || len(view.Batch.Items) != total || view.Progress.Total != total || view.Progress.Completed != total || view.Progress.Failed != 0 || view.Progress.Pending != 0 {
		t.Fatalf("job=%+v progress=%+v elapsed=%s", view.Batch, view.Progress, elapsed)
	}
	if graphQLRequests.Load() != 4 || installationRequests.Load() != 1 || tokenRequests.Load() != 1 {
		t.Fatalf("source requests: GraphQL=%d installation=%d token=%d", graphQLRequests.Load(), installationRequests.Load(), tokenRequests.Load())
	}
	for _, want := range []int{100, 100, 100, 85} {
		select {
		case got := <-pageSizes:
			if got != want {
				t.Fatalf("page size=%d want=%d", got, want)
			}
		default:
			t.Fatal("missing source page")
		}
	}
	backlog, todo := 0, 0
	for _, item := range view.Batch.Items {
		if item.Status != "completed" {
			t.Fatalf("item=%+v", item)
		}
		var issue tracker.NativeIssue
		api(t.Context(), http.MethodGet, base+"/work-items/"+string(item.WorkItemID), nil, &issue, http.StatusOK)
		want := "Backlog"
		if item.Number%3 == 0 {
			want = "Todo"
			todo++
		} else {
			backlog++
		}
		if issue.State != want || !slices.Equal(issue.Labels, []string{"bug", "phase:one"}) || issue.Body != (strings.Repeat(sourceBody, 100)+sourceContract) || issue.LinkedSource == nil || issue.LinkedSource.Status != "complete" || issue.Provenance == nil || issue.Provenance.ExternalID != fmt.Sprintf("I_%d", item.Number) {
			t.Fatalf("imported item=%+v issue=%+v", item, issue)
		}
	}
	t.Logf("isolated_http_fixture issues=%d job_seconds=%.3f default_maintenance_seconds=%.0f graphql_pages=4 page_sizes=100,100,100,85 backlog=%d todo=%d labels=bug,phase:one live_github=false", total, elapsed.Seconds(), hubserver.DefaultWebhookMaintenanceInterval.Seconds(), backlog, todo)
}
