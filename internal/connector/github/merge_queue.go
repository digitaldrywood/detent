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
      headRefOid
      baseRefName
      timelineItems(last: 1, itemTypes: [REMOVED_FROM_MERGE_QUEUE_EVENT]) {
        nodes { ... on RemovedFromMergeQueueEvent { beforeCommit { oid } reason createdAt } }
      }
      mergeQueue { url entries { totalCount } configuration { maximumEntriesToBuild maximumEntriesToMerge minimumEntriesToMerge minimumEntriesToMergeWaitTime } }
      mergeQueueEntry {
        headCommit { oid }
        baseCommit { oid }
        id
        state
        position
        estimatedTimeToMerge
        enqueuedAt
        mergeQueue {
          url
          entries { totalCount }
          configuration { maximumEntriesToMerge minimumEntriesToMerge minimumEntriesToMergeWaitTime }
        }
      }
    }
  }
  rateLimit { limit used remaining cost resetAt }
}`

const enqueuePullRequestMutation = `
mutation DetentEnqueuePullRequest($pullRequestId: ID!, $expectedHeadOid: GitObjectID!) {
  enqueuePullRequest(input: {pullRequestId: $pullRequestId, expectedHeadOid: $expectedHeadOid}) {
    mergeQueueEntry {
      headCommit { oid }
      baseCommit { oid }
      id
      state
      position
      estimatedTimeToMerge
      enqueuedAt
      mergeQueue {
        url
        entries { totalCount }
        configuration { maximumEntriesToMerge minimumEntriesToMerge minimumEntriesToMergeWaitTime }
      }
    }
  }
}`

const dequeuePullRequestMutation = `
mutation DetentDequeuePullRequest($mergeQueueEntryId: ID!) {
  dequeuePullRequest(input: {id: $mergeQueueEntryId}) {
    mergeQueueEntry { id }
  }
}`

type mergeQueueEntryNode struct {
	HeadCommit struct {
		OID string `json:"oid"`
	} `json:"headCommit"`
	BaseCommit struct {
		OID string `json:"oid"`
	} `json:"baseCommit"`
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
		Configuration mergeQueueConfigurationNode `json:"configuration"`
	} `json:"mergeQueue"`
}

type mergeQueueConfigurationNode struct {
	MaximumEntriesToBuild         int `json:"maximumEntriesToBuild"`
	MaximumEntriesToMerge         int `json:"maximumEntriesToMerge"`
	MinimumEntriesToMerge         int `json:"minimumEntriesToMerge"`
	MinimumEntriesToMergeWaitTime int `json:"minimumEntriesToMergeWaitTime"`
}

func (n mergeQueueConfigurationNode) batching() connector.MergeQueueBatching {
	return connector.MergeQueueBatching{
		MaxGroupSize:    n.MaximumEntriesToMerge,
		MinGroupSize:    n.MinimumEntriesToMerge,
		MinGroupWaitSec: int64(n.MinimumEntriesToMergeWaitTime) * 60,
	}
}

func (c *Connector) InspectPullRequestMergeQueue(ctx context.Context, issue connector.Issue) (connector.PullRequestMergeQueueStatus, error) {
	repo, number, ok := hydratedPullRequestRef(issue)
	if !ok {
		return connector.PullRequestMergeQueueStatus{}, errors.New("inspect github merge queue: missing pull request repository or number")
	}
	var response struct {
		Repository *struct {
			PullRequest *struct {
				ID            string `json:"id"`
				HeadRefOid    string `json:"headRefOid"`
				BaseRefName   string `json:"baseRefName"`
				TimelineItems struct {
					Nodes []struct {
						CreatedAt    *time.Time `json:"createdAt"`
						Reason       string     `json:"reason"`
						BeforeCommit struct {
							OID string `json:"oid"`
						} `json:"beforeCommit"`
					} `json:"nodes"`
				} `json:"timelineItems"`
				MergeQueue *struct {
					URL     string `json:"url"`
					Entries struct {
						TotalCount int `json:"totalCount"`
					} `json:"entries"`
					Configuration mergeQueueConfigurationNode `json:"configuration"`
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
		HeadSHA:           strings.TrimSpace(pullRequest.HeadRefOid),
		Available:         pullRequest.MergeQueue != nil || pullRequest.MergeQueueEntry != nil,
		PullRequestNodeID: strings.TrimSpace(pullRequest.ID),
	}
	if !status.Available && strings.TrimSpace(pullRequest.BaseRefName) != "" {
		policy, err := c.branchMergePolicy(ctx, pullRequestRepoName(repo), pullRequest.BaseRefName)
		if err != nil {
			return connector.PullRequestMergeQueueStatus{}, fmt.Errorf("inspect github branch merge policy: %w", err)
		}
		status.Available = policy.MergeQueue
		status.AdmissionLimit = policy.AdmissionLimit
	}
	if pullRequest.MergeQueue != nil {
		status.Depth = pullRequest.MergeQueue.Entries.TotalCount
		status.AdmissionLimit = pullRequest.MergeQueue.Configuration.MaximumEntriesToBuild
		status.Batching = pullRequest.MergeQueue.Configuration.batching()
	}
	if len(pullRequest.TimelineItems.Nodes) > 0 {
		status.RemovalObserved = true
		status.RemovedAt = cloneGitHubTime(pullRequest.TimelineItems.Nodes[0].CreatedAt)
		status.RemovalReason = strings.TrimSpace(pullRequest.TimelineItems.Nodes[0].Reason)
		status.RemovedHeadSHA = strings.TrimSpace(pullRequest.TimelineItems.Nodes[0].BeforeCommit.OID)
	}
	status.Entry = connectorMergeQueueEntry(pullRequest.MergeQueueEntry)
	if status.Entry != nil && status.Entry.Batching == (connector.MergeQueueBatching{}) {
		status.Entry.Batching = status.Batching
	}
	return status, nil
}

func (c *Connector) EnqueuePullRequest(ctx context.Context, issue connector.Issue) (connector.PullRequestMergeQueueEntry, error) {
	if issue.PullRequest == nil {
		return connector.PullRequestMergeQueueEntry{}, errors.New("enqueue github pull request: missing pull request")
	}
	headSHA := strings.TrimSpace(issue.PullRequest.HeadSHA)
	if headSHA == "" {
		return connector.PullRequestMergeQueueEntry{}, errors.New("enqueue github pull request: missing expected head sha")
	}
	nodeID := strings.TrimSpace(issue.PullRequest.NodeID)
	if nodeID == "" {
		status, err := c.InspectPullRequestMergeQueue(ctx, issue)
		if err != nil {
			return connector.PullRequestMergeQueueEntry{}, err
		}
		if status.HeadSHA != headSHA {
			return connector.PullRequestMergeQueueEntry{}, errors.New("enqueue github pull request: pull request head changed")
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
		"pullRequestId":   nodeID,
		"expectedHeadOid": headSHA,
	}, &response); err != nil {
		return connector.PullRequestMergeQueueEntry{}, fmt.Errorf("enqueue github pull request: %w", err)
	}
	if response.EnqueuePullRequest == nil || response.EnqueuePullRequest.MergeQueueEntry == nil {
		return connector.PullRequestMergeQueueEntry{}, errors.New("enqueue github pull request: github returned no merge queue entry")
	}
	entry := connectorMergeQueueEntry(response.EnqueuePullRequest.MergeQueueEntry)
	return *entry, nil
}

func (c *Connector) DequeuePullRequest(ctx context.Context, entry connector.PullRequestMergeQueueEntry) error {
	entryID := strings.TrimSpace(entry.ID)
	if entryID == "" {
		return errors.New("dequeue github pull request: missing merge queue entry id")
	}
	var response struct {
		DequeuePullRequest *struct {
			MergeQueueEntry *struct {
				ID string `json:"id"`
			} `json:"mergeQueueEntry"`
		} `json:"dequeuePullRequest"`
	}
	if err := c.client.GraphQLWithType(ctx, graphQLQueryDequeuePR, dequeuePullRequestMutation, map[string]any{
		"mergeQueueEntryId": entryID,
	}, &response); err != nil {
		return fmt.Errorf("dequeue github pull request: %w", err)
	}
	if response.DequeuePullRequest == nil || response.DequeuePullRequest.MergeQueueEntry == nil {
		return errors.New("dequeue github pull request: github returned no merge queue entry")
	}
	dequeuedID := strings.TrimSpace(response.DequeuePullRequest.MergeQueueEntry.ID)
	if dequeuedID != entryID {
		return fmt.Errorf("dequeue github pull request: github returned merge queue entry %q, want %q", dequeuedID, entryID)
	}
	return nil
}

func connectorMergeQueueEntry(entry *mergeQueueEntryNode) *connector.PullRequestMergeQueueEntry {
	if entry == nil {
		return nil
	}
	out := &connector.PullRequestMergeQueueEntry{
		HeadSHA:                     strings.TrimSpace(entry.HeadCommit.OID),
		BaseSHA:                     strings.TrimSpace(entry.BaseCommit.OID),
		ID:                          strings.TrimSpace(entry.ID),
		State:                       strings.ToUpper(strings.TrimSpace(entry.State)),
		Position:                    entry.Position,
		EstimatedTimeToMergeSeconds: entry.EstimatedTimeToMerge,
		EnqueuedAt:                  cloneGitHubTime(entry.EnqueuedAt),
	}
	if entry.MergeQueue != nil {
		out.Depth = entry.MergeQueue.Entries.TotalCount
		out.URL = strings.TrimSpace(entry.MergeQueue.URL)
		out.Batching = entry.MergeQueue.Configuration.batching()
	}
	return out
}
