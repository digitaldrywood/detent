package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
		case "/repos/example/repo/compare/v1.2.3...main":
			writeReleaseJSON(t, w, map[string]any{"commits": []map[string]any{{"sha": "head", "commit": map[string]any{"message": "feat: release cadence", "committer": map[string]any{"date": "2026-07-10T20:00:00Z"}}}}})
		case "/repos/example/repo/commits/head/pulls":
			writeReleaseJSON(t, w, []map[string]any{{"number": 9, "body": "Fixes #1204"}})
		case "/repos/example/repo/commits/head/check-runs":
			writeReleaseJSON(t, w, map[string]any{"check_runs": []map[string]any{{"name": "CI", "status": "completed", "conclusion": "success", "details_url": "https://github.com/example/repo/actions/runs/42/job/1"}}})
		case "/repos/example/repo/commits/head/status":
			writeReleaseJSON(t, w, map[string]any{"statuses": []any{}})
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	conn, err := NewConnector(Config{Endpoint: server.URL + "/graphql", APIKey: "token", Repository: "example/repo", GitHubStatusSource: GitHubStatusSourceLabel})
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

	requests := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
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
	if first, second := <-requests, <-requests; first != "/repos/example/repo/git/tags" || second != "/repos/example/repo/git/refs" {
		t.Fatalf("CreateTag() paths = %q, %q", first, second)
	}
}

func writeReleaseJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
