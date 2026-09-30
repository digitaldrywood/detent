package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/store"
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
	o.pruneNativeMergeQueueEntries(ctx, state, out)
	runningRepositories := activeMergeWorkerRepositories(state)

	for _, candidate := range staleMergingQueueIssues(out, o.cfg, state, now) {
		if ctx.Err() != nil {
			state.nativeMergeQueueDeferred[strings.TrimSpace(candidate.ID)] = struct{}{}
			continue
		}
		if mergeWorkerRepositoryConsumed(runningRepositories, nativeMergeQueueRepositoryKey(candidate)) {
			continue
		}
		issueID := strings.TrimSpace(candidate.ID)
		if candidate.PullRequest == nil || normalizePullRequestState(candidate.PullRequest.State) != "open" || staleMergingPullRequestDispatchActive(state, issueID) {
			continue
		}
		head := strings.TrimSpace(candidate.PullRequest.HeadSHA)
		cachedEntry, entryCached := state.nativeMergeQueueEntries[issueID]
		cacheFresh := entryCached && now.Sub(cachedEntry.CheckedAt) < nativeMergeQueueEntryRefresh && cachedEntry.HeadSHA == head
		if cacheFresh && len(candidate.PullRequest.UnresolvedReviewThreads) == 0 {
			applyNativeMergeQueueEntry(out, issueID, cachedEntry.Entry)
			continue
		}

		var hydrated bool
		candidate, hydrated = o.hydrateAutoPromoteReviewThreads(ctx, candidate)
		if !hydrated || candidate.PullRequest == nil || strings.TrimSpace(candidate.PullRequest.HeadSHA) != head {
			state.nativeMergeQueueDeferred[issueID] = struct{}{}
			continue
		}

		if cacheFresh {
			if len(candidate.PullRequest.UnresolvedReviewThreads) > 0 {
				o.reworkNativeMergeQueueIssue(ctx, state, out, candidate, AutoPromoteReasonUnresolvedReviewThreads, now)
				continue
			}
			applyNativeMergeQueueEntry(out, issueID, cachedEntry.Entry)
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
		if len(candidate.PullRequest.UnresolvedReviewThreads) > 0 && (status.Available || status.Entry != nil) {
			candidate.PullRequest.MergeQueueEntry = status.Entry
			o.reworkNativeMergeQueueIssue(ctx, state, out, candidate, AutoPromoteReasonUnresolvedReviewThreads, now)
			continue
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
		if previouslyQueued && previous.HeadSHA != "" && previous.HeadSHA != strings.TrimSpace(status.HeadSHA) {
			// Cached ownership belongs to the old head, not its replacement.
			delete(state.nativeMergeQueueEntries, issueID)
			clearNativeMergeQueueEntry(out, issueID)
			candidate.PullRequest.MergeQueueEntry = nil
			previouslyQueued = false
		}
		if err := o.restoreNativeMergeQueueRemovals(ctx, state, candidate); err != nil {
			state.nativeMergeQueueDeferred[issueID] = struct{}{}
			o.logNativeMergeQueueFailure(candidate, "inspection_failed", err)
			continue
		}
		removalApplies := status.RemovalObserved && (strings.TrimSpace(status.RemovedHeadSHA) == "" || strings.TrimSpace(status.RemovedHeadSHA) == strings.TrimSpace(status.HeadSHA))
		if previouslyQueued && previous.Entry.EnqueuedAt != nil {
			// beforeCommit can identify a merge-group commit rather than the PR head.
			// A known enqueue time identifies which queue attempt the removal ended.
			removalApplies = status.RemovalObserved && status.RemovedAt != nil && status.RemovedAt.After(*previous.Entry.EnqueuedAt)
		}
		previousCount := len(state.nativeMergeQueueRemovals[nativeMergeQueueRemovalKey(candidate)])
		if removalApplies {
			reason := strings.TrimSpace(status.RemovalReason)
			if reason == "" {
				reason = "GitHub removed the pull request from the merge queue without a reason"
			}
			removals := appendNativeMergeQueueRemoval(state, nativeMergeQueueRemovalKey(candidate), reason, status.RemovedAt)
			if len(removals) > previousCount {
				if err := o.persistNativeMergeQueueRemoval(ctx, candidate, removals[len(removals)-1], now); err != nil {
					// Retry the observation, not admission, if its outcome was not stored.
					delete(state.nativeMergeQueueRemovals, nativeMergeQueueRemovalKey(candidate))
					state.nativeMergeQueueDeferred[issueID] = struct{}{}
					o.logNativeMergeQueueFailure(candidate, "inspection_failed", err)
					continue
				}
			}
		}
		removals := state.nativeMergeQueueRemovals[nativeMergeQueueRemovalKey(candidate)]
		if len(removals) >= mergeAttemptBudget {
			target := normalizeAutoPromoteConfig(o.cfg.AutoPromote).ReworkState
			state.nativeMergeQueueDeferred[issueID] = struct{}{}
			if err := o.updateIssueState(ctx, state, candidate, target, now, string(AutoPromoteReasonMergeRevocationLimit)); err != nil {
				o.logNativeMergeQueueFailure(candidate, "head_removed_from_queue", err)
				continue
			}
			delete(state.nativeMergeQueueEntries, issueID)
			clearNativeMergeQueueEntry(out, issueID)
			for index := range out {
				if out[index].ID == issueID {
					out[index].State = target
				}
			}
			o.parkNativeMergeQueueBudget(ctx, state, candidate, removals, now)
			continue
		}
		if len(removals) > previousCount {
			// Consume the ended attempt once. The next Merging pass uses
			// normal admission; queue removal is not a branch conflict.
			state.nativeMergeQueueDeferred[issueID] = struct{}{}
			delete(state.nativeMergeQueueEntries, issueID)
			clearNativeMergeQueueEntry(out, issueID)
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
		if automatedReviewPending(candidate.PullRequest, o.cfg) {
			o.requestAutomatedReview(ctx, candidate)
			state.nativeMergeQueueDeferred[issueID] = struct{}{}
			recordAutoPromoteSnapshotDecision(state, issueID, autoPromoteDecision(AutoPromoteActionAwaitReview, AutoPromoteReasonCodexReviewMissing))
			continue
		}
		if !nativeMergeQueueCandidate(candidate, o.cfg) {
			o.logNativeMergeQueueExcluded(state, candidate)
			continue
		}
		if connector.PullRequestConflicts(candidate.PullRequest.MergeableState) {
			o.reworkNativeMergeQueueIssue(ctx, state, out, candidate, AutoPromoteReasonMergeConflicts, now)
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
			message := strings.ToLower(err.Error())
			// Unknown hydration may lag an explicit conflict rejection. Clean
			// evidence takes precedence so an unchanged ready head cannot bounce.
			mergeable := strings.ToLower(strings.TrimSpace(candidate.PullRequest.MergeableState))
			if (mergeable == "" || mergeable == "unknown") && strings.Contains(message, "pull request has merge conflicts") {
				o.reworkNativeMergeQueueIssue(ctx, state, out, candidate, AutoPromoteReasonMergeConflicts, now)
				continue
			}
			if strings.Contains(message, "a conversation must be resolved before this pull request can be merged") {
				o.reworkNativeMergeQueueIssue(ctx, state, out, candidate, AutoPromoteReasonUnresolvedReviewThreads, now)
				continue
			}
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

// reworkNativeMergeQueueIssue uses the same Rework handoff as auto-promotion.
func (o *Orchestrator) reworkNativeMergeQueueIssue(ctx context.Context, state *State, issues []connector.Issue, issue connector.Issue, reason AutoPromoteReason, now time.Time) {
	state.nativeMergeQueueDeferred[strings.TrimSpace(issue.ID)] = struct{}{}
	if err := o.withdrawNativeMergeQueueEntry(ctx, state, issue); err != nil {
		o.logNativeMergeQueueFailure(issue, "inspection_failed", err)
		return
	}
	clearNativeMergeQueueEntry(issues, issue.ID)
	issue = cloneIssue(issue)
	issue.PullRequest.MergeQueueEntry = nil
	summary := AutoPromoteSummaryFromIssue(issue)
	decision := autoPromoteDecision(AutoPromoteActionRework, reason)
	target := normalizeAutoPromoteConfig(o.cfg.AutoPromote).ReworkState
	if !o.applyAutoPromoteDecision(ctx, state, issue, summary, decision, target, now) {
		return
	}
	o.clearAutoPromotedIssueDispatchMemory(state, issue.ID)
	o.recordAutoPromoteReworkHandoff(state, issue, summary, decision, target)
	for index := range issues {
		if issues[index].ID == issue.ID {
			issues[index] = cloneIssue(issue)
			issues[index].State = target
		}
	}
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
	if len(pullRequest.UnresolvedReviewThreads) > 0 || automatedReviewPending(pullRequest, cfg) {
		return false
	}
	if _, revoked := mergeCITriggerLabelRevoked(issue, cfg); revoked {
		return false
	}
	return normalizePullRequestState(pullRequest.State) == "open" &&
		!pullRequest.Draft &&
		strings.TrimSpace(pullRequest.HeadSHA) != "" &&
		nativeMergeQueueChecksReady(pullRequest) &&
		pullRequestRepository(issue) != "" &&
		pullRequestNumber(issue) > 0
}

// Queue admission accepts skipped and neutral checks; verification still belongs to the
// merge group. Do not confuse the connector's pending verification with a
// check that is still running or has never reported.
func nativeMergeQueueChecksReady(pr *connector.PullRequest) bool {
	if mergeWorkerCIGreen(pr.CIStatus) {
		return true
	}
	if pr.CIStatus != "pending" || len(pr.Checks) == 0 {
		return false
	}
	completed := make(map[string]bool, len(pr.Checks))
	for _, check := range pr.Checks {
		if check.Status == "success" && check.Conclusion == "success" {
			completed[check.Name] = true
			continue
		}
		if check.Status != "completed" || (check.Conclusion != "success" && check.Conclusion != "skipped" && check.Conclusion != "neutral") {
			return false
		}
		completed[check.Name] = true
	}
	for _, check := range pr.RequiredCheckFailures {
		if !completed[check.Name] {
			return false
		}
	}
	return true
}

// nativeMergeQueuePromotionHead reuses the queue's check and availability
// evidence so skipped PR jobs can reach the merge group without claiming CI
// passed on the PR head.
func (o *Orchestrator) nativeMergeQueuePromotionHead(ctx context.Context, state *State, issue connector.Issue, now time.Time) string {
	if normalizeState(o.cfg.AutoPromote.PassState) != normalizeState(autoPromoteMergingState) ||
		gate.Effective(o.cfg.AutoPromote.Gate).Kind != gate.KindCommand {
		return ""
	}
	if state == nil || issue.PullRequest == nil || issue.PullRequest.CIStatus != "pending" || !nativeMergeQueueCandidate(issue, o.cfg) {
		return ""
	}
	queue, ok := o.connector.(connector.PullRequestMergeQueue)
	if !ok {
		return ""
	}
	head := strings.TrimSpace(issue.PullRequest.HeadSHA)
	key := nativeMergeQueueRepositoryKey(issue)
	if cached, ok := state.nativeMergeQueueRepos[key]; ok && now.Sub(cached.CheckedAt) < nativeMergeQueueRepositoryExpiry {
		if cached.Available {
			return head
		}
		return ""
	}
	status, err := queue.InspectPullRequestMergeQueue(ctx, issue)
	if err != nil {
		o.logNativeMergeQueueFailure(issue, "inspection_failed", err)
		return ""
	}
	if strings.TrimSpace(status.HeadSHA) != head {
		o.logNativeMergeQueueFailure(issue, "head_changed", nil)
		return ""
	}
	state.nativeMergeQueueRepos[key] = nativeMergeQueueRepository{Available: status.Available, CheckedAt: now}
	if status.Available {
		return head
	}
	return ""
}

func nativeMergeQueueRepositoryKey(issue connector.Issue) string {
	baseRef := ""
	if issue.PullRequest != nil {
		baseRef = strings.TrimSpace(issue.PullRequest.BaseRef)
	}
	return mergeWorkerRepositoryKey(issue) + "@" + baseRef
}

// Withdrawal releases live provider ownership before forgetting local ownership.
// Missing cards and PRs no longer provide a live entry to withdraw.
func (o *Orchestrator) pruneNativeMergeQueueEntries(ctx context.Context, state *State, issues []connector.Issue) {
	present := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		present[strings.TrimSpace(issue.ID)] = struct{}{}
		if mergeWorkerIssue(issue) && issue.PullRequest != nil && normalizePullRequestState(issue.PullRequest.State) == "open" {
			continue
		}
		if err := o.withdrawNativeMergeQueueEntry(ctx, state, issue); err != nil {
			state.nativeMergeQueueDeferred[strings.TrimSpace(issue.ID)] = struct{}{}
			o.logNativeMergeQueueFailure(issue, "inspection_failed", err)
			continue
		}
		clearNativeMergeQueueEntry(issues, issue.ID)
	}
	for issueID := range state.nativeMergeQueueEntries {
		if _, ok := present[issueID]; ok {
			continue
		}
		issue := connector.Issue{ID: issueID}
		if err := o.withdrawNativeMergeQueueEntry(ctx, state, issue); err != nil {
			state.nativeMergeQueueDeferred[issueID] = struct{}{}
			o.logNativeMergeQueueFailure(issue, "inspection_failed", err)
		}
	}
}

func (o *Orchestrator) withdrawNativeMergeQueueEntry(ctx context.Context, state *State, issue connector.Issue) error {
	queue, ok := o.connector.(connector.PullRequestMergeQueue)
	if !ok {
		return nil
	}
	var entry *connector.PullRequestMergeQueueEntry
	if state != nil {
		if cached, ok := state.nativeMergeQueueEntries[strings.TrimSpace(issue.ID)]; ok {
			entry = &cached.Entry
		}
	}
	if issue.PullRequest != nil && issue.PullRequest.MergeQueueEntry != nil {
		entry = issue.PullRequest.MergeQueueEntry
	}
	if entry == nil {
		return nil
	}
	// Only a known open PR provides live ownership to withdraw. Missing cards
	// and landed or closed PRs release the local snapshot without a mutation.
	if issue.PullRequest != nil && normalizePullRequestState(issue.PullRequest.State) == "open" {
		if err := queue.DequeuePullRequest(ctx, *entry); err != nil {
			return fmt.Errorf("withdraw native merge queue entry: %w", err)
		}
	}

	if state != nil {
		delete(state.nativeMergeQueueEntries, strings.TrimSpace(issue.ID))
		clearNativeMergeQueueEntry(state.Pipeline, issue.ID)
	}
	return nil
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

func nativeMergeQueueRemovalKey(issue connector.Issue) string {
	head := ""
	if issue.PullRequest != nil {
		head = strings.TrimSpace(issue.PullRequest.HeadSHA)
	}
	return strings.TrimSpace(issue.ID) + "\n" + head
}

// Removals are keyed by issue and immutable PR head.
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
	if len(removals) > 0 && (removals[len(removals)-1] == key ||
		(removedAt != nil && strings.HasPrefix(removals[len(removals)-1], removedAt.UTC().Format(time.RFC3339Nano)+" "))) {
		return removals
	}
	removals = append(removals, key)
	state.nativeMergeQueueRemovals[issueID] = removals
	return removals
}

// The workflow timeline is the durable source for the existing removal budget.
// Cache it only after a successful read; a process restart must not grant retries.
func (o *Orchestrator) restoreNativeMergeQueueRemovals(ctx context.Context, state *State, issue connector.Issue) error {
	issueID := strings.TrimSpace(issue.ID)
	key := nativeMergeQueueRemovalKey(issue)
	if _, loaded := state.nativeMergeQueueRemovals[key]; loaded {
		return nil
	}
	reader, ok := o.workflowMetrics.(WorkflowMetricsTimelineReader)
	if !ok {
		return nil
	}
	timeline, err := reader.IssueWorkflowTimeline(ctx, store.IssueIdentity{ProjectID: o.workflowMetricsProjectID(), IssueID: issueID})
	if err != nil {
		return fmt.Errorf("load merge queue removals: %w", err)
	}
	var resetID int64
	for _, event := range timeline.Events {
		if event.PhaseType == store.WorkflowPhaseTypeLane &&
			stateIn(event.PhaseName, o.cfg.TerminalStates) && event.ID > resetID {
			resetID = event.ID
		}
	}
	removals := []string{}
	seen := map[string]bool{}
	for _, event := range timeline.Events {
		if event.ID <= resetID || event.PhaseType != store.WorkflowPhaseTypeMergeQueue || event.PhaseName != "head_removed_from_queue" || seen[event.Reason] {
			continue
		}
		var metadata struct {
			HeadSHA string `json:"head_sha"`
		}
		if event.MetadataJSON != "" {
			if err := json.Unmarshal([]byte(event.MetadataJSON), &metadata); err != nil {
				return fmt.Errorf("decode merge queue removal: %w", err)
			}
		}
		if metadata.HeadSHA != "" && (issue.PullRequest == nil || metadata.HeadSHA != strings.TrimSpace(issue.PullRequest.HeadSHA)) {
			continue
		}
		seen[event.Reason] = true
		removals = append(removals, event.Reason)
	}
	state.nativeMergeQueueRemovals[key] = removals
	return nil
}

func (o *Orchestrator) persistNativeMergeQueueRemoval(ctx context.Context, issue connector.Issue, removal string, now time.Time) error {
	if _, ok := o.workflowMetrics.(WorkflowMetricsTimelineReader); !ok {
		return nil
	}
	metadata, err := json.Marshal(struct {
		HeadSHA string `json:"head_sha"`
	}{strings.TrimSpace(issue.PullRequest.HeadSHA)})
	if err != nil {
		return err
	}
	_, err = o.workflowMetrics.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{
		ProjectID: o.workflowMetricsProjectID(), IssueID: strings.TrimSpace(issue.ID),
		Identifier: issue.Identifier, IssueURL: issue.URL,
		PhaseType: store.WorkflowPhaseTypeMergeQueue, PhaseName: "head_removed_from_queue",
		MetadataJSON: string(metadata), Reason: removal, Status: "failed", StartedAt: now, FinishedAt: now,
	})
	return err
}

func pruneNativeMergeQueueRemovals(state *State, issues []connector.Issue) {
	present := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		present[strings.TrimSpace(issue.ID)] = struct{}{}
	}
	for issueID := range state.nativeMergeQueueRemovals {
		if _, ok := present[strings.SplitN(issueID, "\n", 2)[0]]; !ok {
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
	if err := o.connector.CreateComment(ctx, issue.ID, mergeAttemptBudgetComment(issue, removals, normalizeAutoPromoteConfig(o.cfg.AutoPromote).ReworkState)); err != nil && o.logger != nil {
		o.logger.Warn("merge attempt budget comment failed", "issue_id", strings.TrimSpace(issue.ID), "identifier", issue.Identifier, "error", err)
	}
	if o.logger != nil {
		o.logger.Warn("merge_worker_revocation_limit", mergeWorkerLogAttrs(issue,
			"queue_removals", len(removals),
			"limit", mergeAttemptBudget,
			"target_state", normalizeAutoPromoteConfig(o.cfg.AutoPromote).ReworkState,
		)...)
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "merge_worker_revocation_limit",
		Message: fmt.Sprintf("routed %s to %s after %d merge queue removals", issueLabel(issue), normalizeAutoPromoteConfig(o.cfg.AutoPromote).ReworkState, len(removals)),
	})
}

func mergeAttemptBudgetComment(issue connector.Issue, removals []string, target string) string {
	var body strings.Builder
	body.WriteString("Detent routed this issue to ")
	body.WriteString(target)
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
	body.WriteString("\n- human_action: fix the cause of the removals, push a repaired PR head before retrying")
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
