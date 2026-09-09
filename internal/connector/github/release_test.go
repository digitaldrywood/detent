package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/publication"
	releasepkg "github.com/digitaldrywood/detent/internal/release"
)

func TestConnectorInspectReleaseRepository(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/example/repo":
			writeReleaseJSON(t, w, map[string]any{"default_branch": "main"})
		case "/repos/example/repo/git/ref/heads/main":
			writeReleaseJSON(t, w, map[string]any{"object": map[string]any{"sha": "head"}})
		case "/repos/example/repo/tags":
			writeReleaseJSON(t, w, []map[string]any{{"name": "v1.2.3"}, {"name": "not-a-release"}})
		case "/repos/example/repo/commits/v1.2.3":
			writeReleaseJSON(t, w, map[string]any{"sha": "previous", "commit": map[string]any{"committer": map[string]any{"date": "2026-07-09T20:00:00Z"}}})
		case "/repos/example/repo/compare/v1.2.3...head":
			writeReleaseJSON(t, w, map[string]any{"commits": []map[string]any{{"sha": "head", "commit": map[string]any{"message": "feat: release cadence", "committer": map[string]any{"date": "2026-07-10T20:00:00Z"}}}}})
		case "/repos/example/repo/commits/head/pulls":
			writeReleaseJSON(t, w, []map[string]any{{"number": 9, "body": "Fixes #1204"}})
		case "/repos/example/repo/commits/head/check-runs":
			writeReleaseJSON(t, w, map[string]any{"check_runs": []map[string]any{{"head_sha": "head", "name": "CI", "status": "completed", "conclusion": "success", "details_url": "https://github.com/example/repo/actions/runs/42/job/1"}}})
		case "/repos/example/repo/commits/head/status":
			writeReleaseJSON(t, w, map[string]any{"statuses": []any{}})
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	conn, err := NewConnector(Config{Endpoint: server.URL + "/graphql", APIKey: "token", Repository: "example/repo", GitHubStatusSource: GitHubStatusSourceLabel, RequiredStatusChecks: []string{"CI"}})
	if err != nil {
		t.Fatalf("NewConnector() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	got, err := conn.Inspect(t.Context())
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if got.Name != "example/repo" || got.HeadSHA != "head" || got.LatestTag != "v1.2.3" || got.LatestSHA != "previous" {
		t.Fatalf("Inspect() repository = %#v", got)
	}
	if len(got.Commits) != 1 || got.Commits[0].Message != "feat: release cadence" || len(got.Commits[0].IssueRefs) != 1 || got.Commits[0].IssueRefs[0] != "example/repo#1204" {
		t.Fatalf("Inspect() commits = %#v", got.Commits)
	}
	if len(got.RequiredCheckNames) != 1 || got.RequiredCheckNames[0] != "CI" {
		t.Fatalf("required checks = %v", got.RequiredCheckNames)
	}
	if len(got.Checks) != 1 || got.Checks[0].RunID != 42 || got.Checks[0].Conclusion != "success" {
		t.Fatalf("Inspect() checks = %#v", got.Checks)
	}
	wantTaggedAt := time.Date(2026, time.July, 9, 20, 0, 0, 0, time.UTC)
	if !got.TaggedAt.Equal(wantTaggedAt) {
		t.Fatalf("Inspect() TaggedAt = %s, want %s", got.TaggedAt, wantTaggedAt)
	}
}

func TestConnectorCreateTagPublishesAnnotatedReference(t *testing.T) {
	t.Parallel()

	requests := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/example/repo/commits/refs/tags/v1.3.0":
			http.Error(w, "missing", http.StatusNotFound)
		case "/repos/example/repo/git/tags":
			writeReleaseJSON(t, w, map[string]any{"sha": "tag-object"})
		case "/repos/example/repo/git/refs":
			writeReleaseJSON(t, w, map[string]any{"ref": "refs/tags/v1.3.0"})
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	conn, err := NewConnector(Config{Endpoint: server.URL + "/graphql", APIKey: "token", Repository: "example/repo", GitHubStatusSource: GitHubStatusSourceLabel})
	if err != nil {
		t.Fatalf("NewConnector() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.CreateTag(t.Context(), releasepkg.Tag{Name: "v1.3.0", SHA: "head", Message: "notes"}); err != nil {
		t.Fatalf("CreateTag() error = %v", err)
	}
	<-requests
	if first, second := <-requests, <-requests; first != "/repos/example/repo/git/tags" || second != "/repos/example/repo/git/refs" {
		t.Fatalf("CreateTag() paths = %q, %q", first, second)
	}
}

func TestReleaseReportsReconcileResponseLoss(t *testing.T) {
	t.Parallel()
	for _, lost := range []bool{false, true} {
		t.Run(strconv.FormatBool(lost), func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			comments := make([]map[string]string, 100)
			for i := range comments {
				comments[i] = map[string]string{"body": "unrelated"}
			}
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.URL.Path != "/repos/example/repo/issues/9/comments" {
					t.Errorf("unexpected path %s", r.URL.Path)
					http.Error(w, "unexpected", 404)
					return
				}
				if r.Method == http.MethodGet {
					if r.URL.Query().Get("page") == "1" {
						writeReleaseJSON(t, w, comments[:100])
					} else {
						writeReleaseJSON(t, w, comments[100:])
					}
					return
				}
				posts++
				var comment map[string]string
				if err := json.NewDecoder(r.Body).Decode(&comment); err != nil {
					t.Error(err)
				}
				comments = append(comments, comment)
				if lost {
					http.Error(w, "response lost", 500)
					return
				}
				writeReleaseJSON(t, w, comment)
			}))
			t.Cleanup(server.Close)
			for range 2 {
				conn, err := NewConnector(Config{Endpoint: server.URL + "/graphql", APIKey: "token", Repository: "example/repo", GitHubStatusSource: GitHubStatusSourceLabel})
				if err != nil {
					t.Fatal(err)
				}
				_, err = conn.EnsureReleaseReport(t.Context(), releasepkg.Report{IssueRefs: []string{"example/repo#9"}, Fingerprint: "retry:head", Body: "evidence"})
				if err != nil {
					t.Fatal(err)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if posts != 1 {
				t.Fatalf("posts = %d", posts)
			}
		})
	}
}

func TestReleaseTagReconciliation(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"head", "other"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			published := false
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch r.URL.Path {
				case "/repos/example/repo/commits/refs/tags/v1.3.0":
					if !published {
						http.Error(w, "missing", 404)
						return
					}
					writeReleaseJSON(t, w, map[string]string{"sha": target})
				case "/repos/example/repo/git/tags":
					writeReleaseJSON(t, w, map[string]string{"sha": "object"})
				case "/repos/example/repo/git/refs":
					posts++
					published = true
					http.Error(w, "lost response", 500)
				default:
					t.Errorf("unexpected %s", r.URL.Path)
				}
			}))
			t.Cleanup(server.Close)
			for range 2 {
				conn, err := NewConnector(Config{Endpoint: server.URL + "/graphql", APIKey: "token", Repository: "example/repo", GitHubStatusSource: GitHubStatusSourceLabel})
				if err != nil {
					t.Fatal(err)
				}
				err = conn.CreateTag(t.Context(), releasepkg.Tag{Name: "v1.3.0", SHA: "head"})
				if (err != nil) != (target == "other") {
					t.Fatalf("CreateTag = %v", err)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if posts != 1 {
				t.Fatalf("tag posts = %d", posts)
			}
		})
	}
}

func writeReleaseJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func TestReleaseCheckEvidenceCompleteness(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                    string
		checkCount, statusCount int
		wantError               bool
	}{
		{name: "complete", checkCount: 1, statusCount: 1},
		{name: "truncated checks", checkCount: 2, statusCount: 1, wantError: true},
		{name: "truncated statuses", checkCount: 1, statusCount: 2, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/example/repo/commits/head/check-runs":
					writeReleaseJSON(t, w, map[string]any{"total_count": tc.checkCount, "check_runs": []map[string]any{{"head_sha": "head", "name": "CI", "status": "completed", "conclusion": "success"}}})
				case "/repos/example/repo/commits/head/status":
					writeReleaseJSON(t, w, map[string]any{"sha": "head", "total_count": tc.statusCount, "statuses": []map[string]any{{"context": "Security", "state": "success"}}})
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			}))
			t.Cleanup(server.Close)
			conn, err := NewConnector(Config{Endpoint: server.URL + "/graphql", APIKey: "token", Repository: "example/repo", GitHubStatusSource: GitHubStatusSourceLabel})
			if err != nil {
				t.Fatal(err)
			}
			checks, err := conn.releaseChecks(t.Context(), "/repos/example/repo", "head")
			if (err != nil) != tc.wantError {
				t.Fatalf("releaseChecks = %v", err)
			}
			if !tc.wantError && (len(checks) != 2 || checks[0].SHA != "head" || checks[1].SHA != "head" || checks[1].Name != "Security") {
				t.Fatalf("checks = %#v", checks)
			}
		})
	}
}

