package factory

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

func TestNewFromConfigSupportedBackends(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind string
		want string
	}{
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

	for _, kind := range []string{"", "memory", "linear", "local_sqlite", "github_local", "asana"} {
		t.Run(kind, func(t *testing.T) {
			got, err := NewFromConfig(Config{Kind: kind})
			if got != nil {
				t.Fatalf("connector = %T, want nil", got)
			}
			if !errors.Is(err, ErrUnsupportedBackend) {
				t.Fatalf("error = %v, want ErrUnsupportedBackend", err)
			}
		})
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
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

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
	if !errors.Is(err, githubconnector.ErrRESTFanoutDeferred) {
		t.Fatalf("FetchStatusDrift() error = %v, want ErrRESTFanoutDeferred", err)
	}
	if strings.Contains(defaultLogs.String(), "github rest budget preserved") {
		t.Fatalf("default logger received connector warning:\n%s", defaultLogs.String())
	}
}
