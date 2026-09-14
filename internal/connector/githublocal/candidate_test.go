package githublocal

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

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

type partialCandidateBackend struct {
	*recordingGitHubBackend
	remaining     int
	metadataCalls int
}

func (b *partialCandidateBackend) FetchIssueStatesByIdentifiers(_ context.Context, ids []string) ([]connector.Issue, error) {
	if b.remaining == 0 {
		return nil, &githubconnector.RESTFanoutDeferralError{BudgetScope: "candidates", FanoutCap: 2}
	}
	b.remaining--
	return []connector.Issue{{Identifier: ids[0], Title: "Hydrated"}}, nil
}

func (b *partialCandidateBackend) FetchRepositoryInfo(context.Context, string) (githubconnector.RepositoryInfo, error) {
	b.metadataCalls++
	return githubconnector.RepositoryInfo{ID: 123}, nil
}

func TestCandidateHydrationResumesPartialLocalBatch(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{1, 2} {
		t.Run(fmt.Sprintf("budget=%d", limit), func(t *testing.T) {
			t.Parallel()
			issues := make([]connector.Issue, 103)
			// Variable fractional precision sorts differently as SQLite text and Go time.
			base := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
			for i := range issues {
				createdAt := base.Add(time.Duration(i) * time.Nanosecond)
				issues[i] = connector.Issue{CreatedAt: &createdAt, ID: fmt.Sprintf("issue-%03d", i), Identifier: fmt.Sprintf("digitaldrywood/detent#%03d", i+1), State: "Backlog"}
			}
			backend, err := local.New(local.Config{Path: filepath.Join(t.TempDir(), "issues.db"), ProjectID: "detent", Issues: issues})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := backend.Close(); err != nil {
					t.Error(err)
				}
			})
			request := connector.CandidateRequest{Selector: connector.CandidateSelectorStates, States: []string{"Backlog"}, Limit: 100}
			seen := map[string]bool{}
			for range (len(issues) + limit - 1) / limit {
				upstream := &partialCandidateBackend{remaining: limit}
				c := &Connector{local: backend, github: upstream, repository: "digitaldrywood/detent"}
				result, err := c.ReadCandidates(t.Context(), request)
				if upstream.metadataCalls > 1 {
					t.Fatalf("repository metadata requests = %d", upstream.metadataCalls)
				}
				if err != nil && !errors.Is(err, githubconnector.ErrRESTFanoutDeferred) {
					t.Fatal(err)
				}
				for _, issue := range result.Issues {
					if issue.Title != "Hydrated" {
						t.Fatalf("unhydrated issue: %#v", issue)
					}
					seen[issue.ID] = true
				}
				request.Cursor = result.NextCursor
				if request.Cursor == "" && len(seen) == len(issues) {
					break
				}
			}
			if len(seen) != 103 || request.Cursor != "" {
				t.Fatalf("covered=%d cursor=%q", len(seen), request.Cursor)
			}
		})
	}
}