func TestReleaseTagOriginRecovery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, message string
		wantError     bool
	}{
		{"metadata", `notes\n<!-- detent-release-origins:["example/repo#9"] -->`, false},
		{"invalid metadata", `<!-- detent-release-origins:broken -->`, true},
		{"legacy tag", "notes", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/example/repo/git/ref/tags/v1.0.0":
					writeReleaseJSON(t, w, map[string]any{"object": map[string]string{"sha": "object", "type": "tag"}})
				case "/repos/example/repo/git/tags/object":
					writeReleaseJSON(t, w, map[string]string{"message": tc.message})
				case "/repos/example/repo/commits/head/pulls":
					writeReleaseJSON(t, w, []map[string]any{{"number": 10, "body": "Fixes #9"}})
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			}))
			t.Cleanup(server.Close)
			conn, err := NewConnector(Config{Endpoint: server.URL + "/graphql", APIKey: "token", Repository: "example/repo", GitHubStatusSource: GitHubStatusSourceLabel})
			if err != nil {
				t.Fatal(err)
			}
			refs, err := conn.releaseTagOrigins(t.Context(), "/repos/example/repo", "v1.0.0", "head")
			if (err != nil) != tc.wantError {
				t.Fatalf("releaseTagOrigins = %v", err)
			}
			if !tc.wantError && (len(refs) != 1 || refs[0] != "example/repo#9") {
				t.Fatalf("refs = %v", refs)
			}
		})
	}
}

func TestReleaseRerunReconciliation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, sha, status  string
		attempt, wantPosts int
		wantError          bool
	}{
		{"first attempt", "head", "completed", 1, 1, false},
		{"manual rerun does not consume automatic allowance", "head", "completed", 2, 1, false},
		{"pending", "head", "queued", 1, 0, false},
		{"stale", "other", "completed", 1, 0, true},
		{"missing attempt", "head", "completed", 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					writeReleaseJSON(t, w, map[string]any{"head_sha": tc.sha, "status": tc.status, "run_attempt": tc.attempt})
					return
				}
				posts++
				w.WriteHeader(http.StatusCreated)
			}))
			t.Cleanup(server.Close)
			conn, err := NewConnector(Config{Endpoint: server.URL + "/graphql", APIKey: "token", Repository: "example/repo", GitHubStatusSource: GitHubStatusSourceLabel})
			if err != nil {
				t.Fatal(err)
			}
			err = conn.RerunFailedChecks(t.Context(), []releasepkg.Check{{SHA: "head", RunID: 42}, {SHA: "head", RunID: 42}})
			if (err != nil) != tc.wantError || posts != tc.wantPosts {
				t.Fatalf("rerun error=%v posts=%d", err, posts)
			}
		})
	}
}

func TestReleaseCrossRepositoryOrigins(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeReleaseJSON(t, w, []map[string]any{{"number": 10, "body": "Fixes other/repo#9\nFixes #8"}})
	}))
	t.Cleanup(server.Close)
	conn, err := NewConnector(Config{Endpoint: server.URL + "/graphql", APIKey: "token", Repository: "example/repo", GitHubStatusSource: GitHubStatusSourceLabel})
	if err != nil {
		t.Fatal(err)
	}
	refs, err := conn.releaseCommitIssueRefs(t.Context(), "/repos/example/repo", "head")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0] != "example/repo#8" || refs[1] != "other/repo#9" {
		t.Fatalf("refs = %v", refs)
	}
	if _, err := conn.EnsureReleaseReport(t.Context(), releasepkg.Report{IssueRefs: []string{"other/repo#9"}}); err == nil {
		t.Fatal("foreign-only origin must fail closed")
	}
}

func TestReleaseReportPublicationProtection(t *testing.T) {
	t.Parallel()
	for _, visibility := range []publication.Visibility{publication.VisibilityPublic, publication.VisibilityPrivate} {
		t.Run(string(visibility), func(t *testing.T) {
			t.Parallel()
			bodies := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					writeReleaseJSON(t, w, []any{})
					return
				}
				var payload struct {
					Body string `json:"body"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				bodies <- payload.Body
				writeReleaseJSON(t, w, map[string]string{"node_id": "comment"})
			}))
			t.Cleanup(server.Close)
			conn, err := NewConnector(Config{Endpoint: server.URL + "/graphql", APIKey: "token", Repository: "example/repo", GitHubStatusSource: GitHubStatusSourceLabel, Publication: publication.Policy{Sources: []publication.Source{{Repository: "private/source"}}}})
			if err != nil {
				t.Fatal(err)
			}
			conn.publicationVisibilities = map[string]publication.Visibility{"example/repo": visibility}
			_, err = conn.EnsureReleaseReport(t.Context(), releasepkg.Report{IssueRefs: []string{"example/repo#9"}, Fingerprint: "failure:head", Body: "Failure in private/source#42"})
			if err != nil {
				t.Fatal(err)
			}
			body := <-bodies
			if strings.Contains(body, "private/source") != (visibility == publication.VisibilityPrivate) || !strings.Contains(body, "<!-- detent-auto-release:failure:head -->") {
				t.Fatalf("body = %s", body)
			}
		})
	}
}
