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
	allSkipped := len(identities) > 0
	for _, identity := range identities {
		decision, ok := latest[identity]
		if !ok {
			allSkipped = false
			continue
		}
		waitReason := ""
		if decision.Selected {
			outcome, attempted := outcomes[identity]
			if attempted && outcome.dispatched {
				status.SelectedCount++
				allSkipped = false
				continue
			}
			if attempted {
				waitReason = schedulerDecisionWaitReason(outcome.reason)
			}
		} else {
			waitReason = schedulerDecisionWaitReason(decision.SkipReason)
		}
		status.SkippedCount++
		if waitReason == "" {
			allSkipped = false
			continue
		}
		if commonWaitReason == "" {
			commonWaitReason = waitReason
		} else if commonWaitReason != waitReason {
			allSkipped = false
		}
	}

	if status.SelectedCount > 0 {
		status.LastSelectedAt = &now
		return status
	}
	if !allSkipped || status.SkippedCount != status.CandidateCount {
		return status
	}
	status.WaitReason = commonWaitReason
	if previous.CandidateFingerprint == status.CandidateFingerprint &&
		strings.TrimSpace(previous.WaitReason) == status.WaitReason &&
		previous.AllSkippedSince != nil && !previous.AllSkippedSince.IsZero() {
		status.AllSkippedSince = cloneTimePointer(previous.AllSkippedSince)
	} else {
		status.AllSkippedSince = &now
	}
	return status
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
