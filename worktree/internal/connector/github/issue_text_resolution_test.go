package github

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestBlockedByResolutionContinuesAfterRetryableFailure(t *testing.T) {
	t.Parallel()
	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{method: http.MethodGet, path: "/repos/owner/repo/issues/1", status: http.StatusInternalServerError, body: `{"message":"unavailable"}`},
		{method: http.MethodGet, path: "/repos/owner/repo/issues/2", status: http.StatusInternalServerError, body: `{"message":"unavailable"}`},
		{method: http.MethodGet, path: "/repos/owner/repo/issues/3", body: `{"node_id":"I3","number":3,"state":"closed","labels":[]}`},
	})
	var logs bytes.Buffer
	c := newGitHubTestConnector(t, server, Config{GitHubStatusSource: GitHubStatusSourceLabel, Repository: "owner/repo", Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	issues := []connector.Issue{{BlockedBy: []connector.BlockedRef{{Identifier: "owner/repo#1"}, {Identifier: "owner/repo#2"}, {Identifier: "owner/repo#3"}}}}
	if err := c.resolveBlockedByProjectState(t.Context(), issues); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"", "", "Done"} {
		if got := issues[0].BlockedBy[i].State; got != want {
			t.Errorf("blocker %d = %q want %q", i, got, want)
		}
	}
	if strings.Count(logs.String(), "github blocked-by states unresolved") != 1 || !strings.Contains(logs.String(), "owner/repo#1 owner/repo#2") {
		t.Fatalf("aggregate warning missing: %s", logs.String())
	}
}

func TestBlockedByResolutionPreservesCancellation(t *testing.T) {
	t.Parallel()
	for _, withRef := range []bool{false, true} {
		t.Run(map[bool]string{false: "no lookup", true: "lookup"}[withRef], func(t *testing.T) {
			t.Parallel()
			server := newGraphQLTestServer(t, nil)
			c := newGitHubTestConnector(t, server, Config{GitHubStatusSource: GitHubStatusSourceLabel, Repository: "owner/repo"})
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			issues := []connector.Issue{{}}
			if withRef {
				issues[0].BlockedBy = []connector.BlockedRef{{Identifier: "owner/repo#1"}}
			}
			if err := c.resolveBlockedByProjectState(ctx, issues); !errors.Is(err, context.Canceled) {
				t.Fatalf("error=%v want canceled", err)
			}
		})
	}
}
