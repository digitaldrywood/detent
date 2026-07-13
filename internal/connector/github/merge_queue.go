package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

const inspectPullRequestMergeQueueQuery = `
query DetentInspectPullRequestMergeQueue($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      id
      mergeQueue { url }
      mergeQueueEntry {
        id
        state
        position
        estimatedTimeToMerge
        enqueuedAt
        mergeQueue {
          url
          entries { totalCount }
        }
      }
    }
  }
  rateLimit { limit used remaining cost resetAt }
}`

const enqueuePullRequestMutation = `
mutation DetentEnqueuePullRequest($pullRequestId: ID!) {
  enqueuePullRequest(input: {pullRequestId: $pullRequestId}) {
    mergeQueueEntry {
      id
      state
      position
      estimatedTimeToMerge
      enqueuedAt
      mergeQueue {
        url
        entries { totalCount }
      }
    }
  }
  rateLimit { limit used remaining cost resetAt }
}`

type mergeQueueEntryNode struct {
	ID                   string     `json:"id"`
	State                string     `json:"state"`
	Position             int        `json:"position"`
	EstimatedTimeToMerge int64      `json:"estimatedTimeToMerge"`
	EnqueuedAt           *time.Time `json:"enqueuedAt"`
	MergeQueue           *struct {
		URL     string `json:"url"`
		Entries struct {
			TotalCount int `json:"totalCount"`
		} `json:"entries"`
	} `json:"mergeQueue"`
}

func (c *Connector) InspectPullRequestMergeQueue(ctx context.Context, issue connector.Issue) (connector.PullRequestMergeQueueStatus, error) {
	repo, number, ok := hydratedPullRequestRef(issue)
	if !ok {
		return connector.PullRequestMergeQueueStatus{}, errors.New("inspect github merge queue: missing pull request repository or number")
	}
	var response struct {
		Repository *struct {
			PullRequest *struct {
				ID         string `json:"id"`
				MergeQueue *struct {
					URL string `json:"url"`
				} `json:"mergeQueue"`
				MergeQueueEntry *mergeQueueEntryNode `json:"mergeQueueEntry"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := c.client.GraphQLWithType(ctx, graphQLQueryMergeQueue, inspectPullRequestMergeQueueQuery, map[string]any{
		"owner":  repo.Owner,
		"name":   repo.Name,
		"number": number,
	}, &response); err != nil {
		return connector.PullRequestMergeQueueStatus{}, fmt.Errorf("inspect github merge queue: %w", err)
	}
	if response.Repository == nil || response.Repository.PullRequest == nil {
		return connector.PullRequestMergeQueueStatus{}, fmt.Errorf("inspect github merge queue: pull request %s#%d not found", pullRequestRepoName(repo), number)
	}
	pullRequest := response.Repository.PullRequest
	status := connector.PullRequestMergeQueueStatus{
		Available:         pullRequest.MergeQueue != nil || pullRequest.MergeQueueEntry != nil,
		PullRequestNodeID: strings.TrimSpace(pullRequest.ID),
	}
	status.Entry = connectorMergeQueueEntry(pullRequest.MergeQueueEntry)
	return status, nil
}

func (c *Connector) EnqueuePullRequest(ctx context.Context, issue connector.Issue) (connector.PullRequestMergeQueueEntry, error) {
	if issue.PullRequest == nil {
		return connector.PullRequestMergeQueueEntry{}, errors.New("enqueue github pull request: missing pull request")
	}
	nodeID := strings.TrimSpace(issue.PullRequest.NodeID)
	if nodeID == "" {
		status, err := c.InspectPullRequestMergeQueue(ctx, issue)
		if err != nil {
			return connector.PullRequestMergeQueueEntry{}, err
		}
		if !status.Available {
			return connector.PullRequestMergeQueueEntry{}, errors.New("enqueue github pull request: repository does not require a merge queue")
		}
		if status.Entry != nil {
			return *status.Entry, nil
		}
		nodeID = status.PullRequestNodeID
	}
	if nodeID == "" {
		return connector.PullRequestMergeQueueEntry{}, errors.New("enqueue github pull request: missing pull request node id")
	}
	var response struct {
		EnqueuePullRequest *struct {
			MergeQueueEntry *mergeQueueEntryNode `json:"mergeQueueEntry"`
		} `json:"enqueuePullRequest"`
	}
	if err := c.client.GraphQLWithType(ctx, graphQLQueryEnqueuePR, enqueuePullRequestMutation, map[string]any{
		"pullRequestId": nodeID,
	}, &response); err != nil {
		return connector.PullRequestMergeQueueEntry{}, fmt.Errorf("enqueue github pull request: %w", err)
	}
	if response.EnqueuePullRequest == nil || response.EnqueuePullRequest.MergeQueueEntry == nil {
		return connector.PullRequestMergeQueueEntry{}, errors.New("enqueue github pull request: github returned no merge queue entry")
	}
	entry := connectorMergeQueueEntry(response.EnqueuePullRequest.MergeQueueEntry)
	return *entry, nil
}

func connectorMergeQueueEntry(entry *mergeQueueEntryNode) *connector.PullRequestMergeQueueEntry {
	if entry == nil {
		return nil
	}
	out := &connector.PullRequestMergeQueueEntry{
		ID:                          strings.TrimSpace(entry.ID),
		State:                       strings.ToUpper(strings.TrimSpace(entry.State)),
		Position:                    entry.Position,
		EstimatedTimeToMergeSeconds: entry.EstimatedTimeToMerge,
		EnqueuedAt:                  cloneGitHubTime(entry.EnqueuedAt),
	}
	if entry.MergeQueue != nil {
		out.Depth = entry.MergeQueue.Entries.TotalCount
		out.URL = strings.TrimSpace(entry.MergeQueue.URL)
	}
	return out
}
