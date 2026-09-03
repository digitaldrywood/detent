package tracker

import (
	"strings"
	"time"
	"unicode/utf8"
)

const bodyExcerptLimit = 500

type Record struct {
	ID              WorkItemID
	Repository      RepositoryReference
	GitHub          GitHubIssueReference
	Title           string
	Body            string
	URL             string
	SourceState     string
	WorkflowState   *WorkflowState
	Queue           *QueueSummary
	AuthorID        string
	Labels          []string
	Assignees       []string
	CreatedAt       *time.Time
	UpdatedAt       *time.Time
	SourceUpdatedAt *time.Time
	SourceSyncedAt  *time.Time
	Blockers        []WorkItemReference
	Dependents      []WorkItemReference
	Lease           *LeaseSummary
	PullRequests    []PullRequestSummary
	SyncStatus      SyncStatus
	ObservedAt      time.Time
}

func Normalize(record Record) WorkItem {
	github := record.GitHub
	github.NodeID = strings.TrimSpace(github.NodeID)
	if github.DatabaseID != nil {
		databaseID := *github.DatabaseID
		github.DatabaseID = &databaseID
	}
	item := WorkItem{
		ID:              record.ID,
		Repository:      normalizeRepository(record.Repository),
		GitHub:          github,
		Title:           strings.TrimSpace(record.Title),
		BodyExcerpt:     bodyExcerpt(record.Body),
		URL:             strings.TrimSpace(record.URL),
		SourceState:     normalizeSourceState(record.SourceState),
		WorkflowState:   normalizeWorkflowState(record.WorkflowState),
		Queue:           normalizeQueue(record.Queue),
		AuthorID:        strings.TrimSpace(record.AuthorID),
		Labels:          normalizeStrings(record.Labels),
		Assignees:       normalizeStrings(record.Assignees),
		CreatedAt:       cloneTime(record.CreatedAt),
		UpdatedAt:       cloneTime(record.UpdatedAt),
		SourceUpdatedAt: cloneTime(record.SourceUpdatedAt),
		SourceSyncedAt:  cloneTime(record.SourceSyncedAt),
		Blockers:        normalizeReferences(record.Blockers),
		Dependents:      normalizeReferences(record.Dependents),
		PullRequests:    normalizePullRequests(record.PullRequests),
		SyncStatus:      normalizeSyncStatus(record.SyncStatus),
	}
	item.ActiveLease = activeLease(record.Lease, record.ObservedAt)
	item.Dispatchability = deriveDispatchability(item)
	return item
}

func normalizeRecords(records []Record) []WorkItem {
	items := make([]WorkItem, 0, len(records))
	for _, record := range records {
		items = append(items, Normalize(record))
	}
	return items
}

func deriveDispatchability(item WorkItem) Dispatchability {
	reasons := deriveDispatchReasons(item, time.Time{}, "", "")
	return Dispatchability{Dispatchable: len(reasons) == 0, Reasons: reasons}
}

func deriveDispatchReasons(item WorkItem, evaluatedAt time.Time, targetMachineID MachineID, targetSessionID string) []DispatchReason {
	reasons := make([]DispatchReason, 0)
	switch item.SourceState {
	case SourceStateClosed:
		reasons = append(reasons, DispatchReason{Code: DispatchReasonIssueClosed, Message: "issue is closed"})
	case SourceStateOpen:
	default:
		reasons = append(reasons, DispatchReason{Code: DispatchReasonSourceStateUnknown, Message: "issue source state is unknown"})
	}

	if item.WorkflowState == nil || item.WorkflowState.Name == "" {
		reasons = append(reasons, DispatchReason{Code: DispatchReasonWorkflowStateMissing, Message: "workflow state is unavailable"})
	} else if item.WorkflowState.Terminal {
		reasons = append(reasons, DispatchReason{Code: DispatchReasonWorkflowStateTerminal, Message: "workflow state is terminal"})
	} else if !item.WorkflowState.Dispatchable {
		reasons = append(reasons, DispatchReason{Code: DispatchReasonWorkflowStateNotDispatchable, Message: "workflow state is not dispatchable"})
	}

	for i := range item.Blockers {
		blocker := item.Blockers[i]
		if blocker.WorkflowState != nil && blocker.WorkflowState.Terminal {
			continue
		}
		blockerID := blocker.ID
		reasons = append(reasons, DispatchReason{
			Code:       DispatchReasonBlockerUnresolved,
			Message:    "required blocker is unresolved",
			WorkItemID: &blockerID,
		})
	}

	if leaseIsActive(item.ActiveLease, evaluatedAt) && !leaseOwnedBy(item.ActiveLease, targetMachineID, targetSessionID) {
		reasons = append(reasons, DispatchReason{
			Code:      DispatchReasonLeaseActive,
			Message:   "work item has an active lease held by another machine or session",
			LeaseID:   item.ActiveLease.ID,
			MachineID: item.ActiveLease.Machine.ID,
			SessionID: item.ActiveLease.SessionID,
		})
	}
	switch item.SyncStatus {
	case SyncStatusError:
		reasons = append(reasons, DispatchReason{Code: DispatchReasonSyncError, Message: "GitHub synchronization has an error"})
	case SyncStatusStale:
		reasons = append(reasons, DispatchReason{Code: DispatchReasonSyncStale, Message: "GitHub projection is stale"})
	}

	return reasons
}

