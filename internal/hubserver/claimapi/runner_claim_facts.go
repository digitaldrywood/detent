package claimapi

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func NativeAttemptClaimDisposition(data tracker.NativeRunData, checkpoint *tracker.NativeCheckpoint, project tracker.ProjectID) string {
	if data.Disposition == nil {
		if signal, reported := workpad.SignalFromComment(data.CompletionBody, "", string(project)); reported && signal != nil && signal.Invalid == nil && (signal.HumanAction != "" || signal.ReasonCode == "permission_wait") {
			data.Disposition = &tracker.NativeDisposition{Status: signal.Status, HumanAction: signal.HumanAction != "", ReasonCode: signal.ReasonCode}
		}
	}
	if data.Disposition != nil && !RecordedHumanHold(data.TerminalFailure, data.Disposition, data.CompletionBody) {
		dependencies, valid := RecordedRecoveryDependencies(data.Disposition, string(project))
		if RecordedMachineFailure(data.TerminalFailure, data.Disposition) || valid && len(dependencies) > 0 || data.Disposition.ReasonCode == "stale_base" {
			return "progress"
		}
	}
	if kind := AnsweredClaimKind(data, checkpoint); kind != "" {
		return kind
	}
	if data.TerminalFailure != nil {
		return "progress"
	}
	if data.Disposition != nil && (data.Disposition.Status != workpad.StatusComplete || data.Disposition.Blockers || data.Disposition.ReasonCode != "") {
		return "progress"
	}
	if checkpoint != nil && (checkpoint.ExternalEffect == "git_push" || checkpoint.ExternalEffect == "pr_create") && (data.Disposition == nil || checkpoint.EffectState == "pending" || checkpoint.EffectState == "ambiguous") {
		return "progress"
	}
	return ""
}

func AttemptClaimFacts(attempt leasestore.Attempt, disposition string, unresolvedOnly bool) leasestore.AttemptClaimFacts {
	return leasestore.AttemptClaimFacts{AttemptID: attempt.ID, Sequence: attempt.Sequence, Status: attempt.Status, PayloadObjectKey: attempt.PayloadObjectKey, CheckpointObjectKey: attempt.CheckpointObjectKey, Disposition: disposition, UnresolvedOnly: unresolvedOnly}
}

func (d AttemptFactsDeps) PrepareAttemptClaimFacts(ctx context.Context, snapshot hubrecords.TenantAttemptSnapshot) (leasestore.AttemptClaimFacts, error) {
	run, err := d.ReadAttempt(ctx, snapshot)
	if err != nil {
		return leasestore.AttemptClaimFacts{}, err
	}
	if err := hubrecords.NativeAttemptRunMismatch(snapshot, run); err != nil {
		return leasestore.AttemptClaimFacts{}, fmt.Errorf("recover attempt claim facts: %w", err)
	}
	status, statusErr := d.AttemptStatus(tracker.NativeRunEvent{Type: snapshot.Event.Type, Data: run})
	if statusErr != nil || status != snapshot.Attempt.Status {
		return leasestore.AttemptClaimFacts{}, fmt.Errorf("recover attempt claim status: %w", artifact.ErrIntegrity)
	}
	checkpoint, err := d.ReadCheckpoint(ctx, snapshot)
	if err != nil {
		return leasestore.AttemptClaimFacts{}, err
	}
	return AttemptClaimFacts(snapshot.Attempt, NativeAttemptClaimDisposition(run, checkpoint, snapshot.Event.ProjectID), true), nil
}

