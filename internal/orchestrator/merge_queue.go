package orchestrator

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	nativeMergeQueueEntryRefresh     = 2 * time.Minute
	nativeMergeQueueRepositoryExpiry = 5 * time.Minute
)

type nativeMergeQueueEntry struct {
	Entry     connector.PullRequestMergeQueueEntry
	CheckedAt time.Time
}

type nativeMergeQueueRepository struct {
	Available bool
	CheckedAt time.Time
}

func (o *Orchestrator) delegateNativeMergeQueueIssues(
	ctx context.Context,
	state *State,
	issues []connector.Issue,
	now time.Time,
) []connector.Issue {
	out := cloneIssues(issues)
	if state == nil {
		return out
	}
	state.nativeMergeQueueDeferred = map[string]struct{}{}
	queue, ok := o.connector.(connector.PullRequestMergeQueue)
	if !ok || !o.cfg.MergeFastPathEnabled {
		return out
	}
	pruneNativeMergeQueueEntries(state, out)

	for _, candidate := range staleMergingQueueIssues(out, o.cfg) {
		issueID := strings.TrimSpace(candidate.ID)
		if !nativeMergeQueueCandidate(candidate) || staleMergingPullRequestDispatchActive(state, issueID) {
			continue
		}
		if cached, ok := state.nativeMergeQueueEntries[issueID]; ok && now.Sub(cached.CheckedAt) < nativeMergeQueueEntryRefresh {
			applyNativeMergeQueueEntry(out, issueID, cached.Entry)
			continue
		}

		repositoryKey := nativeMergeQueueRepositoryKey(candidate)
		repository, repositoryKnown := state.nativeMergeQueueRepos[repositoryKey]
		if repositoryKnown && now.Sub(repository.CheckedAt) >= nativeMergeQueueRepositoryExpiry {
			repositoryKnown = false
		}
		if repositoryKnown && !repository.Available {
			continue
		}

		cached, previouslyQueued := state.nativeMergeQueueEntries[issueID]
		status, err := queue.InspectPullRequestMergeQueue(ctx, candidate)
		if err != nil {
			state.nativeMergeQueueDeferred[issueID] = struct{}{}
			o.logNativeMergeQueueFailure(candidate, "inspection_failed", err)
			continue
		}
		state.nativeMergeQueueRepos[repositoryKey] = nativeMergeQueueRepository{
			Available: status.Available,
			CheckedAt: now,
		}
		if status.Entry != nil {
			cacheNativeMergeQueueEntry(state, issueID, *status.Entry, now)
			applyNativeMergeQueueEntry(out, issueID, *status.Entry)
			o.logNativeMergeQueueDelegated(candidate, *status.Entry, "observed")
			continue
		}
		if previouslyQueued {
			cached.CheckedAt = now
			cached.Entry.State = "MISSING"
			state.nativeMergeQueueEntries[issueID] = cached
			applyNativeMergeQueueEntry(out, issueID, cached.Entry)
			o.logNativeMergeQueueFailure(candidate, "entry_missing", nil)
			continue
		}
		if !status.Available {
			continue
		}

		enqueueIssue := cloneIssue(candidate)
		if enqueueIssue.PullRequest != nil && strings.TrimSpace(enqueueIssue.PullRequest.NodeID) == "" {
			enqueueIssue.PullRequest.NodeID = strings.TrimSpace(status.PullRequestNodeID)
		}
		entry, err := queue.EnqueuePullRequest(ctx, enqueueIssue)
		if err != nil {
			state.nativeMergeQueueDeferred[issueID] = struct{}{}
			o.logNativeMergeQueueFailure(candidate, "enqueue_failed", err)
			continue
		}
		cacheNativeMergeQueueEntry(state, issueID, entry, now)
		applyNativeMergeQueueEntry(out, issueID, entry)
		o.logNativeMergeQueueDelegated(candidate, entry, "enqueued")
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      now,
			Event:   "merge_worker_native_queue_enqueued",
			Message: "enqueued " + issueLabel(candidate) + " in the native merge queue",
		})
	}
	return out
}

