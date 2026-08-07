package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

type RepositoryInfo struct {
	ID            int64
	NameWithOwner string
	HTMLURL       string
	Private       bool
	Visibility    string
	DefaultBranch string
}

func (c *Connector) FetchRepositoryInfo(ctx context.Context, repository string) (RepositoryInfo, error) {
	repository = strings.TrimSpace(repository)
	if repository == "" {
		return RepositoryInfo{}, ErrMissingRepository
	}
	var response struct {
		ID            int64  `json:"id"`
		FullName      string `json:"full_name"`
		HTMLURL       string `json:"html_url"`
		Private       bool   `json:"private"`
		Visibility    string `json:"visibility"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.client.REST(ctx, http.MethodGet, restRepositoryPath(repository), nil, &response); err != nil {
		return RepositoryInfo{}, fmt.Errorf("fetch github repository info: %w", err)
	}
	if response.ID == 0 {
		return RepositoryInfo{}, ErrInvalidResponse
	}
	info := RepositoryInfo{
		ID:            response.ID,
		NameWithOwner: strings.TrimSpace(response.FullName),
		HTMLURL:       strings.TrimSpace(response.HTMLURL),
		Private:       response.Private,
		Visibility:    strings.ToLower(strings.TrimSpace(response.Visibility)),
		DefaultBranch: strings.TrimSpace(response.DefaultBranch),
	}
	if info.Visibility == "" {
		if info.Private {
			info.Visibility = "private"
		} else {
			info.Visibility = "public"
		}
	}
	if info.NameWithOwner == "" {
		info.NameWithOwner = repository
	}
	return info, nil
}
