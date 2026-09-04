package orchestrator

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	nativeMergeQueueEntryRefresh     = 2 * time.Minute
	nativeMergeQueueRepositoryExpiry = 5 * time.Minute
	nativeMergeQueueTerminalSweep    = 2 * time.Minute
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
	if nativeMergeQueueCleanupRequired(o.cfg) {
		return out
	}
	state.nativeMergeQueueDeferred = map[string]struct{}{}
	queue, ok := o.connector.(connector.PullRequestMergeQueue)
	if !ok {
		return out
	}
	pruneNativeMergeQueueEntries(state, out)
	stickyIssueID := stickyMergingIssueID(state, out, now, o.cfg.MergeFairnessAge)

	for _, candidate := range staleMergingQueueIssues(out, o.cfg, state, now) {
		issueID := strings.TrimSpace(candidate.ID)
		if !nativeMergeQueueCandidate(candidate, o.cfg) || staleMergingPullRequestDispatchActive(state, issueID) {
			continue
		}
		if cached, ok := state.nativeMergeQueueEntries[issueID]; ok && now.Sub(cached.CheckedAt) < nativeMergeQueueEntryRefresh {
			applyNativeMergeQueueEntry(out, issueID, cached.Entry)
			continue
		}
		if stickyIssueID != "" && issueID != stickyIssueID {
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

		_, previouslyQueued := state.nativeMergeQueueEntries[issueID]
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
			delete(state.nativeMergeQueueEntries, issueID)
			clearNativeMergeQueueEntry(out, issueID)
			o.logNativeMergeQueueFailure(candidate, "entry_missing", nil)
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

func (o *Orchestrator) reconcileUnsafeNativeMergeQueueIssues(
	ctx context.Context,
	state *State,
	issues []connector.Issue,
	previous []connector.Issue,
	now time.Time,
) []connector.Issue {
	out := mergeIssueSlices(issues, previous)
	if state == nil || !nativeMergeQueueCleanupRequired(o.cfg) {
		return out
	}
	queue, ok := o.connector.(connector.PullRequestMergeQueue)
	if !ok {
		return out
	}
	out = mergeIssueSlices(out, nativeMergeQueueRetryIssues(state.nativeQueueRetries))
	previousDeferred := state.nativeMergeQueueDeferred
	state.nativeMergeQueueDeferred = map[string]struct{}{}
	state.nativeQueueRetries = map[string]connector.Issue{}
	previousMerging := make(map[string]struct{}, len(previous))
	for _, issue := range previous {
		if mergeWorkerIssue(issue) {
			previousMerging[strings.TrimSpace(issue.ID)] = struct{}{}
		}
	}
	for _, issue := range out {
		if !nativeMergeQueueCleanupCandidate(issue, state, previousMerging, previousDeferred) {
			continue
		}
		o.reconcileUnsafeNativeMergeQueue(ctx, state, out, issue, queue, now)
	}
	pruneNativeMergeQueueEntries(state, out)
	return out
}

func nativeMergeQueueCleanupRequired(cfg Config) bool {
	return !cfg.MergeFastPathEnabled ||
		gateRequiresPullRequest(cfg.AutoPromote.Gate) ||
		gate.Effective(cfg.AutoPromote.Gate).SecurityAudit.Enabled
}

func (o *Orchestrator) fetchUnsafeNativeMergeQueueTerminalIssues(
	ctx context.Context,
	state *State,
	now time.Time,
	reserve githubBudgetReserveDecision,
) []connector.Issue {
	if state == nil || !nativeMergeQueueCleanupRequired(o.cfg) {
		return nil
	}
	if !state.nativeQueueSweepAt.IsZero() && now.Sub(state.nativeQueueSweepAt) < nativeMergeQueueTerminalSweep {
		return nil
	}
	if _, ok := o.connector.(connector.PullRequestMergeQueue); !ok {
		state.nativeQueueSweepAt = now
		return nil
	}
	if reserve.degraded {
		return nil
	}
	states := displayStateNames(o.cfg.TerminalStates)
	if len(states) == 0 {
		state.nativeQueueSweepAt = now
		return nil
	}
	issues, err := o.fetchObservedIssuesByStates(ctx, states)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("fetch unsafe native merge queue terminal issues failed", "error", err)
		}
		markRefreshError(state, "fetch unsafe native merge queue terminal issues failed: "+err.Error(), now)
		return nil
	}
	state.nativeQueueSweepAt = now
	return terminalIssues(issues, o.cfg.TerminalStates)
}

