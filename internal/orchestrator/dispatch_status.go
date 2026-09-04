package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
)

func (o *Orchestrator) observeProjectDispatchStatus(
	ctx context.Context,
	state *State,
	candidates []connector.Issue,
	decisions []dispatchPlanDecision,
	outcomes map[string]dispatchIssueOutcome,
	now time.Time,
) {
	if o == nil || state == nil || now.IsZero() {
		return
	}
	status := projectDispatchStatusFromCycle(
		state.DispatchStatus,
		strings.TrimSpace(o.cfg.Project.ID),
		candidates,
		decisions,
		outcomes,
		now,
	)
	state.DispatchStatus = status
	recorder, ok := o.workAttempts.(store.ProjectDispatchStatusStore)
	if !ok {
		return
	}
	if err := recorder.RecordProjectDispatchStatus(ctx, status); err != nil && o.logger != nil {
		o.logger.Warn("record project dispatch status failed", "project_id", status.ProjectID, "error", err)
	}
}

func projectDispatchStatusFromCycle(
	previous store.ProjectDispatchStatus,
	projectID string,
	candidates []connector.Issue,
	decisions []dispatchPlanDecision,
	outcomes map[string]dispatchIssueOutcome,
	now time.Time,
) store.ProjectDispatchStatus {
	now = now.UTC().Truncate(time.Second)
	latest := make(map[string]dispatchPlanDecision, len(decisions))
	for _, decision := range decisions {
		if identity := workflowIssueIdentityKey(decision.Issue); identity != "" {
			latest[identity] = decision
		}
	}
	identities := dispatchCandidateIdentities(candidates)
	tracked := identities[:0]
	for _, identity := range identities {
		if decision, ok := latest[identity]; ok && dispatchStatusExcludesCandidate(decision) {
			continue
		}
		tracked = append(tracked, identity)
	}
	identities = tracked
	status := store.ProjectDispatchStatus{
		ProjectID:            strings.TrimSpace(projectID),
		CandidateCount:       len(identities),
		CandidateFingerprint: dispatchCandidateFingerprint(identities),
		LastSelectedAt:       cloneTimePointer(previous.LastSelectedAt),
		ObservedAt:           now,
	}

	commonWaitReason := ""
	commonWaitReasonCode := ""
	mixedWaitReasonDetail := false
	allSkipped := len(identities) > 0
	for _, identity := range identities {
		decision, ok := latest[identity]
		if !ok {
			allSkipped = false
			continue
		}
		waitReason := ""
		waitReasonCode := ""
		if decision.Selected {
			outcome, attempted := outcomes[identity]
			if attempted && outcome.dispatched {
				status.SelectedCount++
				status.EligibleCandidateCount++
				allSkipped = false
				continue
			}
			if attempted {
				waitReasonCode = strings.TrimSpace(outcome.reason)
				waitReason = strings.TrimSpace(outcome.waitReason)
				if waitReason == "" {
					waitReason = schedulerDecisionWaitReason(outcome.reason)
				}
				if outcome.reason == projectFailureBreakerDispatchPaused {
					status.EligibleCandidateCount++
				}
			}
		} else {
			waitReasonCode = strings.TrimSpace(decision.SkipReason)
			waitReason = dispatchPlanDecisionWaitReason(decision)
			if decision.SkipReason == dispatchSkipProjectFailureBreaker {
				status.EligibleCandidateCount++
			}
		}
		status.SkippedCount++
		if waitReason == "" || waitReasonCode == "" {
			allSkipped = false
			continue
		}
		if commonWaitReasonCode == "" {
			commonWaitReasonCode = waitReasonCode
			commonWaitReason = waitReason
		} else if commonWaitReasonCode != waitReasonCode {
			allSkipped = false
		} else if commonWaitReason != waitReason {
			mixedWaitReasonDetail = true
		}
	}

	if status.SelectedCount > 0 {
		status.LastSelectedAt = &now
		return status
	}
	if !allSkipped || status.SkippedCount != status.CandidateCount {
		return status
	}
	status.WaitReasonCode = commonWaitReasonCode
	status.WaitReason = commonWaitReason
	if mixedWaitReasonDetail {
		status.WaitReason = schedulerDecisionWaitReason(commonWaitReasonCode)
	}
	allSkippedSince := now
	continuousStarvation := previous.CandidateCount > 0 &&
		previous.SelectedCount == 0 &&
		previous.SkippedCount == previous.CandidateCount
	matchingRefusalWindow := previous.CandidateFingerprint == status.CandidateFingerprint &&
		dispatchStatusWaitReasonMatches(previous, status)
	if continuousStarvation || matchingRefusalWindow {
		if previous.AllSkippedSince != nil && !previous.AllSkippedSince.IsZero() && previous.AllSkippedSince.Before(allSkippedSince) {
			allSkippedSince = *previous.AllSkippedSince
		}
	}
	if candidateReadySince, ok := dispatchCandidateReadySince(candidates, identities, now); ok {
		if previous.LastSelectedAt != nil &&
			!previous.LastSelectedAt.IsZero() &&
			previous.LastSelectedAt.After(candidateReadySince) &&
			!previous.LastSelectedAt.After(now) {
			candidateReadySince = *previous.LastSelectedAt
		}
		if candidateReadySince.Before(allSkippedSince) {
			allSkippedSince = candidateReadySince
		}
	}
	status.AllSkippedSince = &allSkippedSince
	return status
}

