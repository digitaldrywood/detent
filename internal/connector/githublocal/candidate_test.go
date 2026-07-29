package githublocal

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/connector/local"
)

func TestConnectorReadCandidatesUsesLocalStateStore(t *testing.T) {
	t.Parallel()

	server := newGitHubLocalTestServer(t)
	c, err := New(Config{
		GitHub: githubconnector.Config{
			Endpoint: server.URL + "/graphql",
			APIKey:   "token",
		},
		Local: local.Config{
			Path: filepath.Join(t.TempDir(), "candidate.db"),
		},
		Repository: "digitaldrywood/detent",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	got, err := c.ReadCandidates(context.Background(), connector.CandidateRequest{
		Selector: connector.CandidateSelectorStates,
		States:   []string{"Backlog"},
		Limit:    10,
	})
	if err != nil {
		t.Fatalf("ReadCandidates() error = %v", err)
	}
	if len(got.Issues) != 0 || got.Truncated {
		t.Fatalf("result = %#v, want empty complete local result", got)
	}
}

func TestConnectorReadCandidatesSelectsLocalLabelsBeforeHydration(t *testing.T) {
	t.Parallel()

	server := newGitHubLocalTestServer(t)
	c, err := New(Config{
		GitHub: githubconnector.Config{
			Endpoint: server.URL + "/graphql",
			APIKey:   "token",
		},
		Local: local.Config{
			Path: filepath.Join(t.TempDir(), "candidate-label.db"),
			Issues: []connector.Issue{{
				ID:         "github:123:779",
				Identifier: "digitaldrywood/detent#779",
				State:      "Todo",
				Labels:     []string{"enhancement"},
			}},
		},
		Repository: "digitaldrywood/detent",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	got, err := c.ReadCandidates(context.Background(), connector.CandidateRequest{
		Selector: connector.CandidateSelectorLabels,
		Labels:   []string{"enhancement"},
		Limit:    10,
	})
	if err != nil {
		t.Fatalf("ReadCandidates() error = %v", err)
	}
	if len(got.Issues) != 1 || got.Issues[0].ID != "github:123:779" || got.Issues[0].Title != "Closed upstream issue" {
		t.Fatalf("result = %#v, want hydrated local label match", got)
	}
}