func nativeMergeQueueCleanupCandidate(
	issue connector.Issue,
	state *State,
	previousMerging map[string]struct{},
	previousDeferred map[string]struct{},
) bool {
	issueID := strings.TrimSpace(issue.ID)
	if issueID == "" {
		return false
	}
	if mergeWorkerIssue(issue) {
		return true
	}
	if _, ok := state.nativeMergeQueueEntries[issueID]; ok {
		return true
	}
	if _, ok := previousMerging[issueID]; ok {
		return true
	}
	if _, ok := previousDeferred[issueID]; ok {
		return true
	}
	if issue.PullRequest != nil && issue.PullRequest.MergeQueueEntry != nil {
		return true
	}
	return nativeMergeQueueRecoveryCandidate(issue)
}

func nativeMergeQueueRecoveryCandidate(issue connector.Issue) bool {
	if issue.PullRequest == nil || normalizePullRequestState(issue.PullRequest.State) != "open" {
		return false
	}
	return pullRequestRepository(issue) != "" && pullRequestNumber(issue) > 0
}

func (o *Orchestrator) reconcileUnsafeNativeMergeQueue(
	ctx context.Context,
	state *State,
	issues []connector.Issue,
	issue connector.Issue,
	queue connector.PullRequestMergeQueue,
	now time.Time,
) {
	issueID := strings.TrimSpace(issue.ID)
	status, err := queue.InspectPullRequestMergeQueue(ctx, issue)
	if err != nil {
		state.nativeMergeQueueDeferred[issueID] = struct{}{}
		state.nativeQueueRetries[issueID] = cloneIssue(issue)
		o.logNativeMergeQueueFailure(issue, "inspection_failed", err)
		return
	}
	if status.Entry == nil {
		delete(state.nativeMergeQueueEntries, issueID)
		clearNativeMergeQueueEntry(issues, issueID)
		return
	}
	entry := *status.Entry
	if err := queue.DequeuePullRequest(ctx, entry); err != nil {
		state.nativeMergeQueueDeferred[issueID] = struct{}{}
		state.nativeQueueRetries[issueID] = cloneIssue(issue)
		cacheNativeMergeQueueEntry(state, issueID, entry, now)
		applyNativeMergeQueueEntry(issues, issueID, entry)
		o.logNativeMergeQueueFailure(issue, "dequeue_failed", err)
		return
	}
	delete(state.nativeMergeQueueEntries, issueID)
	clearNativeMergeQueueEntry(issues, issueID)
	o.logNativeMergeQueueDequeued(issue, entry)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "merge_worker_native_queue_dequeued",
		Message: "dequeued " + issueLabel(issue) + " from the native merge queue",
	})
}

func nativeMergeQueueRetryIssues(retries map[string]connector.Issue) []connector.Issue {
	issueIDs := make([]string, 0, len(retries))
	for issueID := range retries {
		issueIDs = append(issueIDs, issueID)
	}
	sort.Strings(issueIDs)
	issues := make([]connector.Issue, 0, len(issueIDs))
	for _, issueID := range issueIDs {
		issues = append(issues, cloneIssue(retries[issueID]))
	}
	return issues
}

func nativeMergeQueueCandidate(issue connector.Issue, cfg Config) bool {
	if strings.TrimSpace(issue.ID) == "" || issue.PullRequest == nil {
		return false
	}
	if gateRequiresPullRequest(cfg.AutoPromote.Gate) {
		return false
	}
	if gate.Effective(cfg.AutoPromote.Gate).SecurityAudit.Enabled {
		return false
	}
	if _, revoked := mergeApprovalLabelRevoked(issue, cfg); revoked {
		return false
	}
	pullRequest := issue.PullRequest
	if pullRequestHydrationBlocksProgress(pullRequest) {
		return false
	}
	if _, revoked := mergeCITriggerLabelRevoked(issue, cfg); revoked {
		return false
	}
	return normalizePullRequestState(pullRequest.State) == "open" &&
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
		if _, deferred := state.nativeMergeQueueDeferred[issueID]; deferred {
			continue
		}
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

func clearNativeMergeQueueEntry(issues []connector.Issue, issueID string) {
	for index := range issues {
		if strings.TrimSpace(issues[index].ID) != issueID || issues[index].PullRequest == nil {
			continue
		}
		cloned := cloneIssue(issues[index])
		cloned.PullRequest.MergeQueueEntry = nil
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

func (o *Orchestrator) logNativeMergeQueueDequeued(issue connector.Issue, entry connector.PullRequestMergeQueueEntry) {
	if o.logger == nil {
		return
	}
	o.logger.Info("merge_worker_native_queue_dequeued", mergeWorkerLogAttrs(issue,
		"queue_entry_id", entry.ID,
		"queue_state", entry.State,
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