func nativeMergeQueueCandidate(issue connector.Issue) bool {
	if strings.TrimSpace(issue.ID) == "" || issue.PullRequest == nil {
		return false
	}
	pullRequest := issue.PullRequest
	return !pullRequestHydrationBlocksProgress(pullRequest) &&
		normalizePullRequestState(pullRequest.State) == "open" &&
		!pullRequest.Draft &&
		mergeWorkerCIGreen(pullRequest.CIStatus) &&
		pullRequestRepository(issue) != "" &&
		pullRequestNumber(issue) > 0
}

func nativeMergeQueueRepositoryKey(issue connector.Issue) string {
	baseRef := ""
	if issue.PullRequest != nil {
		baseRef = strings.ToLower(strings.TrimSpace(issue.PullRequest.BaseRef))
	}
	return mergeWorkerRepositoryKey(issue) + "@" + baseRef
}

func pruneNativeMergeQueueEntries(state *State, issues []connector.Issue) {
	active := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		if mergeWorkerIssue(issue) {
			active[strings.TrimSpace(issue.ID)] = struct{}{}
		}
	}
	for issueID := range state.nativeMergeQueueEntries {
		if _, ok := active[issueID]; !ok {
			delete(state.nativeMergeQueueEntries, issueID)
		}
	}
}

func cacheNativeMergeQueueEntry(state *State, issueID string, entry connector.PullRequestMergeQueueEntry, now time.Time) {
	state.nativeMergeQueueEntries[issueID] = nativeMergeQueueEntry{
		Entry:     clonePullRequestMergeQueueEntry(entry),
		CheckedAt: now,
	}
}

func applyNativeMergeQueueEntry(issues []connector.Issue, issueID string, entry connector.PullRequestMergeQueueEntry) {
	for index := range issues {
		if strings.TrimSpace(issues[index].ID) != issueID || issues[index].PullRequest == nil {
			continue
		}
		cloned := cloneIssue(issues[index])
		queueEntry := clonePullRequestMergeQueueEntry(entry)
		cloned.PullRequest.MergeQueueEntry = &queueEntry
		issues[index] = cloned
	}
}

func overlayNativeMergeQueueIssues(issues []connector.Issue, updated []connector.Issue) []connector.Issue {
	byID := make(map[string]connector.Issue, len(updated))
	for _, issue := range updated {
		if issueID := strings.TrimSpace(issue.ID); issueID != "" {
			byID[issueID] = issue
		}
	}
	out := cloneIssues(issues)
	for index, issue := range out {
		if updatedIssue, ok := byID[strings.TrimSpace(issue.ID)]; ok {
			out[index] = cloneIssue(updatedIssue)
		}
	}
	return out
}

func cloneNativeMergeQueueEntries(entries map[string]nativeMergeQueueEntry) map[string]nativeMergeQueueEntry {
	out := make(map[string]nativeMergeQueueEntry, len(entries))
	for issueID, entry := range entries {
		entry.Entry = clonePullRequestMergeQueueEntry(entry.Entry)
		out[issueID] = entry
	}
	return out
}

func clonePullRequestMergeQueueEntry(entry connector.PullRequestMergeQueueEntry) connector.PullRequestMergeQueueEntry {
	cloned := entry
	if entry.EnqueuedAt != nil {
		enqueuedAt := *entry.EnqueuedAt
		cloned.EnqueuedAt = &enqueuedAt
	}
	return cloned
}

func (o *Orchestrator) logNativeMergeQueueDelegated(issue connector.Issue, entry connector.PullRequestMergeQueueEntry, source string) {
	if o.logger == nil {
		return
	}
	o.logger.Info("merge_worker_native_queue_delegated", mergeWorkerLogAttrs(issue,
		"source", source,
		"queue_state", entry.State,
		"queue_position", entry.Position,
		"queue_depth", entry.Depth,
		"estimated_time_to_merge_seconds", entry.EstimatedTimeToMergeSeconds,
	)...)
}

func (o *Orchestrator) logNativeMergeQueueFailure(issue connector.Issue, reason string, err error) {
	if o.logger == nil {
		return
	}
	attrs := mergeWorkerLogAttrs(issue, "reason", reason)
	if err != nil {
		attrs = append(attrs, slog.Any("error", err))
	}
	o.logger.Warn("merge_worker_native_queue_failed", attrs...)
}
