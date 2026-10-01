package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// FetchWorkflowSources reads default-branch workflow blobs in one GraphQL query.
// It uses the connector's normal authentication and budget accounting.
func (c *Connector) FetchWorkflowSources(ctx context.Context, repository string) (map[string]string, error) {
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("invalid GitHub repository %q", repository)
	}
	var response struct {
		Repository *struct {
			DefaultBranchRef *struct {
				Name string `json:"name"`
			} `json:"defaultBranchRef"`
			Object *struct {
				Entries []struct {
					Name   string `json:"name"`
					Object struct {
						Text        *string `json:"text"`
						IsTruncated bool    `json:"isTruncated"`
					} `json:"object"`
				} `json:"entries"`
			} `json:"object"`
		} `json:"repository"`
	}
	const query = `query DoctorWorkflowSources($owner: String!, $name: String!) {
 repository(owner: $owner, name: $name) {
  defaultBranchRef { name }
  object(expression: "HEAD:.github/workflows") { ... on Tree { entries { name object { ... on Blob { text isTruncated } } } } }
 }
 rateLimit { limit used remaining cost resetAt }
}`
	if err := c.client.GraphQLWithType(ctx, "audit", query, map[string]any{"owner": owner, "name": name}, &response); err != nil {
		return nil, fmt.Errorf("read workflow sources: %w", err)
	}
	if response.Repository == nil || response.Repository.DefaultBranchRef == nil {
		return nil, errors.New("repository default branch unavailable")
	}
	sources := map[string]string{}
	if response.Repository.Object == nil {
		return sources, nil
	}
	for _, entry := range response.Repository.Object.Entries {
		if !strings.HasSuffix(entry.Name, ".yml") && !strings.HasSuffix(entry.Name, ".yaml") {
			continue
		}
		if entry.Object.Text == nil || entry.Object.IsTruncated {
			return nil, fmt.Errorf("workflow %s is unreadable or truncated", entry.Name)
		}
		sources[entry.Name] = *entry.Object.Text
	}
	return sources, nil
}
