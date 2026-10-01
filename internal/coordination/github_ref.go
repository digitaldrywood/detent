package coordination

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

const githubRefCommitMutation = `
mutation DetentCoordinationCommit($input: CreateCommitOnBranchInput!) {
  createCommitOnBranch(input: $input) { commit { oid committedDate } }
}`

var (
	ErrInvalidGitHubRefConfig = errors.New("github ref coordination config is invalid")
	ErrInvalidGitHubResponse  = errors.New("github ref coordination response is invalid")
)

type GitHubClient interface {
	GraphQL(context.Context, string, map[string]any, any) error
	REST(context.Context, string, string, any, any) error
}

type GitHubRefConfig struct {
	Repository string
	Branch     string
	Client     GitHubClient
	Purpose    string
	Now        func() time.Time
}

type GitHubRefStore struct {
	client    GitHubClient
	owner     string
	name      string
	branch    string
	purpose   string
	now       func() time.Time
	mu        sync.Mutex
	lastKey   string
	lastState githubRefState
}

type githubRefState struct {
	record      Record
	found       bool
	branchFound bool
}

func NewGitHubRefStore(cfg GitHubRefConfig) (*GitHubRefStore, error) {
	owner, name, ok := strings.Cut(strings.TrimSpace(cfg.Repository), "/")
	if !ok || strings.TrimSpace(owner) == "" || strings.TrimSpace(name) == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("%w: repository must use owner/name syntax", ErrInvalidGitHubRefConfig)
	}
	branch := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cfg.Branch), "refs/heads/"))
	if branch == "" {
		return nil, fmt.Errorf("%w: branch is required", ErrInvalidGitHubRefConfig)
	}
	if cfg.Client == nil {
		return nil, fmt.Errorf("%w: github client is required", ErrInvalidGitHubRefConfig)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &GitHubRefStore{
		client:  cfg.Client,
		owner:   strings.TrimSpace(owner),
		name:    strings.TrimSpace(name),
		branch:  branch,
		purpose: strings.TrimSpace(cfg.Purpose),
		now:     now,
	}, nil
}

func (s *GitHubRefStore) Get(ctx context.Context, key string) (Record, bool, error) {
	state, err := s.read(ctx, key)
	if err != nil {
		return Record{}, false, err
	}
	s.mu.Lock()
	s.lastKey, s.lastState = key, state
	s.mu.Unlock()
	return state.record, state.found, nil
}

func (s *GitHubRefStore) Close() error {
	if s == nil {
		return nil
	}
	closer, ok := s.client.(io.Closer)
	if !ok {
		return nil
	}
	return closer.Close()
}

func (s *GitHubRefStore) CompareAndSwap(ctx context.Context, key string, expectedVersion string, value []byte) (Record, bool, error) {
	s.mu.Lock()
	state, cached := s.lastState, s.lastKey == key && s.lastState.record.Version == strings.TrimSpace(expectedVersion)
	s.lastKey = ""
	s.mu.Unlock()
	if !cached {
		var err error
		state, err = s.read(ctx, key)
		if err != nil {
			return Record{}, false, err
		}
	}
	if state.record.Version != strings.TrimSpace(expectedVersion) {
		return Record{}, false, nil
	}
	if !state.branchFound {
		if expectedVersion != "" {
			return Record{}, false, nil
		}
		if err := s.createBranch(ctx); err != nil {
			refreshed, readErr := s.read(ctx, key)
			if readErr == nil && refreshed.branchFound {
				return Record{}, false, nil
			}
			return Record{}, false, err
		}
		return Record{}, false, nil
	}

	cleanKey, err := cleanCoordinationKey(key)
	if err != nil {
		return Record{}, false, err
	}
	variables := map[string]any{
		"input": map[string]any{
			"branch": map[string]any{
				"repositoryNameWithOwner": s.owner + "/" + s.name,
				"branchName":              s.branch,
			},
			"message":         map[string]any{"headline": "chore(detent): update coordination state"},
			"expectedHeadOid": state.record.Version,
			"fileChanges": map[string]any{
				"additions": []map[string]any{{
					"path":     cleanKey,
					"contents": base64.StdEncoding.EncodeToString(value),
				}},
			},
		},
	}
	var response struct {
		CreateCommitOnBranch *struct {
			Commit *struct {
				OID           string    `json:"oid"`
				CommittedDate time.Time `json:"committedDate"`
			} `json:"commit"`
		} `json:"createCommitOnBranch"`
	}
	purpose := s.purpose
	if purpose == "" {
		purpose = "schedule_ownership"
	}
	if coordinationPurpose(ctx) != "" {
		purpose = coordinationPurpose(ctx)
	}
	var commitErr error
	if typed, ok := s.client.(interface {
		GraphQLWithType(context.Context, string, string, map[string]any, any) error
	}); ok {
		commitErr = typed.GraphQLWithType(ctx, purpose, githubRefCommitMutation, variables, &response)
	} else {
		commitErr = s.client.GraphQL(ctx, githubRefCommitMutation, variables, &response)
	}
	if err := commitErr; err != nil {
		refreshed, readErr := s.read(ctx, cleanKey)
		if readErr == nil {
			if refreshed.record.Version != state.record.Version && bytes.Equal(refreshed.record.Value, value) {
				return refreshed.record, true, nil
			}
			if refreshed.record.Version != state.record.Version {
				return Record{}, false, nil
			}
		}
		return Record{}, false, err
	}
	if response.CreateCommitOnBranch == nil || response.CreateCommitOnBranch.Commit == nil || strings.TrimSpace(response.CreateCommitOnBranch.Commit.OID) == "" {
		return Record{}, false, ErrInvalidGitHubResponse
	}
	modifiedAt := response.CreateCommitOnBranch.Commit.CommittedDate.UTC()
	if modifiedAt.IsZero() {
		modifiedAt = s.now().UTC()
	}
	return Record{
		Value:      append([]byte(nil), value...),
		Version:    strings.TrimSpace(response.CreateCommitOnBranch.Commit.OID),
		ModifiedAt: modifiedAt,
	}, true, nil
}