func leaseIsActive(lease *LeaseSummary, evaluatedAt time.Time) bool {
	return lease != nil && (evaluatedAt.IsZero() || lease.ExpiresAt.After(evaluatedAt))
}

func leaseOwnedBy(lease *LeaseSummary, targetMachineID MachineID, targetSessionID string) bool {
	targetSessionID = strings.TrimSpace(targetSessionID)
	return targetMachineID != "" && targetSessionID != "" && lease != nil && lease.Machine.ID == targetMachineID && strings.TrimSpace(lease.SessionID) == targetSessionID
}

func normalizeRepository(repository RepositoryReference) RepositoryReference {
	repository.GitHubNodeID = strings.TrimSpace(repository.GitHubNodeID)
	repository.Owner = strings.TrimSpace(repository.Owner)
	repository.Name = strings.TrimSpace(repository.Name)
	return repository
}

func normalizeSourceState(value string) SourceState {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case string(SourceStateOpen):
		return SourceStateOpen
	case string(SourceStateClosed):
		return SourceStateClosed
	default:
		return SourceStateUnknown
	}
}

func normalizeSyncStatus(value SyncStatus) SyncStatus {
	switch SyncStatus(strings.ToLower(strings.TrimSpace(string(value)))) {
	case SyncStatusSynced:
		return SyncStatusSynced
	case SyncStatusPending:
		return SyncStatusPending
	case SyncStatusRetrying:
		return SyncStatusRetrying
	case SyncStatusError:
		return SyncStatusError
	case SyncStatusStale:
		return SyncStatusStale
	default:
		return SyncStatusStale
	}
}

func normalizeWorkflowState(state *WorkflowState) *WorkflowState {
	if state == nil {
		return nil
	}
	result := *state
	result.SourceName = strings.TrimSpace(result.SourceName)
	result.Name = strings.TrimSpace(result.Name)
	return &result
}

func normalizeQueue(queue *QueueSummary) *QueueSummary {
	if queue == nil {
		return nil
	}
	result := *queue
	result.Scope = strings.TrimSpace(result.Scope)
	result.State = strings.TrimSpace(result.State)
	result.Rank = strings.TrimSpace(result.Rank)
	if result.PriorityRank != nil {
		priority := *result.PriorityRank
		result.PriorityRank = &priority
	}
	return &result
}

func normalizeReferences(references []WorkItemReference) []WorkItemReference {
	result := make([]WorkItemReference, 0, len(references))
	for _, reference := range references {
		reference.Repository = strings.TrimSpace(reference.Repository)
		reference.Title = strings.TrimSpace(reference.Title)
		reference.URL = strings.TrimSpace(reference.URL)
		reference.SourceState = normalizeSourceState(string(reference.SourceState))
		reference.WorkflowState = normalizeWorkflowState(reference.WorkflowState)
		result = append(result, reference)
	}
	return result
}

func normalizePullRequests(pullRequests []PullRequestSummary) []PullRequestSummary {
	result := make([]PullRequestSummary, 0, len(pullRequests))
	for _, pullRequest := range pullRequests {
		pullRequest.GitHubNodeID = strings.TrimSpace(pullRequest.GitHubNodeID)
		pullRequest.Title = strings.TrimSpace(pullRequest.Title)
		pullRequest.URL = strings.TrimSpace(pullRequest.URL)
		pullRequest.State = strings.ToLower(strings.TrimSpace(pullRequest.State))
		pullRequest.HeadRef = strings.TrimSpace(pullRequest.HeadRef)
		pullRequest.HeadSHA = strings.TrimSpace(pullRequest.HeadSHA)
		pullRequest.BaseRef = strings.TrimSpace(pullRequest.BaseRef)
		pullRequest.BaseSHA = strings.TrimSpace(pullRequest.BaseSHA)
		pullRequest.Merge.State = strings.TrimSpace(pullRequest.Merge.State)
		pullRequest.Merge.RefreshedAt = cloneTime(pullRequest.Merge.RefreshedAt)
		result = append(result, pullRequest)
	}
	return result
}

func activeLease(lease *LeaseSummary, observedAt time.Time) *LeaseSummary {
	if lease == nil || !observedAt.IsZero() && !lease.ExpiresAt.After(observedAt) {
		return nil
	}
	result := *lease
	result.ID = LeaseID(strings.TrimSpace(string(result.ID)))
	result.Machine.ID = MachineID(strings.TrimSpace(string(result.Machine.ID)))
	result.Machine.Hostname = strings.TrimSpace(result.Machine.Hostname)
	result.Machine.DisplayName = strings.TrimSpace(result.Machine.DisplayName)
	result.SessionID = strings.TrimSpace(result.SessionID)
	return &result
}

func normalizeStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result
}

func bodyExcerpt(body string) string {
	body = strings.TrimSpace(body)
	if utf8.RuneCountInString(body) <= bodyExcerptLimit {
		return body
	}
	runes := []rune(body)
	return strings.TrimSpace(string(runes[:bodyExcerptLimit])) + "…"
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	result := *value
	return &result
}
