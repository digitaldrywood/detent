package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	nativeMergeQueueEntryRefresh     = 2 * time.Minute
	nativeMergeQueueRepositoryExpiry = 5 * time.Minute
)

type nativeMergeQueueEntry struct {
	Entry     connector.PullRequestMergeQueueEntry
	HeadSHA   string
	CheckedAt time.Time
}

type nativeMergeQueueRepository struct {
	Available bool
	CheckedAt time.Time
}

// mergeAttemptBudget bounds queue entries per issue without a merge; the
// programmatic path shares it through maxIdenticalMergeRevocations.
const mergeAttemptBudget = 2

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
	pruneNativeMergeQueueRemovals(state, out)
	queue, ok := o.connector.(connector.PullRequestMergeQueue)
	if !ok {
		return out
	}
	pruneNativeMergeQueueEntries(state, out)
	stickyIssueID := stickyMergingIssueID(state, out, now, o.cfg.MergeFairnessAge)

	for _, candidate := range staleMergingQueueIssues(out, o.cfg, state, now) {
		if ctx.Err() != nil {
			state.nativeMergeQueueDeferred[strings.TrimSpace(candidate.ID)] = struct{}{}
			continue
		}
		if _, reserved := mergeReservationBlocks(state, candidate, now); reserved {
			continue
		}
		issueID := strings.TrimSpace(candidate.ID)
		if candidate.PullRequest == nil || normalizePullRequestState(candidate.PullRequest.State) != "open" || staleMergingPullRequestDispatchActive(state, issueID) {
			continue
		}
		if cached, ok := state.nativeMergeQueueEntries[issueID]; ok && now.Sub(cached.CheckedAt) < nativeMergeQueueEntryRefresh && cached.HeadSHA == strings.TrimSpace(candidate.PullRequest.HeadSHA) {
			applyNativeMergeQueueEntry(out, issueID, cached.Entry)
			continue
		}
		if mergeFairnessBlocks(state, stickyIssueID, candidate, now) {
			continue
		}

		repositoryKey := nativeMergeQueueRepositoryKey(candidate)
		repository, repositoryKnown := state.nativeMergeQueueRepos[repositoryKey]
		if repositoryKnown && now.Sub(repository.CheckedAt) >= nativeMergeQueueRepositoryExpiry {
			repositoryKnown = false
		}
		previous, previouslyQueued := state.nativeMergeQueueEntries[issueID]
		if repositoryKnown && !repository.Available && !previouslyQueued && candidate.PullRequest.MergeQueueEntry == nil {
			continue
		}

		status, err := queue.InspectPullRequestMergeQueue(ctx, candidate)
		if err != nil {
			state.nativeMergeQueueDeferred[issueID] = struct{}{}
			o.logNativeMergeQueueFailure(candidate, "inspection_failed", err)
			continue
		}
		if strings.TrimSpace(status.HeadSHA) != strings.TrimSpace(candidate.PullRequest.HeadSHA) {
			state.nativeMergeQueueDeferred[issueID] = struct{}{}
			o.logNativeMergeQueueFailure(candidate, "head_changed", nil)
			continue
		}
		state.nativeMergeQueueRepos[repositoryKey] = nativeMergeQueueRepository{
			Available: status.Available,
			CheckedAt: now,
		}
		if status.Entry != nil {
			cacheNativeMergeQueueEntry(state, issueID, *status.Entry, now)
			cached := state.nativeMergeQueueEntries[issueID]
			cached.HeadSHA = strings.TrimSpace(candidate.PullRequest.HeadSHA)
			state.nativeMergeQueueEntries[issueID] = cached
			applyNativeMergeQueueEntry(out, issueID, *status.Entry)
			o.logNativeMergeQueueDelegated(candidate, *status.Entry, "observed")
			continue
		}
		removalApplies := status.RemovalObserved && (strings.TrimSpace(status.RemovedHeadSHA) == "" || strings.TrimSpace(status.RemovedHeadSHA) == strings.TrimSpace(status.HeadSHA))
		if previouslyQueued && previous.Entry.EnqueuedAt != nil {
			// beforeCommit can identify a merge-group commit rather than the PR head.
			// A known enqueue time identifies which queue attempt the removal ended.
			removalApplies = status.RemovalObserved && status.RemovedAt != nil && status.RemovedAt.After(*previous.Entry.EnqueuedAt)
		}
		if removalApplies {
			state.nativeMergeQueueDeferred[issueID] = struct{}{}
			reason := strings.TrimSpace(status.RemovalReason)
			if reason == "" {
				reason = "GitHub removed the pull request from the merge queue without a reason"
			}
			removals := appendNativeMergeQueueRemoval(state, issueID, reason, status.RemovedAt)
			targetState, laneReason := autoPromoteReworkState, reason
			if len(removals) >= mergeAttemptBudget {
				targetState, laneReason = autoPromoteSourceState, string(AutoPromoteReasonMergeRevocationLimit)
			}
			if err := o.updateIssueState(ctx, state, candidate, targetState, now, laneReason); err != nil {
				o.logNativeMergeQueueFailure(candidate, "head_removed_from_queue", err)
				continue
			}
			delete(state.nativeMergeQueueEntries, issueID)
			clearNativeMergeQueueEntry(out, issueID)
			for index := range out {
				if out[index].ID == issueID {
					out[index].State = targetState
				}
			}
			if targetState == autoPromoteSourceState {
				o.parkNativeMergeQueueBudget(ctx, state, candidate, removals, now)
				continue
			}
			o.logNativeMergeQueueFailure(candidate, "head_removed_from_queue", nil)
			continue
		}
		if !status.Available {
			// A disabled queue with no provider entry releases cached ownership.
			// This only clears our snapshot; it never dequeues a provider entry.
			delete(state.nativeMergeQueueEntries, issueID)
			clearNativeMergeQueueEntry(out, issueID)
			continue
		}
		if previouslyQueued {
			// Absence alone from an available queue is not an outcome.
			applyNativeMergeQueueEntry(out, issueID, state.nativeMergeQueueEntries[issueID].Entry)
			continue
		}
		if !nativeMergeQueueCandidate(candidate, o.cfg) {
			o.logNativeMergeQueueExcluded(state, candidate)
			continue
		}
		if status.AdmissionLimit <= 0 || status.Depth >= status.AdmissionLimit {
			state.nativeMergeQueueDeferred[issueID] = struct{}{}
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
		cached := state.nativeMergeQueueEntries[issueID]
		cached.HeadSHA = strings.TrimSpace(candidate.PullRequest.HeadSHA)
		state.nativeMergeQueueEntries[issueID] = cached
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

func nativeMergeQueueCandidate(issue connector.Issue, cfg Config) bool {
	if strings.TrimSpace(issue.ID) == "" || issue.PullRequest == nil {
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
	if gateRequiresPullRequest(cfg.AutoPromote.Gate) && len(pullRequest.UnresolvedReviewThreads) > 0 {
		return false
	}
	if _, revoked := mergeCITriggerLabelRevoked(issue, cfg); revoked {
		return false
	}
	return normalizePullRequestState(pullRequest.State) == "open" &&
		!pullRequest.Draft &&
		strings.TrimSpace(pullRequest.HeadSHA) != "" &&
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
	if entry.EnqueuedAt == nil {
		entry.EnqueuedAt = &now
	}
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

// Removals are keyed by issue so a repaired head still spends the same budget.
// The last removal time deduplicates repeated observations of one removal.
func appendNativeMergeQueueRemoval(state *State, issueID, reason string, removedAt *time.Time) []string {
	if state.nativeMergeQueueRemovals == nil {
		state.nativeMergeQueueRemovals = map[string][]string{}
	}
	key := reason
	if removedAt != nil {
		key = removedAt.UTC().Format(time.RFC3339Nano) + " " + reason
	}
	removals := state.nativeMergeQueueRemovals[issueID]
	if len(removals) > 0 && removals[len(removals)-1] == key {
		return removals
	}
	removals = append(removals, key)
	state.nativeMergeQueueRemovals[issueID] = removals
	return removals
}

func pruneNativeMergeQueueRemovals(state *State, issues []connector.Issue) {
	present := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		present[strings.TrimSpace(issue.ID)] = struct{}{}
	}
	for issueID := range state.nativeMergeQueueRemovals {
		if _, ok := present[issueID]; !ok {
			delete(state.nativeMergeQueueRemovals, issueID)
		}
	}
}

func cloneNativeMergeQueueRemovals(removals map[string][]string) map[string][]string {
	out := make(map[string][]string, len(removals))
	for issueID, reasons := range removals {
		out[issueID] = append([]string(nil), reasons...)
	}
	return out
}

func (o *Orchestrator) parkNativeMergeQueueBudget(ctx context.Context, state *State, issue connector.Issue, removals []string, now time.Time) {
	delete(state.nativeMergeQueueRemovals, strings.TrimSpace(issue.ID))
	if err := o.connector.CreateComment(ctx, issue.ID, mergeAttemptBudgetComment(issue, removals)); err != nil && o.logger != nil {
		o.logger.Warn("merge attempt budget comment failed", "issue_id", strings.TrimSpace(issue.ID), "identifier", issue.Identifier, "error", err)
	}
	if o.logger != nil {
		o.logger.Warn("merge_worker_revocation_limit", mergeWorkerLogAttrs(issue,
			"queue_removals", len(removals),
			"limit", mergeAttemptBudget,
			"target_state", autoPromoteSourceState,
		)...)
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "merge_worker_revocation_limit",
		Message: fmt.Sprintf("routed %s to %s after %d merge queue removals", issueLabel(issue), autoPromoteSourceState, len(removals)),
	})
}

func mergeAttemptBudgetComment(issue connector.Issue, removals []string) string {
	var body strings.Builder
	body.WriteString("Detent routed this issue to ")
	body.WriteString(autoPromoteSourceState)
	body.WriteString(" after the merge queue removed its pull request ")
	body.WriteString(strconv.Itoa(len(removals)))
	body.WriteString(" times without merging.\n\n- reason: ")
	body.WriteString(string(AutoPromoteReasonMergeRevocationLimit))
	body.WriteString("\n- limit: ")
	body.WriteString(strconv.Itoa(mergeAttemptBudget))
	for _, removal := range removals {
		body.WriteString("\n- queue_removal: ")
		body.WriteString(removal)
	}
	if issue.PullRequest != nil {
		if url := strings.TrimSpace(issue.PullRequest.URL); url != "" {
			body.WriteString("\n- pull_request: ")
			body.WriteString(url)
		}
	}
	body.WriteString("\n- human_action: fix the cause of the removals, then move the issue to Rework")
	return body.String()
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
		"integration_head_sha", entry.HeadSHA,
		"integration_base_sha", entry.BaseSHA,
		"queue_entry_id", entry.ID,
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

// nativeMergeQueueOwnsIssue keeps provider-owned work out of every worker path,
// including snapshots that omit the queue entry. Availability expires only when
// a fresh provider inspection confirms the queue is unavailable.
func nativeMergeQueueOwnsIssue(state *State, issue connector.Issue, cfg Config) bool {
	if nativeMergeQueueHasEntry(state, issue) {
		return true
	}
	return state != nil && state.nativeMergeQueueRepos[nativeMergeQueueRepositoryKey(issue)].Available && nativeMergeQueueCandidate(issue, cfg)
}

func (o *Orchestrator) logNativeMergeQueueExcluded(state *State, issue connector.Issue) {
	if o.logger == nil || state == nil || issue.PullRequest == nil {
		return
	}
	issueID := strings.TrimSpace(issue.ID)
	head := strings.TrimSpace(issue.PullRequest.HeadSHA)
	if state.nativeMergeQueueExcluded[issueID] == head {
		return
	}
	state.nativeMergeQueueExcluded[issueID] = head
	o.logger.Info("merge_worker_native_queue_excluded", mergeWorkerLogAttrs(issue,
		"draft", issue.PullRequest.Draft,
		"ci_status", issue.PullRequest.CIStatus,
		"unresolved_review_threads", len(issue.PullRequest.UnresolvedReviewThreads),
		"security_audit", gate.Effective(o.cfg.AutoPromote.Gate).SecurityAudit.Enabled,
	)...)
}

func nativeMergeQueueHasEntry(state *State, issue connector.Issue) bool {
	if issue.PullRequest != nil && issue.PullRequest.MergeQueueEntry != nil {
		return true
	}
	if state == nil {
		return false
	}
	_, ok := state.nativeMergeQueueEntries[strings.TrimSpace(issue.ID)]
	return ok
}