func (s *GitHubRefStore) read(ctx context.Context, key string) (githubRefState, error) {
	cleanKey, err := cleanCoordinationKey(key)
	if err != nil {
		return githubRefState{}, err
	}
	var ref struct {
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	base := "/repos/" + url.PathEscape(s.owner) + "/" + url.PathEscape(s.name)
	if err := s.client.REST(ctx, http.MethodGet, base+"/git/ref/heads/"+escapePath(s.branch), nil, &ref); err != nil {
		if isGitHubNotFound(err) {
			return githubRefState{}, nil
		}
		return githubRefState{}, err
	}
	if strings.TrimSpace(ref.Object.SHA) == "" || ref.Object.Type != "commit" {
		return githubRefState{}, ErrInvalidGitHubResponse
	}
	state := githubRefState{}
	state.branchFound = true
	state.record.Version = strings.TrimSpace(ref.Object.SHA)
	var content struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
		Type     string `json:"type"`
	}
	contentPath := base + "/contents/" + escapePath(cleanKey) + "?ref=" + url.QueryEscape(state.record.Version)
	if err := s.client.REST(ctx, http.MethodGet, contentPath, nil, &content); err != nil {
		if isGitHubNotFound(err) {
			return state, nil
		}
		return githubRefState{}, err
	}
	if content.Type != "file" || content.Encoding != "base64" {
		return githubRefState{}, ErrInvalidGitHubResponse
	}
	decoded, err := base64.StdEncoding.DecodeString(content.Content)
	if err != nil {
		return githubRefState{}, fmt.Errorf("%w: decode coordination contents: %w", ErrInvalidGitHubResponse, err)
	}
	state.found = true
	state.record.Value = decoded
	var commits []struct {
		Commit struct {
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	commitPath := base + "/commits?sha=" + url.QueryEscape(state.record.Version) + "&path=" + url.QueryEscape(cleanKey) + "&per_page=1"
	if err := s.client.REST(ctx, http.MethodGet, commitPath, nil, &commits); err != nil {
		return githubRefState{}, err
	}
	if len(commits) == 0 || commits[0].Commit.Committer.Date.IsZero() {
		return githubRefState{}, ErrInvalidGitHubResponse
	}
	state.record.ModifiedAt = commits[0].Commit.Committer.Date.UTC()
	return state, nil
}

func (s *GitHubRefStore) createBranch(ctx context.Context) error {
	base := "/repos/" + url.PathEscape(s.owner) + "/" + url.PathEscape(s.name)
	var repository struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := s.client.REST(ctx, http.MethodGet, base, nil, &repository); err != nil {
		return err
	}
	if repository.DefaultBranch == "" {
		return ErrInvalidGitHubResponse
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := s.client.REST(ctx, http.MethodGet, base+"/git/ref/heads/"+escapePath(repository.DefaultBranch), nil, &ref); err != nil {
		return err
	}
	if ref.Object.SHA == "" {
		return ErrInvalidGitHubResponse
	}
	return s.client.REST(ctx, http.MethodPost, base+"/git/refs", map[string]string{"ref": "refs/heads/" + s.branch, "sha": ref.Object.SHA}, nil)
}

func escapePath(value string) string {
	parts := strings.Split(value, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func isGitHubNotFound(err error) bool {
	var status interface{ HTTPStatus() int }
	return errors.As(err, &status) && status.HTTPStatus() == http.StatusNotFound
}

func cleanCoordinationKey(key string) (string, error) {
	key = strings.TrimSpace(strings.ReplaceAll(key, "\\", "/"))
	cleaned := path.Clean(key)
	if key == "" || cleaned == "." || cleaned == "/" || strings.HasPrefix(cleaned, "../") || cleaned == ".." || strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("%w: key must be a relative path", ErrInvalidGitHubRefConfig)
	}
	return cleaned, nil
}
