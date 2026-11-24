package phaserecovery

import (
	"encoding/json"
	"strings"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/failureattribution"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/runner/prompt"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func ReviewAttempt(recovery tracker.NativeRecovery, command string, landingNeedsWork func() bool, completed func(string) bool) *tracker.NativeAttempt {
	detail := recovery.ChangeDetail
	repairResume := detail != nil && tracker.RepairDependenciesReady(recovery.Issue, changerequest.IndependentRepairDependencies(*detail))
	if detail == nil || recovery.Change == nil || detail.Change.ID != recovery.Change.ChangeID || detail.Change.CurrentVersion != recovery.Change.VersionID || detail.Change.CurrentLanding() != nil || detail.Summary.Status == "stale_policy" || detail.Summary.NativeReview == "changes_requested" && !repairResume || landingNeedsWork() {
		return nil
	}
	var current *tracker.ChangeVersion
	for i := range detail.Versions {
		if detail.Versions[i].ID == detail.Change.CurrentVersion {
			current = &detail.Versions[i]
			break
		}
	}
	if current == nil || current.HeadSHA != recovery.Change.HeadSHA || current.Policy.ID != current.PolicyID || current.Policy.Gates.AutomatedReview != gate.AutomatedReviewRequired && !repairResume || current.Source == nil || current.RunID == "" || current.AttemptID == "" {
		return nil
	}
	if command != "" && (current.Validation == nil || current.Validation.Command != command || current.Validation.ExitCode != 0 || current.Validation.HeadSHA != current.HeadSHA || current.Validation.TreeSHA == "") {
		return nil
	}
	var sourceReview, acceptanceReview *gate.ValidatorResult
	newerHumanFeedback := prompt.NativeReviewDiscussionForVersion(recovery.Discussion, detail.Discussion, current.ID).HumanFeedbackSince(current.CreatedAt)
	sourceReviewIndex, acceptanceReviewIndex := -1, -1
	for i := len(detail.Reviews) - 1; i >= 0; i-- {
		review := detail.Reviews[i]
		if review.VersionID != current.ID {
			continue
		}
		if acceptanceReview == nil && review.Validator != nil {
			acceptanceReview, acceptanceReviewIndex = review.Validator, i
		}
		if sourceReview == nil && review.ImplementationReview != nil {
			sourceReview, sourceReviewIndex = review.ImplementationReview, i
			newerHumanFeedback = prompt.NativeReviewDiscussionForVersion(recovery.Discussion, detail.Discussion, current.ID).HumanFeedbackSince(review.CreatedAt)
		}
		if sourceReview != nil && acceptanceReview != nil {
			break
		}
	}
	if repairResume && newerHumanFeedback {
		return nil
	}
	if !repairResume && (sourceReview != nil && sourceReview.Verdict != gate.ValidatorVerdictPass && sourceReview.Verdict != gate.ValidatorVerdictWait || acceptanceReview != nil && acceptanceReview.Verdict != gate.ValidatorVerdictPass && acceptanceReview.Verdict != gate.ValidatorVerdictWait) {
		return nil
	}
	if !repairResume && sourceReview != nil && sourceReview.Verdict == gate.ValidatorVerdictPass && acceptanceReview != nil && acceptanceReview.Verdict == gate.ValidatorVerdictPass && acceptanceReviewIndex > sourceReviewIndex && !newerHumanFeedback {
		return nil
	}
	var source *tracker.NativeAttempt
	finalizationObserved, finalizationPending := false, false
	for i := len(recovery.Attempts) - 1; i >= 0; i-- {
		attempt := &recovery.Attempts[i]
		if finalization := attempt.Finalization; !finalizationObserved && finalization != nil && attempt.PolicyID == current.PolicyID && finalization.ChangeID == current.ChangeID && finalization.VersionID == current.ID && finalization.HeadSHA == current.HeadSHA {
			finalizationObserved = true
			finalizationPending = finalization.Changed && !finalization.Reviewed && finalization.Error == "" && finalization.VersionError == "" && (attempt.Status == "interrupted" || attempt.Status == "failed" || attempt.Status == "cancelled")
		}
		if attempt.Checkpoint != nil && (attempt.Checkpoint.UncertainForgeEffect() || attempt.Checkpoint.HeadSHA != "" && attempt.Checkpoint.HeadSHA != current.HeadSHA || attempt.Checkpoint.WorktreeState == "dirty") {
			return nil
		}
		disposition := attempt.Disposition
		if disposition != nil && (disposition.Status != workpad.StatusComplete || disposition.Blockers || disposition.HumanAction || disposition.ReasonCode != "") {
			return nil
		}
		if attempt.AttemptID == current.AttemptID && attempt.RunID == current.RunID {
			source = attempt
			break
		}
	}
	if source == nil || source.PolicyID != current.PolicyID || source.Disposition == nil || source.CompletionBody == "" || source.Status != "succeeded" && source.Status != "failed" && source.Status != "interrupted" && source.Status != "cancelled" {
		return nil
	}
	if !completed(source.CompletionBody) {
		return nil
	}
	if !repairResume && !current.Policy.Gates.Validator && sourceReview != nil && sourceReview.Verdict == gate.ValidatorVerdictPass && !finalizationPending && !newerHumanFeedback {
		return nil
	}
	return source
}

func ReviewMatches(recovery tracker.NativeRecovery, local *workspace.RecoveryState, current *workspace.ChangeSource) bool {
	if current == nil || local == nil || recovery.Change == nil || recovery.ChangeDetail == nil || local.HeadSHA != current.Version.HeadSHA || len(local.TrackedPaths) != 0 || len(local.UntrackedPaths) != 0 {
		return false
	}
	version := current.Version
	if *recovery.Change != (tracker.NativeChangeReference{ChangeID: version.ChangeID, VersionID: version.ID, HeadSHA: version.HeadSHA}) || version.Policy.ID != version.PolicyID {
		return false
	}
	for _, published := range recovery.ChangeDetail.Versions {
		if published.ID == version.ID {
			if published.HeadSHA != version.HeadSHA || published.BaseSHA != version.BaseSHA || published.Repository != version.Repository || published.AttemptID != version.AttemptID || published.RunID != version.RunID || published.PolicyID != version.PolicyID {
				return false
			}
			if phase := current.PushedPhase; phase != nil {
				return phase.HeadSHA == version.HeadSHA && phase.TreeSHA == local.DiffStat.TreeSHA && phase.PolicyID == version.PolicyID && local.DiffStat.IsEmpty()
			}
			return version.Source != nil && version.Source.Validate(version.BaseSHA, version.HeadSHA, current.Bundle) == nil
		}
	}
	return false
}

func TurnAttempt(recovery tracker.NativeRecovery, repairResume, landingNeedsWork bool, completed func(string) bool) *tracker.NativeAttempt {
	if landingNeedsWork {
		return nil
	}
	if detail := recovery.ChangeDetail; detail != nil {
		if detail.Change.CurrentLanding() != nil || detail.Summary.Status == "stale_policy" || detail.Summary.NativeReview == "changes_requested" {
			return nil
		}
		for _, review := range detail.Reviews {
			if review.VersionID == detail.Change.CurrentVersion && (review.Decision == "changes_requested" || review.SecurityAudit != nil && review.SecurityAudit.Verdict != "pass") {
				return nil
			}
		}
	}
	state := tracker.NativeStepReviewState(recovery)
	for i := len(recovery.Attempts) - 1; i >= 0; i-- {
		attempt := &recovery.Attempts[i]
		if attempt.Status == "succeeded" && !repairResume {
			return nil
		}
		if attempt.Runtime != nil && attempt.Runtime.Validation != nil && attempt.Runtime.Validation.ExitCode != 0 && !repairResume {
			return nil
		}
		for _, receipt := range attempt.Steps {
			if receipt.Step != "agent_turn" {
				continue
			}
			if receipt.PolicyID != recovery.Lease.PolicyID || receipt.ReviewState != state || receipt.ReviewEpoch != tracker.NativeStepReviewEpoch(recovery) && !repairResume {
				return nil
			}
			if !completed(receipt.CompletionBody) {
				return nil
			}
			copy := *attempt
			copy.CompletionBody = receipt.CompletionBody
			copy.Checkpoint = &receipt.Source
			return &copy
		}
	}
	return nil
}

func PhaseTurnAttempt(recovery tracker.NativeRecovery, phase tracker.NativePhaseReceipt, completed func(string) bool) *tracker.NativeAttempt {
	for i := len(recovery.Attempts) - 1; i >= 0; i-- {
		attempt := recovery.Attempts[i]
		if attempt.AttemptID != phase.AttemptID {
			continue
		}
		for _, receipt := range attempt.Steps {
			if receipt.Step != "agent_turn" || receipt.PolicyID != recovery.Lease.PolicyID || receipt.Source.HeadSHA != phase.HeadSHA ||
				receipt.ReviewState != tracker.NativeStepReviewState(recovery) || receipt.ReviewEpoch != tracker.NativeStepReviewEpoch(recovery) {
				continue
			}
			if !completed(receipt.CompletionBody) {
				return nil
			}
			attempt.CompletionBody = receipt.CompletionBody
			attempt.Checkpoint = &receipt.Source
			return &attempt
		}
	}
	return nil
}

func TurnMatches(attempt *tracker.NativeAttempt, local *workspace.RecoveryState) bool {
	return attempt != nil && attempt.Checkpoint != nil && local != nil && attempt.Checkpoint.HeadSHA != "" &&
		attempt.Checkpoint.HeadSHA == local.HeadSHA && attempt.Checkpoint.WorkspaceDigest != "" && attempt.Checkpoint.WorkspaceDigest == local.WorkspaceFingerprint
}

func Checkpoint(recovery tracker.NativeRecovery, current *workspace.ChangeSource) *workspace.CheckpointRef {
	if recovery.SourceVersionID != "" {
		return nil
	}
	previous := recovery.SourceAttempt()
	if previous == nil || !previous.Checkpoint.GitRef() || previous.Checkpoint.UncertainForgeEffect() {
		return nil
	}
	if current != nil && current.Version.AttemptID != "" && current.Version.AttemptID != previous.AttemptID {
		source, version := -1, -1
		for i, attempt := range recovery.Attempts {
			switch attempt.AttemptID {
			case previous.AttemptID:
				source = i
			case current.Version.AttemptID:
				version = i
			}
		}
		if version > source {
			return nil
		}
	}
	checkpoint := previous.Checkpoint
	return &workspace.CheckpointRef{Ref: checkpoint.Ref, CommitSHA: checkpoint.CommitSHA, BaseSHA: checkpoint.BaseSHA, TreeSHA: checkpoint.TreeSHA}
}

func ReworkFindings(version, head string, audit *tracker.SecurityAuditReview, reviews ...*gate.ValidatorResult) string {
	var findings []string
	for _, result := range reviews {
		if result != nil && result.Verdict == gate.ValidatorVerdictRework && result.VersionID == version && result.HeadSHA == head {
			body, err := json.Marshal(result)
			if err == nil {
				findings = append(findings, string(body))
			}
		}
	}
	if audit != nil && audit.Verdict == "rework" && audit.VersionID == version && audit.Run.HeadSHA == head {
		body, err := json.Marshal(audit)
		if err == nil {
			findings = append(findings, string(body))
		}
	}
	return strings.Join(findings, "\n\n")
}

func LandingNeedsRework(recovery tracker.NativeRecovery) bool {
	landing := Landing(recovery)
	return landing != nil && !landing.Landed && (landing.RefusalKind == workspace.LandRefusalConflict ||
		landing.RefusalKind == workspace.LandRefusalBaseMoved && landing.BaseSHA != "" ||
		landing.GateFailed && landing.Gate != nil && landing.Gate.ExitCode != 0 && landing.Gate.Attribution == failureattribution.Independent)
}

func NativeReviewConversationRefusal(recovery tracker.NativeRecovery) string {
	landing := Landing(recovery)
	if recovery.Change == nil || landing == nil || landing.Landed || landing.RefusalKind != workspace.LandRefusalProtected {
		return ""
	}
	for i := len(recovery.History) - 1; i >= 0; i-- {
		event := recovery.History[i]
		if event.Type != "workflow.transitioned" || !strings.EqualFold(strings.TrimSpace(event.Data.FromState), "Merging") {
			continue
		}
		if workspace.ReviewConversationsRefusal(event.Data.ReasonDetail) {
			return strings.TrimSpace(event.Data.ReasonDetail)
		}
		return ""
	}
	return ""
}

func Landing(recovery tracker.NativeRecovery) *tracker.NativeLandingReceipt {
	for i := len(recovery.Attempts) - 1; i >= 0; i-- {
		runtime := recovery.Attempts[i].Runtime
		if runtime == nil || runtime.Landing == nil {
			continue
		}
		landing := runtime.Landing
		if recovery.Change != nil && (landing.ChangeID != recovery.Change.ChangeID || landing.VersionID != recovery.Change.VersionID || landing.HeadSHA != recovery.Change.HeadSHA) {
			return nil
		}
		return landing
	}
	return nil
}

func CompletionSignalFromMessage(message string) (*workpad.Signal, bool) {
	signal, reported := workpad.SignalFromComment(message, "", "")
	if reported && signal != nil && signal.Invalid == nil && hostPublicationOnly(signal) {
		signal.Status, signal.ReasonCode, signal.Blockers = workpad.StatusComplete, "", nil
	}
	return signal, reported
}

func DispositionFromMessage(message string) *tracker.NativeDisposition {
	signal, reported := CompletionSignalFromMessage(message)
	if !reported || signal == nil || signal.Invalid != nil {
		return nil
	}
	return &tracker.NativeDisposition{Status: signal.Status, Blockers: len(signal.Blockers) != 0, HumanAction: signal.HumanAction != "", ReasonCode: signal.ReasonCode, FinalSummary: workpad.FinalSummary(message), BlockerEvidence: signal.Blockers}
}

func hostPublicationOnly(signal *workpad.Signal) bool {
	if signal.Status != workpad.StatusBlocked || signal.HumanAction != "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(signal.ReasonCode)) {
	case "publication_blocked", "awaiting_host_publish":
		return len(signal.Blockers) == 0
	case "host_publication_pending":
		if len(signal.Blockers) == 0 {
			return true
		}
		if len(signal.Blockers) == 1 {
			blocker := signal.Blockers[0]
			return blocker.Ref == "instance:tool" && blocker.Owner == workpad.BlockerOwnerInstance && blocker.Predicate == nil
		}
	}
	return false
}
