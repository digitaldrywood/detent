package tracker

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

func DeriveDispatchQueue(candidates []WorkItem, snapshot DispatchSnapshot) DispatchQueue {
	queue := DispatchQueue{
		Dispatchable:    make([]WorkItem, 0, len(candidates)),
		NonDispatchable: make([]WorkItem, 0, len(candidates)),
	}
	for _, candidate := range candidates {
		candidate.Dispatchability = deriveSnapshotDispatchability(candidate, snapshot)
		if candidate.Dispatchability.Dispatchable {
			queue.Dispatchable = append(queue.Dispatchable, candidate)
		} else {
			queue.NonDispatchable = append(queue.NonDispatchable, candidate)
		}
	}
	sortWorkItemsForDispatch(queue.Dispatchable)
	sortWorkItemsForDispatch(queue.NonDispatchable)
	return queue
}

func deriveSnapshotDispatchability(item WorkItem, snapshot DispatchSnapshot) Dispatchability {
	reasons := deriveDispatchReasons(item, snapshot.EvaluatedAt, snapshot.TargetMachineID)
	ownLease := ownsActiveLease(item, snapshot)
	if usage, ok := snapshot.RepositoryConcurrency[item.Repository.ID]; ok && concurrencyFull(usage, ownLease) {
		active := effectiveActive(usage.Active, ownLease)
		reasons = append(reasons, DispatchReason{
			Code:       DispatchReasonRepositoryConcurrencyLimit,
			Message:    fmt.Sprintf("repository concurrency limit reached: %d of %d active", active, usage.Limit),
			Repository: item.Repository.ID,
			Active:     active,
			Limit:      usage.Limit,
		})
	}
	project := workItemProject(item)
	if usage, ok := projectConcurrency(snapshot.ProjectConcurrency, project); ok && concurrencyFull(usage, ownLease) {
		active := effectiveActive(usage.Active, ownLease)
		reasons = append(reasons, DispatchReason{
			Code:    DispatchReasonProjectConcurrencyLimit,
			Message: fmt.Sprintf("project concurrency limit reached: %d of %d active", active, usage.Limit),
			Scope:   project,
			Active:  active,
			Limit:   usage.Limit,
		})
	}
	if reason := machineAvailabilityReason(item, snapshot); reason != nil {
		reasons = append(reasons, *reason)
	}
	if reason := operatorPauseReason(item, snapshot); reason != nil {
		reasons = append(reasons, *reason)
	}
	if reason := syncSafetyReason(item, snapshot); reason != nil {
		reasons = append(reasons, *reason)
	}
	return Dispatchability{Dispatchable: len(reasons) == 0, Reasons: reasons}
}

func machineAvailabilityReason(item WorkItem, snapshot DispatchSnapshot) *DispatchReason {
	compatible := false
	healthy := false
	active := 0
	limit := 0
	for _, machine := range snapshot.Machines {
		if snapshot.TargetMachineID != "" && machine.ID != snapshot.TargetMachineID {
			continue
		}
		if !machineCompatible(machine, item) {
			continue
		}
		compatible = true
		if !machine.Healthy {
			continue
		}
		healthy = true
		machineActive := effectiveActive(machine.ActiveLeases, ownsActiveLease(item, snapshot) && machine.ID == snapshot.TargetMachineID)
		if snapshot.TargetMachineID != "" {
			active = machineActive
			limit = machine.Capacity
		}
		if machine.Capacity > machineActive {
			return nil
		}
	}
	if !compatible {
		return &DispatchReason{
			Code:      DispatchReasonNoCompatibleMachine,
			Message:   "no compatible machine is available for this work item",
			MachineID: snapshot.TargetMachineID,
		}
	}
	if !healthy {
		return &DispatchReason{
			Code:      DispatchReasonMachineUnhealthy,
			Message:   "no compatible machine is healthy",
			MachineID: snapshot.TargetMachineID,
		}
	}
	return &DispatchReason{
		Code:      DispatchReasonMachineCapacityFull,
		Message:   "all compatible healthy machines are at capacity",
		MachineID: snapshot.TargetMachineID,
		Active:    active,
		Limit:     limit,
	}
}

func machineCompatible(machine MachineAvailability, item WorkItem) bool {
	if len(machine.RepositoryIDs) > 0 && !containsRepository(machine.RepositoryIDs, item.Repository.ID) {
		return false
	}
	if len(machine.ProjectScopes) == 0 {
		return true
	}
	project := workItemProject(item)
	for _, scope := range machine.ProjectScopes {
		if strings.EqualFold(strings.TrimSpace(scope), project) {
			return true
		}
	}
	return false
}

func operatorPauseReason(item WorkItem, snapshot DispatchSnapshot) *DispatchReason {
	if snapshot.OperatorPaused {
		return &DispatchReason{Code: DispatchReasonOperatorPaused, Message: "dispatch is paused by the operator"}
	}
	if containsRepository(snapshot.PausedRepositories, item.Repository.ID) {
		return &DispatchReason{
			Code:       DispatchReasonOperatorPaused,
			Message:    "dispatch is paused by the operator for this repository",
			Repository: item.Repository.ID,
		}
	}
	project := workItemProject(item)
	if project != "" && containsFolded(snapshot.PausedProjects, project) {
		return &DispatchReason{
			Code:    DispatchReasonOperatorPaused,
			Message: "dispatch is paused by the operator for this project",
			Scope:   project,
		}
	}
	return nil
}