func RecordedHumanHold(failure *tracker.NativeTerminalFailure, disposition *tracker.NativeDisposition, texts ...string) bool {
	if disposition != nil && (disposition.ReasonCode == "permission_wait" || slices.ContainsFunc(disposition.BlockerEvidence, func(blocker workpad.Blocker) bool {
		return blocker.Owner == workpad.BlockerOwnerHuman
	})) {
		return true
	}
	for _, text := range texts {
		signal := recordedWorkpadQuestionSignal(text)
		if signal == nil {
			continue
		}
		if explicitRecordedHumanHold(signal) ||
			slices.ContainsFunc(signal.Blockers, func(blocker workpad.Blocker) bool { return !recordedInstanceBlocker(blocker) }) && !RecordedMachineFailure(failure, nil) {
			return true
		}
	}
	return false
}

func recordedWorkpadQuestionSignal(text string) *workpad.Signal {
	block, found := workpad.LastStatusBlock(text)
	if !found {
		return nil
	}
	signal, err := workpad.ParseStatusBlock(block, "")
	if err != nil || signal.Invalid != nil || signal.Status != workpad.StatusBlocked || strings.TrimSpace(signal.HumanAction) == "" {
		return nil
	}
	return signal
}

func explicitRecordedHumanHold(signal *workpad.Signal) bool {
	return signal.ExplicitHumanBlocker || signal.ReasonCode == "permission_wait" || signal.Fields["human_action_source"] == "issue_contract"
}

func RecordedMachineFailure(failure *tracker.NativeTerminalFailure, disposition *tracker.NativeDisposition) bool {
	if failure != nil && (failure.InfrastructureDecision() != nil || failure.Decision != nil && failure.Decision.Valid() && slices.Contains([]string{"instance", "issue"}, failure.Decision.Outcome)) {
		return true
	}
	if disposition == nil || disposition.Status != workpad.StatusBlocked {
		return false
	}
	if disposition.ReasonCode == "instance_limitation" {
		return true
	}
	return slices.ContainsFunc(disposition.BlockerEvidence, recordedInstanceBlocker)
}

func recordedInstanceBlocker(blocker workpad.Blocker) bool {
	return blocker.Owner == workpad.BlockerOwnerInstance && (!blocker.Unverifiable || blocker.Predicate == nil && workpad.IsSymbolicRef(blocker.Ref))
}

func RecordedRecoveryDependencies(disposition *tracker.NativeDisposition, project string) (map[int64]bool, bool) {
	if disposition == nil || disposition.Status != workpad.StatusBlocked || len(disposition.BlockerEvidence) == 0 {
		return nil, true
	}
	dependencies := make(map[int64]bool, len(disposition.BlockerEvidence))
	for _, blocker := range disposition.BlockerEvidence {
		if recordedInstanceBlocker(blocker) {
			continue
		}
		predicate := blocker.Predicate
		if blocker.Owner != workpad.BlockerOwnerOrchestrator || blocker.Unverifiable || predicate == nil {
			return nil, false
		}
		merged := predicate.Type == workpad.PredicatePullRequestState
		if !merged && (predicate.Type != workpad.PredicateIssueState || slices.ContainsFunc(predicate.States, func(state string) bool {
			return !strings.EqualFold(state, "open")
		})) {
			return nil, false
		}
		if merged && (len(predicate.States) == 0 || slices.ContainsFunc(predicate.States, func(state string) bool {
			switch strings.ToLower(state) {
			case "open", "closed", "missing":
				return false
			default:
				return true
			}
		})) {
			return nil, false
		}
		reference := predicate.Identifier
		if reference == "" {
			reference = blocker.Ref
		}
		ref, err := workpad.ParseRef(reference, project)
		if err != nil || !strings.HasPrefix(ref, project+"#") {
			return nil, false
		}
		number, err := strconv.ParseInt(strings.TrimPrefix(ref, project+"#"), 10, 64)
		if err != nil || number <= 0 {
			return nil, false
		}
		dependencies[number] = merged || dependencies[number]
	}
	return dependencies, len(dependencies) <= corestore.MaxPageSize
}

func RecordedWorkpadQuestion(text string) bool {
	signal := recordedWorkpadQuestionSignal(text)
	return signal != nil && explicitRecordedHumanHold(signal)
}
