package coordination

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
)

const githubRefStateQuery = `
query DetentCoordinationState($owner: String!, $name: String!, $qualifiedName: String!, $expression: String!, $path: String!) {
  repository(owner: $owner, name: $name) {
    id
    defaultBranchRef { target { oid } }
    ref(qualifiedName: $qualifiedName) {
      target {
        oid
        ... on Commit {
          history(first: 1, path: $path) { nodes { committedDate } }
        }
      }
    }
    object(expression: $expression) {
      ... on Blob { oid text }
    }
  }
}`

const githubRefCreateMutation = `
mutation DetentCoordinationCreateRef($input: CreateRefInput!) {
  createRef(input: $input) { ref { id target { oid } } }
}`

const githubRefCommitMutation = `
mutation DetentCoordinationCommit($input: CreateCommitOnBranchInput!) {
  createCommitOnBranch(input: $input) { commit { oid committedDate } }
}`

var (
	ErrInvalidGitHubRefConfig = errors.New("github ref coordination config is invalid")
	ErrInvalidGitHubResponse  = errors.New("github ref coordination response is invalid")
)

type GraphQLClient interface {
	GraphQL(context.Context, string, map[string]any, any) error
}

type GitHubRefConfig struct {
	Repository string
	Branch     string
	Client     GraphQLClient
	Now        func() time.Time
}

type GitHubRefStore struct {
	client GraphQLClient
	owner  string
	name   string
	branch string
	now    func() time.Time
}

type githubRefState struct {
	record           Record
	found            bool
	repositoryID     string
	defaultBranchOID string
	branchFound      bool
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
		return nil, fmt.Errorf("%w: graphql client is required", ErrInvalidGitHubRefConfig)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &GitHubRefStore{
		client: cfg.Client,
		owner:  strings.TrimSpace(owner),
		name:   strings.TrimSpace(name),
		branch: branch,
		now:    now,
	}, nil
}

func (s *GitHubRefStore) Get(ctx context.Context, key string) (Record, bool, error) {
	state, err := s.read(ctx, key)
	if err != nil {
		return Record{}, false, err
	}
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
	state, err := s.read(ctx, key)
	if err != nil {
		return Record{}, false, err
	}
	if state.record.Version != strings.TrimSpace(expectedVersion) {
		return Record{}, false, nil
	}
	if !state.branchFound {
		if expectedVersion != "" {
			return Record{}, false, nil
		}
		if err := s.createBranch(ctx, state.repositoryID, state.defaultBranchOID); err != nil {
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
	if err := s.client.GraphQL(ctx, githubRefCommitMutation, variables, &response); err != nil {
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
	variables := map[string]any{
		"owner":         s.owner,
		"name":          s.name,
		"qualifiedName": "refs/heads/" + s.branch,
		"expression":    s.branch + ":" + cleanKey,
		"path":          cleanKey,
	}
	var response struct {
		Repository *struct {
			ID               string `json:"id"`
			DefaultBranchRef *struct {
				Target struct {
					OID string `json:"oid"`
				} `json:"target"`
			} `json:"defaultBranchRef"`
			Ref *struct {
				Target struct {
					OID     string `json:"oid"`
					History struct {
						Nodes []struct {
							CommittedDate time.Time `json:"committedDate"`
						} `json:"nodes"`
					} `json:"history"`
				} `json:"target"`
			} `json:"ref"`
			Object *struct {
				OID  string `json:"oid"`
				Text string `json:"text"`
			} `json:"object"`
		} `json:"repository"`
	}
	if err := s.client.GraphQL(ctx, githubRefStateQuery, variables, &response); err != nil {
		return githubRefState{}, err
	}
	if response.Repository == nil || strings.TrimSpace(response.Repository.ID) == "" {
		return githubRefState{}, ErrInvalidGitHubResponse
	}
	state := githubRefState{repositoryID: strings.TrimSpace(response.Repository.ID)}
	if response.Repository.DefaultBranchRef != nil {
		state.defaultBranchOID = strings.TrimSpace(response.Repository.DefaultBranchRef.Target.OID)
	}
	if response.Repository.Ref == nil {
		return state, nil
	}
	state.branchFound = true
	state.record.Version = strings.TrimSpace(response.Repository.Ref.Target.OID)
	if response.Repository.Object == nil {
		return state, nil
	}
	state.found = true
	state.record.Value = []byte(response.Repository.Object.Text)
	if nodes := response.Repository.Ref.Target.History.Nodes; len(nodes) > 0 {
		state.record.ModifiedAt = nodes[0].CommittedDate.UTC()
	}
	return state, nil
}

func (s *GitHubRefStore) createBranch(ctx context.Context, repositoryID string, oid string) error {
	if strings.TrimSpace(repositoryID) == "" || strings.TrimSpace(oid) == "" {
		return fmt.Errorf("%w: default branch is unavailable", ErrInvalidGitHubResponse)
	}
	variables := map[string]any{
		"input": map[string]any{
			"repositoryId": repositoryID,
			"name":         "refs/heads/" + s.branch,
			"oid":          oid,
		},
	}
	var response struct {
		CreateRef *struct {
			Ref *struct {
				ID string `json:"id"`
			} `json:"ref"`
		} `json:"createRef"`
	}
	if err := s.client.GraphQL(ctx, githubRefCreateMutation, variables, &response); err != nil {
		return err
	}
	if response.CreateRef == nil || response.CreateRef.Ref == nil || strings.TrimSpace(response.CreateRef.Ref.ID) == "" {
		return ErrInvalidGitHubResponse
	}
	return nil
}

func cleanCoordinationKey(key string) (string, error) {
	key = strings.TrimSpace(strings.ReplaceAll(key, "\\", "/"))
	cleaned := path.Clean(key)
	if key == "" || cleaned == "." || cleaned == "/" || strings.HasPrefix(cleaned, "../") || cleaned == ".." || strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("%w: key must be a relative path", ErrInvalidGitHubRefConfig)
	}
	return cleaned, nil
}