func syncSafetyReason(item WorkItem, snapshot DispatchSnapshot) *DispatchReason {
	if snapshot.SyncSafetyBlocked {
		return &DispatchReason{Code: DispatchReasonSyncUnsafe, Message: "dispatch is prevented by the Hub synchronization safety gate"}
	}
	if containsRepository(snapshot.SyncUnsafeRepositories, item.Repository.ID) {
		return &DispatchReason{
			Code:       DispatchReasonSyncUnsafe,
			Message:    "repository synchronization is not safe for dispatch",
			Repository: item.Repository.ID,
		}
	}
	return nil
}

func concurrencyFull(usage ConcurrencyUsage, discountOwnedLease bool) bool {
	return usage.Limit <= 0 || effectiveActive(usage.Active, discountOwnedLease) >= usage.Limit
}

func effectiveActive(active int, discountOwnedLease bool) int {
	active = nonNegative(active)
	if discountOwnedLease && active > 0 {
		return active - 1
	}
	return active
}

func nonNegative(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

func containsRepository(repositories []RepositoryID, repository RepositoryID) bool {
	for _, candidate := range repositories {
		if candidate == repository {
			return true
		}
	}
	return false
}

func containsFolded(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), target) {
			return true
		}
	}
	return false
}

func projectConcurrency(usages map[string]ConcurrencyUsage, project string) (ConcurrencyUsage, bool) {
	if project == "" {
		return ConcurrencyUsage{}, false
	}
	if usage, ok := usages[project]; ok {
		return usage, true
	}
	keys := make([]string, 0)
	for scope := range usages {
		if strings.EqualFold(strings.TrimSpace(scope), project) {
			keys = append(keys, scope)
		}
	}
	if len(keys) == 0 {
		return ConcurrencyUsage{}, false
	}
	sort.Strings(keys)
	return usages[keys[0]], true
}

func workItemProject(item WorkItem) string {
	if item.Queue == nil {
		return ""
	}
	return strings.TrimSpace(item.Queue.Scope)
}

func ownsActiveLease(item WorkItem, snapshot DispatchSnapshot) bool {
	return snapshot.TargetMachineID != "" && item.ActiveLease != nil && item.ActiveLease.Machine.ID == snapshot.TargetMachineID && (snapshot.EvaluatedAt.IsZero() || item.ActiveLease.ExpiresAt.After(snapshot.EvaluatedAt))
}

func sortWorkItemsForDispatch(items []WorkItem) {
	sort.SliceStable(items, func(i, j int) bool {
		return compareWorkItemsForDispatch(items[i], items[j]) < 0
	})
}

func compareWorkItemsForDispatch(left, right WorkItem) int {
	if comparison := compareInt(queuePriority(left), queuePriority(right)); comparison != 0 {
		return comparison
	}
	if comparison := compareQueueRanks(left, right); comparison != 0 {
		return comparison
	}
	if comparison := compareCreationTimes(left.CreatedAt, right.CreatedAt); comparison != 0 {
		return comparison
	}
	leftRepository := canonicalRepository(left.Repository)
	rightRepository := canonicalRepository(right.Repository)
	if comparison := strings.Compare(leftRepository, rightRepository); comparison != 0 {
		return comparison
	}
	return compareInt(left.GitHub.Number, right.GitHub.Number)
}

func queuePriority(item WorkItem) int {
	if item.Queue == nil || item.Queue.PriorityRank == nil {
		return QueuePriorityLow + 1
	}
	switch *item.Queue.PriorityRank {
	case QueuePriorityUrgent, QueuePriorityHigh, QueuePriorityNormal, QueuePriorityLow:
		return *item.Queue.PriorityRank
	default:
		return QueuePriorityLow + 1
	}
}

func compareQueueRanks(left, right WorkItem) int {
	leftRank := ""
	rightRank := ""
	if left.Queue != nil {
		leftRank = strings.TrimSpace(left.Queue.Rank)
	}
	if right.Queue != nil {
		rightRank = strings.TrimSpace(right.Queue.Rank)
	}
	if leftRank == "" && rightRank != "" {
		return 1
	}
	if leftRank != "" && rightRank == "" {
		return -1
	}
	return strings.Compare(leftRank, rightRank)
}

func compareCreationTimes(left, right *time.Time) int {
	if left == nil && right != nil {
		return 1
	}
	if left != nil && right == nil {
		return -1
	}
	if left == nil {
		return 0
	}
	if left.Before(*right) {
		return -1
	}
	if left.After(*right) {
		return 1
	}
	return 0
}

func canonicalRepository(repository RepositoryReference) string {
	return strings.ToLower(strings.TrimSpace(repository.Owner)) + "/" + strings.ToLower(strings.TrimSpace(repository.Name))
}

func compareInt(left, right int) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}
