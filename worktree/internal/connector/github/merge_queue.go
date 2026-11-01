package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/runtimeoutput"
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
	if status.Entry == nil && status.RemovalObserved && issue.PullRequest != nil && strings.TrimSpace(issue.PullRequest.HeadSHA) == status.HeadSHA && status.RemovedHeadSHA != "" {
		removedCommit := status.RemovedHeadSHA
		if removedCommit != status.HeadSHA {
			var comparison struct {
				Status string `json:"status"`
			}
			path := restRepositoryPath(pullRequestRepoName(repo)) + "/compare/" + url.PathEscape(status.HeadSHA) + "..." + url.PathEscape(removedCommit)
			if err := c.client.REST(ctx, http.MethodGet, path, nil, &comparison); err != nil {
				return status, fmt.Errorf("identify removed merge group: %w", err)
			}
			if comparison.Status == "ahead" || comparison.Status == "identical" {
				status.RemovedHeadSHA = status.HeadSHA
			}
		}
		if status.RemovedHeadSHA == status.HeadSHA {
			runs, err := fetchRESTCheckRuns(ctx, c.client, restCommitCheckRunsPath(repo, removedCommit))
			if err != nil {
				return status, fmt.Errorf("read removed merge-group checks: %w", err)
			}
			if _, err := c.transientCheckRunFailures(ctx, repo, runs); err != nil {
				return status, err
			}
			for _, run := range effectiveCheckRuns(runs) {
				if !completedFailedCheckRun(run) {
					continue
				}
				status.RemovalReason += fmt.Sprintf("\n- failed job: %s; run: %d; %s", run.Name, checkRunWorkflowRunID(run), firstNonBlank(run.DetailsURL, run.HTMLURL))
				if !strings.Contains(run.FailureDetail, "--- FAIL:") {
					detail, err := c.mergeQueueFailedTestLog(ctx, repo, run)
					if err != nil {
						if pullRequestHydrationThrottleError(err) {
							return status, err
						}
						// Missing or expired logs must not erase the queue outcome.
						if c.logger != nil {
							c.logger.DebugContext(ctx, "read merge-group job log failed", "check_run_id", run.ID, "error", err)
						}
					} else if detail != "" {
						run.FailureDetail = strings.TrimSpace(run.FailureDetail + "\n" + detail)
					}
				}
				if run.FailureDetail != "" {
					status.RemovalReason += "\n  " + run.FailureDetail
				}
			}
		}
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
		// GitHub may already have consumed the entry by merging or removing it.
		return nil
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

// Use Actions' job identity from its details URL, not the check-run ID.
func (c *Connector) mergeQueueFailedTestLog(ctx context.Context, repo pullRequestRepo, run restCheckRun) (string, error) {
	parsed, err := url.Parse(firstNonBlank(run.DetailsURL, run.HTMLURL))
	if err != nil {
		return "", nil
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 || parts[len(parts)-2] != "job" {
		return "", nil
	}
	job, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
	if err != nil || job <= 0 {
		return "", nil
	}
	path := restRepositoryPath(pullRequestRepoName(repo)) + "/actions/jobs/" + strconv.FormatInt(job, 10) + "/logs"
	logs, _, err := c.client.RESTText(ctx, path, "application/vnd.github+json", 4<<20)
	if err != nil {
		return "", err
	}
	var failures []string
	for line := range strings.SplitSeq(logs, "\n") {
		if index := strings.Index(line, "--- FAIL:"); index >= 0 {
			failures = append(failures, strings.TrimSpace(line[index:]))
			if len(failures) == 10 {
				break
			}
		}
	}
	return runtimeoutput.Truncate(strings.Join(failures, "\n"), 2048).Value, nil
}
