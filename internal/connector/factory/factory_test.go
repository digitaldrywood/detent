package factory

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/connector/local"
	"github.com/digitaldrywood/detent/internal/connector/memory"
)

func TestNewFromConfigSupportedBackends(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind string
		want string
	}{
		{name: "empty defaults to memory", kind: "", want: "memory"},
		{name: "memory", kind: "memory", want: "memory"},
		{name: "linear", kind: "linear", want: "linear"},
		{name: "github", kind: "github", want: "github"},
		{name: "normalizes whitespace and case", kind: " GitHub ", want: "github"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := NewFromConfig(Config{Kind: tt.kind})
			if err != nil {
				t.Fatalf("NewFromConfig() error = %v", err)
			}
			if got.Name() != tt.want {
				t.Fatalf("Name() = %q, want %q", got.Name(), tt.want)
			}
		})
	}
}

func TestNewFromConfigRejectsNotReadyBackends(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"gitlab", "jira"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			got, err := NewFromConfig(Config{Kind: kind})
			if got != nil {
				t.Fatalf("connector = %T, want nil", got)
			}
			if !errors.Is(err, ErrBackendNotReady) {
				t.Fatalf("error = %v, want ErrBackendNotReady", err)
			}
		})
	}
}

func TestNewFromConfigRejectsUnknownBackend(t *testing.T) {
	t.Parallel()

	got, err := NewFromConfig(Config{Kind: "asana"})
	if got != nil {
		t.Fatalf("connector = %T, want nil", got)
	}
	if !errors.Is(err, ErrUnsupportedBackend) {
		t.Fatalf("error = %v, want ErrUnsupportedBackend", err)
	}
}

func TestFactoryMemoryConnectorUsesConfiguredIssues(t *testing.T) {
	t.Parallel()

	issues := []connector.Issue{{ID: "issue-1", State: "Todo"}}
	var events []memory.Event
	c, err := NewFromConfig(Config{
		Kind: "memory",
		Memory: memory.Config{
			Issues: issues,
			EventSink: func(event memory.Event) {
				events = append(events, event)
			},
		},
	})
	if err != nil {
		t.Fatalf("NewFromConfig() error = %v", err)
	}

	got, err := c.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if !reflect.DeepEqual(got, issues) {
		t.Fatalf("FetchCandidateIssues() = %#v, want %#v", got, issues)
	}

	if err := c.CreateComment(context.Background(), "issue-1", "body"); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}
	if !reflect.DeepEqual(events, []memory.Event{{Kind: memory.EventKindComment, IssueID: "issue-1", Body: "body"}}) {
		t.Fatalf("events = %#v, want comment event", events)
	}
}

func TestFactoryGitHubConnectorRequiresProjectForPolling(t *testing.T) {
	t.Parallel()

	c, err := NewFromConfig(Config{Kind: "github"})
	if err != nil {
		t.Fatalf("NewFromConfig() error = %v", err)
	}

	if _, err := c.FetchCandidateIssues(context.Background()); !errors.Is(err, githubconnector.ErrMissingProject) {
		t.Fatalf("FetchCandidateIssues() error = %v, want ErrMissingProject", err)
	}
}

func TestFactoryGitHubIssueFieldConnectorRequiresRepositoryForPolling(t *testing.T) {
	t.Parallel()

	c, err := NewFromConfig(Config{
		Kind:               "github",
		GitHubStatusSource: githubconnector.GitHubStatusSourceIssueField,
	})
	if err != nil {
		t.Fatalf("NewFromConfig() error = %v", err)
	}

	if _, err := c.FetchCandidateIssues(context.Background()); !errors.Is(err, githubconnector.ErrMissingRepository) {
		t.Fatalf("FetchCandidateIssues() error = %v, want ErrMissingRepository", err)
	}
}

func TestFactoryGitHubLabelConnectorRequiresRepositoryForPolling(t *testing.T) {
	t.Parallel()

	c, err := NewFromConfig(Config{
		Kind:               "github",
		GitHubStatusSource: githubconnector.GitHubStatusSourceLabel,
	})
	if err != nil {
		t.Fatalf("NewFromConfig() error = %v", err)
	}

	if _, err := c.FetchCandidateIssues(context.Background()); !errors.Is(err, githubconnector.ErrMissingRepository) {
		t.Fatalf("FetchCandidateIssues() error = %v, want ErrMissingRepository", err)
	}
}

