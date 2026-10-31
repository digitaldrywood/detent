package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type repositoryCommand func(context.Context, ...string) ([]byte, error)

func githubRepositoryCommand(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh")
	cmd.Args = append(cmd.Args, args...)
	var output bytes.Buffer
	cmd.Stdout = &repositoryOutput{buffer: &output, remaining: 2 << 20}
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, errors.New("GitHub CLI request unavailable")
	}
	return output.Bytes(), nil
}

type repositoryOutput struct {
	buffer    *bytes.Buffer
	remaining int
}

func (w *repositoryOutput) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		return 0, errors.New("repository listing output exceeded limit")
	}
	w.remaining -= len(p)
	return w.buffer.Write(p)
}

func listRunnerRepositories(ctx context.Context, requestID string, run repositoryCommand) runnerauth.RepositoryList {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	list := runnerauth.RepositoryList{RequestID: requestID, Repositories: []runnerauth.Repository{}}
	if _, err := run(ctx, "auth", "status", "--hostname", "github.com"); err != nil {
		list.Reason = "no GitHub CLI sign-in"
		return list
	}
	seen := make(map[string]bool)
	for page := 1; page <= runnerauth.RepositoryListLimit/100; page++ {
		raw, err := run(ctx, "api", "--hostname", "github.com", "--method", "GET", fmt.Sprintf("user/repos?affiliation=owner,collaborator,organization_member&sort=pushed&direction=desc&per_page=100&page=%d", page))
		var repos []struct {
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
			Name     string     `json:"name"`
			Private  bool       `json:"private"`
			PushedAt *time.Time `json:"pushed_at"`
		}
		if err != nil || json.Unmarshal(raw, &repos) != nil {
			list.Repositories = []runnerauth.Repository{}
			list.Reason = "repository listing unavailable"
			return list
		}
		for _, repo := range repos {
			key := repo.Owner.Login + "/" + repo.Name
			if !seen[key] {
				list.Repositories = append(list.Repositories, runnerauth.Repository{Owner: repo.Owner.Login, Name: repo.Name, Private: repo.Private, PushedAt: repo.PushedAt})
				seen[key] = true
			}
		}
		if len(repos) < 100 {
			break
		}
	}
	slices.SortStableFunc(list.Repositories, func(a, b runnerauth.Repository) int {
		if a.PushedAt == nil {
			if b.PushedAt == nil {
				return 0
			}
			return 1
		}
		if b.PushedAt == nil {
			return -1
		}
		return b.PushedAt.Compare(*a.PushedAt)
	})
	if len(list.Repositories) > runnerauth.RepositoryListLimit {
		list.Repositories = list.Repositories[:runnerauth.RepositoryListLimit]
	}
	if list.Validate() != nil {
		list.Repositories = []runnerauth.Repository{}
		list.Reason = "repository listing unavailable"
	}
	return list
}

func (c *NativeClient) reportRepositoryList(ctx context.Context, requestID string) error {
	run := c.repositoryCommand
	if run == nil {
		run = githubRepositoryCommand
	}
	request := struct {
		tracker.Mutation
		runnerauth.RepositoryList
	}{Mutation: tracker.Mutation{IdempotencyKey: requestID}, RepositoryList: listRunnerRepositories(ctx, requestID, run)}
	return c.client.request(ctx, http.MethodPost, c.base()+"/onboarding/repositories/report", request, nil)
}
