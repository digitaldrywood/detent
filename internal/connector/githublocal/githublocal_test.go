package githublocal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/connector/local"
)

func TestConnectorImportPersistsAndDetectsClosedUpstreamDivergence(t *testing.T) {
	t.Parallel()

	server := newGitHubLocalTestServer(t)
	dbPath := filepath.Join(t.TempDir(), "work-items.db")
	cfg := Config{
		GitHub: githubconnector.Config{
			Endpoint: server.URL + "/graphql",
			APIKey:   "ghp_test",
		},
		Local: local.Config{
			Path: dbPath,
		},
		Repository:     "digitaldrywood/detent",
		ActiveStates:   []string{"Todo", "In Progress"},
		TerminalStates: []string{"Done"},
		Now: func() time.Time {
			return time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
		},
	}

	conn, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	imported, err := conn.ImportIssues(context.Background(), []int{779}, "In Progress")
	if err != nil {
		t.Fatalf("ImportIssues() error = %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if len(imported) != 1 {
		t.Fatalf("imported len = %d, want 1", len(imported))
	}
	if imported[0].ID != "github:123:779" {
		t.Fatalf("imported ID = %q, want local surrogate", imported[0].ID)
	}
	if imported[0].Metadata[MetadataDivergence] != DivergenceClosedUpstreamLocalActive {
		t.Fatalf("divergence = %q, want %q", imported[0].Metadata[MetadataDivergence], DivergenceClosedUpstreamLocalActive)
	}
	if imported[0].Closed || imported[0].ClosedReason != "" {
		t.Fatalf("imported closed metadata = (%v, %q), want active local issue", imported[0].Closed, imported[0].ClosedReason)
	}

	restarted, err := New(cfg)
	if err != nil {
		t.Fatalf("restart New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := restarted.Close(); err != nil {
			t.Fatalf("restart Close() error = %v", err)
		}
	})
	issues, err := restarted.FetchIssuesByStates(context.Background(), []string{"In Progress"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues len = %d, want 1", len(issues))
	}
	if issues[0].ID != "github:123:779" || issues[0].State != "In Progress" {
		t.Fatalf("issue identity/state = %q/%q, want local surrogate/In Progress", issues[0].ID, issues[0].State)
	}
	if issues[0].Title != "Closed upstream issue" {
		t.Fatalf("Title = %q, want GitHub title", issues[0].Title)
	}
	if issues[0].Metadata[local.MetadataGitHubRepositoryID] != "123" || issues[0].Metadata[local.MetadataGitHubIssueNumber] != "779" {
		t.Fatalf("GitHub identity metadata = %#v", issues[0].Metadata)
	}
	if issues[0].Metadata[MetadataDivergence] != DivergenceClosedUpstreamLocalActive {
		t.Fatalf("divergence after restart = %q", issues[0].Metadata[MetadataDivergence])
	}
	if issues[0].Closed || issues[0].ClosedReason != "" {
		t.Fatalf("closed metadata after restart = (%v, %q), want active local issue", issues[0].Closed, issues[0].ClosedReason)
	}
}

func TestConnectorIssueMutatorsStayLocalAndPRLifecycleWritesPassThrough(t *testing.T) {
	t.Parallel()

	server := newGitHubLocalTestServer(t)
	conn, err := New(Config{
		GitHub: githubconnector.Config{
			Endpoint: server.URL + "/graphql",
			APIKey:   "ghp_test",
		},
		Local: local.Config{
			Path: filepath.Join(t.TempDir(), "guard-work-items.db"),
			Issues: []connector.Issue{{
				ID:         "github:123:779",
				Identifier: "digitaldrywood/detent#779",
				Title:      "Local issue",
				State:      "Todo",
				Fields:     map[string]string{},
				Metadata: map[string]string{
					local.MetadataGitHubNodeID:       "I_kwDOtest779",
					local.MetadataGitHubRepositoryID: "123",
					local.MetadataGitHubIssueNumber:  "779",
				},
				AssignedToWorker: true,
			}},
			TerminalStates: []string{"Done"},
		},
		Repository:     "digitaldrywood/detent",
		ActiveStates:   []string{"Todo", "In Progress"},
		TerminalStates: []string{"Done"},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	if err := conn.CreateComment(context.Background(), "github:123:779", "local audit"); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}
	if err := conn.UpdateIssueState(context.Background(), "github:123:779", "In Progress"); err != nil {
		t.Fatalf("UpdateIssueState() error = %v", err)
	}
	if err := conn.SetAssignee(context.Background(), "github:123:779", "detent-bot"); err != nil {
		t.Fatalf("SetAssignee() error = %v", err)
	}
	if err := conn.SetField(context.Background(), "github:123:779", "lease", "agent-1"); err != nil {
		t.Fatalf("SetField() error = %v", err)
	}
	if err := conn.SetIssueField(context.Background(), "github:123:779", 100, "claimed"); err != nil {
		t.Fatalf("SetIssueField() error = %v", err)
	}
	if err := conn.ClearIssueField(context.Background(), "github:123:779", 100); err != nil {
		t.Fatalf("ClearIssueField() error = %v", err)
	}
	if err := conn.CloseIssue(context.Background(), "github:123:779"); err != nil {
		t.Fatalf("CloseIssue() error = %v", err)
	}
	if got := server.writeRequests(); len(got) != 0 {
		t.Fatalf("issue mutators wrote to GitHub: %#v", got)
	}

	if err := conn.CreatePullRequestComment(context.Background(), "digitaldrywood/detent", 12, "ship it"); err != nil {
		t.Fatalf("CreatePullRequestComment() error = %v", err)
	}
	if err := conn.MergePullRequest(context.Background(), "digitaldrywood/detent", 12, "head-sha"); err != nil {
		t.Fatalf("MergePullRequest() error = %v", err)
	}
	got := server.writeRequests()
	want := []githubLocalTestRequest{
		{Method: http.MethodPost, Path: "/repos/digitaldrywood/detent/issues/12/comments"},
		{Method: http.MethodPut, Path: "/repos/digitaldrywood/detent/pulls/12/merge"},
	}
	if len(got) != len(want) {
		t.Fatalf("write request len = %d, want %d: %#v", len(got), len(want), got)
	}
	for index := range want {
		if got[index].Method != want[index].Method || got[index].Path != want[index].Path {
			t.Fatalf("write request[%d] = %#v, want %#v", index, got[index], want[index])
		}
	}
}

func TestConnectorFetchIssueStatesByIdentifiersReturnsLocalOnlyIssues(t *testing.T) {
	t.Parallel()

	server := newGitHubLocalTestServer(t)
	conn, err := New(Config{
		GitHub: githubconnector.Config{
			Endpoint: server.URL + "/graphql",
			APIKey:   "ghp_test",
		},
		Local: local.Config{
			Path: filepath.Join(t.TempDir(), "local-only-work-items.db"),
			Issues: []connector.Issue{{
				ID:         "external-123",
				Identifier: "external-123",
				Title:      "Runtime item",
				State:      "Todo",
				Fields:     map[string]string{},
			}},
		},
		Repository:   "digitaldrywood/detent",
		ActiveStates: []string{"Todo", "In Progress"},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	issues, err := conn.FetchIssueStatesByIdentifiers(context.Background(), []string{"external-123"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIdentifiers() error = %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues len = %d, want 1: %#v", len(issues), issues)
	}
	if issues[0].Identifier != "external-123" || issues[0].Title != "Runtime item" {
		t.Fatalf("issue = %#v, want local-only runtime item", issues[0])
	}
}

type githubLocalTestRequest struct {
	Method string
	Path   string
}

type githubLocalTestServer struct {
	*httptest.Server
	t        *testing.T
	mu       sync.Mutex
	requests []githubLocalTestRequest
}

func newGitHubLocalTestServer(t *testing.T) *githubLocalTestServer {
	t.Helper()
	testServer := &githubLocalTestServer{t: t}
	server := httptest.NewServer(http.HandlerFunc(testServer.handle))
	testServer.Server = server
	t.Cleanup(server.Close)
	return testServer
}

func (s *githubLocalTestServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.mu.Lock()
		s.requests = append(s.requests, githubLocalTestRequest{Method: r.Method, Path: r.URL.Path})
		s.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/digitaldrywood/detent":
		writeGitHubLocalJSON(s.t, w, map[string]any{
			"id":        123,
			"full_name": "digitaldrywood/detent",
			"html_url":  "https://github.com/digitaldrywood/detent",
		})
	case r.Method == http.MethodGet && r.URL.Path == "/repos/digitaldrywood/detent/issues/779":
		body := "Depends on: #1"
		writeGitHubLocalJSON(s.t, w, map[string]any{
			"node_id":      "I_kwDOtest779",
			"number":       779,
			"title":        "Closed upstream issue",
			"body":         body,
			"state":        "closed",
			"state_reason": "completed",
			"html_url":     "https://github.com/digitaldrywood/detent/issues/779",
			"created_at":   "2026-07-01T12:00:00Z",
			"updated_at":   "2026-07-02T12:00:00Z",
			"user":         map[string]any{"login": "octocat"},
			"assignees":    []map[string]any{{"node_id": "U_1", "login": "detent-bot"}},
			"labels":       []map[string]string{{"name": "enhancement"}},
		})
	case r.Method == http.MethodGet && r.URL.Path == "/repos/digitaldrywood/detent/pulls":
		writeGitHubLocalJSON(s.t, w, []map[string]any{})
	case r.Method == http.MethodPost && r.URL.Path == "/repos/digitaldrywood/detent/issues/12/comments":
		writeGitHubLocalJSON(s.t, w, map[string]any{"node_id": "comment-node"})
	case r.Method == http.MethodPut && r.URL.Path == "/repos/digitaldrywood/detent/pulls/12/merge":
		writeGitHubLocalJSON(s.t, w, map[string]any{"sha": "merge-sha", "merged": true, "message": "ok"})
	default:
		http.NotFound(w, r)
	}
}

func (s *githubLocalTestServer) writeRequests() []githubLocalTestRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]githubLocalTestRequest(nil), s.requests...)
}

func writeGitHubLocalJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
