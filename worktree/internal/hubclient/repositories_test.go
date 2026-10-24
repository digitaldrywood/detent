package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestRunnerRepositoryList(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name              string
		authErr, errorAPI error
		pages             int
		want              int
		reason            string
	}{
		{name: "user and organization repositories", pages: 1, want: 2},
		{name: "bounded", pages: 20, want: runnerauth.RepositoryListLimit},
		{name: "CLI absent", authErr: os.ErrNotExist, reason: "no GitHub CLI sign-in"},
		{name: "signed out", authErr: errors.New("credential-secret"), reason: "no GitHub CLI sign-in"},
		{name: "API failure", errorAPI: errors.New("credential-secret example-org/private"), pages: 1, reason: "repository listing unavailable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			list := listRunnerRepositories(t.Context(), "request", func(_ context.Context, args ...string) ([]byte, error) {
				if args[0] == "auth" {
					return []byte("credential-secret"), tt.authErr
				}
				calls++
				if !strings.Contains(args[len(args)-1], "affiliation=owner,collaborator,organization_member&sort=pushed&direction=desc") {
					t.Fatalf("missing scope or order: %v", args)
				}
				if tt.errorAPI != nil {
					return nil, tt.errorAPI
				}
				count := 2
				if tt.pages > 1 {
					count = 100
				}
				repos := make([]map[string]any, 0, count)
				for i := range count {
					repos = append(repos, map[string]any{"owner": map[string]string{"login": []string{"example-user", "example-org"}[i%2]}, "name": fmt.Sprintf("repository-%04d", calls*100+i), "private": i%2 == 1, "pushed_at": time.Date(2026, 1, 1, 0, 0, calls*100+i, 0, time.UTC), "token": "credential-secret"})
				}
				return json.Marshal(repos)
			})
			if len(list.Repositories) != tt.want || list.Reason != tt.reason || calls != tt.pages {
				t.Fatalf("report count=%d reason=%q calls=%d", len(list.Repositories), list.Reason, calls)
			}
			for i, repo := range list.Repositories {
				if repo.Owner == "" || repo.Name == "" || repo.PushedAt == nil {
					t.Fatalf("incomplete entry: %+v", repo)
				}
				if i > 0 && repo.PushedAt.After(*list.Repositories[i-1].PushedAt) {
					t.Fatal("not sorted newest first")
				}
			}
			if tt.want == 2 && (list.Repositories[0].Owner != "example-org" || !list.Repositories[0].Private || list.Repositories[1].Private || list.Repositories[0].Name != "repository-0101") {
				t.Fatalf("metadata = %+v", list.Repositories)
			}
			raw, err := json.Marshal(list)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "credential-secret") {
				t.Fatal("credentials in report")
			}
		})
	}
}