func TestFactoryGitHubConnectorImplementsAuthenticator(t *testing.T) {
	t.Parallel()

	c, err := NewFromConfig(Config{Kind: "github"})
	if err != nil {
		t.Fatalf("NewFromConfig() error = %v", err)
	}
	if _, ok := c.(connector.Authenticator); !ok {
		t.Fatalf("connector = %T, want connector.Authenticator", c)
	}
}

func TestFactoryLinearConnectorSupportsIssueCommentsOnly(t *testing.T) {
	t.Parallel()

	c, err := NewFromConfig(Config{Kind: "linear"})
	if err != nil {
		t.Fatalf("NewFromConfig() error = %v", err)
	}
	if _, ok := c.(connector.IssueCommentReader); !ok {
		t.Fatalf("connector = %T, want connector.IssueCommentReader", c)
	}
	if _, ok := c.(connector.PullRequestCommenter); ok {
		t.Fatalf("connector = %T, want no connector.PullRequestCommenter", c)
	}
}

func TestFactoryGitHubLocalConnectorSelection(t *testing.T) {
	t.Parallel()

	c, err := NewFromConfig(Config{
		Kind:       "github_local",
		Repository: "digitaldrywood/detent",
		LocalSQLite: local.Config{
			Path: ":memory:",
		},
		ActiveStates:   []string{"Todo", "In Progress"},
		TerminalStates: []string{"Done"},
	})
	if err != nil {
		t.Fatalf("NewFromConfig() error = %v", err)
	}
	if closer, ok := c.(connector.Closer); ok {
		t.Cleanup(func() {
			if err := closer.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
		})
	}
	if c.Name() != "github_local" {
		t.Fatalf("Name() = %q, want github_local", c.Name())
	}
	if _, ok := c.(connector.PullRequestCommenter); !ok {
		t.Fatalf("connector = %T, want connector.PullRequestCommenter", c)
	}
	if _, ok := c.(connector.PullRequestMerger); !ok {
		t.Fatalf("connector = %T, want connector.PullRequestMerger", c)
	}
}

func TestFactoryGitHubConnectorImplementsProvisioner(t *testing.T) {
	t.Parallel()

	c, err := NewFromConfig(Config{Kind: "github"})
	if err != nil {
		t.Fatalf("NewFromConfig() error = %v", err)
	}
	if _, ok := c.(connector.Provisioner); !ok {
		t.Fatalf("connector = %T, want connector.Provisioner", c)
	}
}

func TestFactoryGitHubConnectorUsesConfiguredLogger(t *testing.T) {
	var defaultLogs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&defaultLogs, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	})))
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/digitaldrywood/detent/issues":
			if got := r.URL.Query().Get("state"); got != "open" {
				t.Fatalf("state query = %q, want open", got)
			}
		case "/search/issues":
			if !strings.Contains(r.URL.Query().Get("q"), "is:closed") {
				t.Fatalf("search query = %q, want closed issue search", r.URL.Query().Get("q"))
			}
		default:
			t.Fatalf("path = %s, want repository issues or search path", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/search/issues" {
			_, _ = w.Write([]byte(`{"total_count":0,"items":[]}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(server.Close)

	c, err := NewFromConfig(Config{
		Kind:                        "github",
		Endpoint:                    server.URL + "/graphql",
		APIKey:                      "token",
		GitHubStatusSource:          githubconnector.GitHubStatusSourceLabel,
		Repository:                  "digitaldrywood/detent",
		GitHubRESTFanoutMaxRequests: 1,
		Logger:                      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewFromConfig() error = %v", err)
	}

	driftReader, ok := c.(connector.StatusDriftReader)
	if !ok {
		t.Fatalf("connector = %T, want connector.StatusDriftReader", c)
	}
	if _, err := driftReader.FetchStatusDrift(context.Background()); err != nil {
		t.Fatalf("FetchStatusDrift() first error = %v", err)
	}
	_, err = driftReader.FetchStatusDrift(context.Background())
	if !errors.Is(err, githubconnector.ErrRESTBudgetReserved) {
		t.Fatalf("FetchStatusDrift() error = %v, want ErrRESTBudgetReserved", err)
	}
	if strings.Contains(defaultLogs.String(), "github rest budget preserved") {
		t.Fatalf("default logger received connector warning:\n%s", defaultLogs.String())
	}
}