func dispatchCandidateReadySince(candidates []connector.Issue, identities []string, now time.Time) (time.Time, bool) {
	tracked := make(map[string]struct{}, len(identities))
	for _, identity := range identities {
		tracked[identity] = struct{}{}
	}
	readySince := time.Time{}
	for _, candidate := range candidates {
		if _, ok := tracked[workflowIssueIdentityKey(candidate)]; !ok ||
			candidate.StageUpdatedAt == nil ||
			candidate.StageUpdatedAt.IsZero() ||
			candidate.StageUpdatedAt.After(now) {
			continue
		}
		if readySince.IsZero() || candidate.StageUpdatedAt.Before(readySince) {
			readySince = candidate.StageUpdatedAt.UTC()
		}
	}
	return readySince, !readySince.IsZero()
}

func dispatchStatusWaitReasonMatches(previous store.ProjectDispatchStatus, current store.ProjectDispatchStatus) bool {
	previousCode := strings.TrimSpace(previous.WaitReasonCode)
	currentCode := strings.TrimSpace(current.WaitReasonCode)
	if previousCode != "" {
		return previousCode == currentCode
	}
	previousReason := strings.TrimSpace(previous.WaitReason)
	return previousReason == strings.TrimSpace(current.WaitReason) || previousReason == currentCode
}

func dispatchPlanDecisionWaitReason(decision dispatchPlanDecision) string {
	if detail := strings.TrimSpace(decision.SkipDetail); detail != "" {
		return detail
	}
	return schedulerDecisionWaitReason(decision.SkipReason)
}

func dispatchStatusExcludesCandidate(decision dispatchPlanDecision) bool {
	if decision.Selected {
		return false
	}
	switch strings.TrimSpace(decision.SkipReason) {
	case dispatchSkipAlreadyRunning, dispatchSkipAlreadyClaimed:
		return true
	default:
		return false
	}
}

func dispatchCandidateIdentities(candidates []connector.Issue) []string {
	seen := make(map[string]struct{}, len(candidates))
	identities := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		identity := workflowIssueIdentityKey(candidate)
		if identity == "" {
			continue
		}
		if _, ok := seen[identity]; ok {
			continue
		}
		seen[identity] = struct{}{}
		identities = append(identities, identity)
	}
	sort.Strings(identities)
	return identities
}

func dispatchCandidateFingerprint(identities []string) string {
	if len(identities) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(identities, "\x00")))
	return hex.EncodeToString(sum[:])
}
