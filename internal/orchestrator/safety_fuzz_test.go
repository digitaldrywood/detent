package orchestrator

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

type releaseErrorSafetyScheduler struct {
	scheduler.GlobalScheduler
	err error
}

func (s *releaseErrorSafetyScheduler) ReleaseSlot(slot scheduler.Slot) error {
	if err := s.GlobalScheduler.ReleaseSlot(slot); err != nil {
		return err
	}
	return s.err
}

func FuzzSafetyCriticalOrchestratorBoundaries(f *testing.F) {
	f.Add(0, 0, 0, "", "clean", int64(1), " head ", "lint,test", int64(1), "head", "test,lint", int64(60), int64(0), true, int64(0), int64(1), int64(2), true, uint8(0))
	f.Add(1, 2, 3, "fingerprint", "dirty", int64(1), "head-a", "test", int64(2), "head-b", "lint", int64(-60), int64(0), true, int64(10), int64(-10), int64(0), false, uint8(1))
	f.Add(0, 0, 0, "clean", "dirty", int64(1), "same-head", "fail", int64(1), "same-head", "pass", int64(0), int64(0), false, int64(0), int64(0), int64(0), false, uint8(2))
	f.Add(0, 0, 0, "", "", int64(0), "", "", int64(0), "", "", int64(0), int64(0), false, int64(0), int64(10), int64(0), false, uint8(3))
	f.Add(1, 0, 0, "old-output", "same-deliverable", int64(1), "old-receipt", "recut", int64(1), "new-receipt", "pending_review", int64(0), int64(0), false, int64(0), int64(0), int64(0), false, uint8(0))
	f.Add(2, 0, 0, "new-output", "new-deliverable", int64(1), "same-receipt", "recut", int64(2), "same-receipt", "pending_review", int64(0), int64(0), false, int64(0), int64(0), int64(0), false, uint8(1))

	f.Fuzz(func(
		t *testing.T,
		filesChanged int,
		addedLines int,
		removedLines int,
		fingerprint string,
		status string,
		leftNumber int64,
		leftHead string,
		leftChecks string,
		rightNumber int64,
		rightHead string,
		rightChecks string,
		resetSeconds int64,
		nowSeconds int64,
		hasReset bool,
		createdSeconds int64,
		stageSeconds int64,
		acceptedSeconds int64,
		accepted bool,
		gateScenario uint8,
	) {
		diffStats := DiffStats{
			FilesChanged: filesChanged,
			AddedLines:   addedLines,
			RemovedLines: removedLines,
			Fingerprint:  fingerprint,
			Status:       status,
		}
		wantClean := diffStatsPresent(diffStats) && filesChanged == 0 && addedLines == 0 && removedLines == 0
		if got := implementProgressDiffStatsClean(diffStats); got != wantClean {
			t.Fatalf("implementProgressDiffStatsClean(%#v) = %t, want %t", diffStats, got, wantClean)
		}
		unpushed := diffStats
		unpushed.UnpushedCommits = 1
		unpushed.HeadSHA = leftHead
		gotStranded, gotDeferred := implementProgressUnpushedClassification(unpushed, &connector.PullRequest{HeadSHA: rightHead})
		wantStranded := false
		wantDeferred := ""
		switch {
		case !implementProgressDiffStatsClean(unpushed):
			wantStranded = true
		case strings.TrimSpace(leftHead) == "":
			wantDeferred = workspaceHeadUnavailableReason
		case strings.TrimSpace(rightHead) == "":
			wantDeferred = pullRequestHeadUnavailableReason
		case strings.TrimSpace(leftHead) != strings.TrimSpace(rightHead):
			wantStranded = true
		}
		if gotStranded != wantStranded || gotDeferred != wantDeferred {
			t.Fatalf("implementProgressUnpushedClassification(%#v) = %t, %q, want %t, %q", unpushed, gotStranded, gotDeferred, wantStranded, wantDeferred)
		}

		leftSignature := autoPromoteReworkSignature{PRNumber: leftNumber, HeadSHA: leftHead, FailedChecks: strings.Split(leftChecks, ",")}
		rightSignature := autoPromoteReworkSignature{PRNumber: rightNumber, HeadSHA: rightHead, FailedChecks: strings.Split(rightChecks, ",")}
		gotEqual := implementProgressSignatureEqual(leftSignature, rightSignature)
		wantEqual := leftNumber == rightNumber &&
			strings.TrimSpace(leftHead) == strings.TrimSpace(rightHead) &&
			slicesEqual(autoPromoteCanonicalChecks(leftSignature.FailedChecks), autoPromoteCanonicalChecks(rightSignature.FailedChecks))
		if gotEqual != wantEqual || gotEqual != implementProgressSignatureEqual(rightSignature, leftSignature) {
			t.Fatalf("implementProgressSignatureEqual(%#v, %#v) = %t, want %t", leftSignature, rightSignature, gotEqual, wantEqual)
		}

		leftFingerprint := &spendProgressPRFingerprint{
			Number:         leftNumber,
			HeadSHA:        strings.TrimSpace(leftHead),
			MergeableState: strings.ToLower(strings.TrimSpace(status)),
			CIStatus:       strings.ToLower(strings.TrimSpace(leftChecks)),
		}
		rightFingerprint := &spendProgressPRFingerprint{
			Number:         rightNumber,
			HeadSHA:        strings.TrimSpace(rightHead),
			MergeableState: strings.ToLower(strings.TrimSpace(fingerprint)),
			CIStatus:       strings.ToLower(strings.TrimSpace(rightChecks)),
		}
		wantAdvance := ""
		switch {
		case leftNumber != rightNumber:
			wantAdvance = "pull_request_created"
		case leftFingerprint.HeadSHA != "" && rightFingerprint.HeadSHA != "" && leftFingerprint.HeadSHA != rightFingerprint.HeadSHA:
			wantAdvance = "pull_request_head_changed"
		case leftFingerprint.MergeableState == "dirty" && rightFingerprint.MergeableState == "clean":
			wantAdvance = "pull_request_mergeable"
		case spendProgressCIFailing(leftFingerprint.CIStatus) && spendProgressCIPassing(rightFingerprint.CIStatus):
			wantAdvance = "pull_request_ci_passing"
		}
		if got := spendProgressPRAdvance(leftFingerprint, rightFingerprint); got != wantAdvance {
			t.Fatalf("spendProgressPRAdvance(%#v, %#v) = %q, want %q", leftFingerprint, rightFingerprint, got, wantAdvance)
		}
		leftArtifact := &spendProgressArtifactFingerprint{
			ReceiptHash:            strings.TrimSpace(leftHead),
			Status:                 strings.TrimSpace(leftChecks),
			DeliverableFingerprint: strings.TrimSpace(status),
			OutputFiles:            1,
			OutputFingerprint:      strings.TrimSpace(fingerprint),
		}
		rightArtifact := &spendProgressArtifactFingerprint{
			ReceiptHash:            strings.TrimSpace(rightHead),
			Status:                 strings.TrimSpace(rightChecks),
			DeliverableFingerprint: strings.TrimSpace(fingerprint),
			OutputFingerprint:      strings.TrimSpace(status),
		}
		if rightNumber > 0 {
			rightArtifact.OutputFiles = 1
		}
		wantArtifactAdvance := ""
		switch {
		case rightArtifact.ReceiptHash != "" && rightArtifact.ReceiptHash != leftArtifact.ReceiptHash:
			wantArtifactAdvance = "artifact_receipt_changed"
		case rightArtifact.Status != "" && rightArtifact.Status != leftArtifact.Status:
			wantArtifactAdvance = "artifact_status_changed"
		case rightArtifact.DeliverableFingerprint != "" && rightArtifact.DeliverableFingerprint != leftArtifact.DeliverableFingerprint:
			wantArtifactAdvance = "artifact_deliverable_changed"
		case rightArtifact.OutputFiles > 0 && rightArtifact.OutputFingerprint != "" && rightArtifact.OutputFingerprint != leftArtifact.OutputFingerprint:
			wantArtifactAdvance = "artifact_output_changed"
		}
		if got := spendProgressArtifactAdvance(leftArtifact, rightArtifact); got != wantArtifactAdvance {
			t.Fatalf("spendProgressArtifactAdvance(%#v, %#v) = %q, want %q", leftArtifact, rightArtifact, got, wantArtifactAdvance)
		}
		wantTokenLimitReached := rightNumber > 0 && leftNumber >= rightNumber
		if got := spendProgressTokenLimitReached(leftNumber, rightNumber); got != wantTokenLimitReached {
			t.Fatalf("spendProgressTokenLimitReached(%d, %d) = %t, want %t", leftNumber, rightNumber, got, wantTokenLimitReached)
		}

		resetSeconds %= 1_000_000_000
		nowSeconds %= 1_000_000_000
		now := time.Unix(nowSeconds, 0).UTC()
		resetAt := time.Time{}
		wantResumeAt := now.Add(backendCapacityProbeDelay)
		if hasReset {
			resetAt = time.Unix(resetSeconds, 0).UTC()
			wantResumeAt = resetAt.Add(backendCapacityResetJitter)
			minimum := now.Add(backendCapacityResetJitter)
			if wantResumeAt.Before(minimum) {
				wantResumeAt = minimum
			}
		}
		if got := backendCapacityResumeAt(resetAt, now); !got.Equal(wantResumeAt) {
			t.Fatalf("backendCapacityResumeAt(%s, %s) = %s, want %s", resetAt, now, got, wantResumeAt)
		}
		probeAt := now.Add(backendCapacityProbeDelayForAttempt(filesChanged % 100))
		wantProbeAt := probeAt
		if wantResumeAt.After(now) && wantResumeAt.Before(wantProbeAt) {
			wantProbeAt = wantResumeAt
		}
		if got := backendCapacityBoundedProbeAt(wantResumeAt, probeAt, now); !got.Equal(wantProbeAt) {
			t.Fatalf("backendCapacityBoundedProbeAt(%s, %s, %s) = %s, want %s", wantResumeAt, probeAt, now, got, wantProbeAt)
		}

		createdSeconds %= 1_000_000_000
		stageSeconds %= 1_000_000_000
		acceptedSeconds %= 1_000_000_000
		createdAt := time.Unix(createdSeconds, 0).UTC()
		stageUpdatedAt := time.Unix(stageSeconds, 0).UTC()
		acceptedAt := time.Unix(acceptedSeconds, 0).UTC()
		issue := connector.Issue{CreatedAt: &createdAt, StageUpdatedAt: &stageUpdatedAt}
		attempt := store.WorkAttempt{CompletedAt: acceptedAt}
		if accepted {
			attempt.WorkerMetadataJSON = marshalWorkAttemptJSON(map[string]any{
				spendProgressMetadataKey: spendProgressRecord{AcceptedStateChange: true},
			})
		}
		wantBaseline := createdAt
		if accepted && acceptedAt.After(wantBaseline) {
			wantBaseline = acceptedAt
		}
		if got := spendProgressBaseline(issue, []store.WorkAttempt{attempt}); !got.Equal(wantBaseline) {
			t.Fatalf("spendProgressBaseline() = %s, want %s", got, wantBaseline)
		}

		leftPriority := int(leftNumber % 10)
		rightPriority := int(rightNumber % 10)
		leftIssue := rankingIssueWithLabels("left", "Todo", leftPriority, createdAt, leftHead)
		rightIssue := rankingIssueWithLabels("right", "Todo", rightPriority, stageUpdatedAt, rightHead)
		forward := []connector.Issue{leftIssue, rightIssue}
		reverse := []connector.Issue{rightIssue, leftIssue}
		sortIssuesForDispatch(forward, []string{"Todo"}, []string{"bug", "enhancement"}, true)
		sortIssuesForDispatch(reverse, []string{"Todo"}, []string{"bug", "enhancement"}, true)
		if !equalStrings(rankingIssueIDs(forward), rankingIssueIDs(reverse)) {
			t.Fatalf("ranking depends on input order: forward=%#v reverse=%#v", rankingIssueIDs(forward), rankingIssueIDs(reverse))
		}

		assertDemandDrivenPriorityReservation(t, gateScenario%4, now)
	})
}

func assertDemandDrivenPriorityReservation(t *testing.T, scenario uint8, now time.Time) {
	t.Helper()

	higher := scheduler.ProjectCandidate{ID: "detent", Weight: 1, Priority: 0}
	lower := scheduler.ProjectCandidate{ID: "gopher-ai", Weight: 1, Priority: 3}
	gate := scheduler.NewGlobalDispatchGate(
		scheduler.NewStrictPriority(scheduler.Config{Capacity: 1}),
		higher,
		lower,
	)

	switch scenario {
	case 0:
		gate.BeginProjectCycle(higher)
		gate.EndProjectCycle(higher.ID)
		candidate := scheduler.ProjectCandidate{ID: higher.ID + "/admission", Weight: 1, Priority: higher.Priority}
		slot := requireSafetyGateGranted(t, gate, candidate, now)
		if err := gate.Release(slot); err != nil {
			t.Fatalf("release dynamic candidate: %v", err)
		}
		gate.MarkIdle(candidate)
		requireSafetyGateGrantedAndReleased(t, gate, lower, now.Add(time.Second))
	case 1:
		gate.BeginProjectCycle(higher)
		gate.EndProjectCycle(higher.ID)
		requireSafetyGateGrantedAndReleased(t, gate, lower, now)
	case 2:
		gate.BeginProjectCycle(higher)
		requireSafetyGateReserved(t, gate, lower, now)
		gate.EndProjectCycle(higher.ID)
		requireSafetyGateGrantedAndReleased(t, gate, lower, now.Add(time.Second))

		gate.MarkReady(higher)
		requireSafetyGateReserved(t, gate, lower, now.Add(2*time.Second))
		gate.MarkIdle(higher)
		requireSafetyGateGrantedAndReleased(t, gate, lower, now.Add(3*time.Second))

		gate.BeginProjectCycle(higher)
		slot := requireSafetyGateGranted(t, gate, higher, now.Add(4*time.Second))
		gate.EndProjectCycle(higher.ID)
		if err := gate.Release(slot); err != nil {
			t.Fatalf("release higher-priority candidate: %v", err)
		}
		requireSafetyGateReserved(t, gate, lower, now.Add(5*time.Second))
	case 3:
		releaseErr := errors.New("release failed")
		global := &releaseErrorSafetyScheduler{
			GlobalScheduler: scheduler.NewStrictPriority(scheduler.Config{Capacity: 1}),
			err:             releaseErr,
		}
		gate = scheduler.NewGlobalDispatchGate(global, higher, lower)
		gate.BeginProjectCycle(higher)
		gate.EndProjectCycle(higher.ID)
		candidate := scheduler.ProjectCandidate{ID: higher.ID + "/admission", Weight: 1, Priority: higher.Priority}
		slot := requireSafetyGateGranted(t, gate, candidate, now)
		if err := gate.Release(slot); !errors.Is(err, releaseErr) {
			t.Fatalf("release dynamic candidate: %v, want %v", err, releaseErr)
		}
		requireSafetyGateReserved(t, gate, lower, now.Add(time.Second))
	}
}

func requireSafetyGateGrantedAndReleased(
	t *testing.T,
	gate *scheduler.GlobalDispatchGate,
	project scheduler.ProjectCandidate,
	now time.Time,
) {
	t.Helper()
	slot := requireSafetyGateGranted(t, gate, project, now)
	if err := gate.Release(slot); err != nil {
		t.Fatalf("release %s: %v", project.ID, err)
	}
	gate.MarkIdle(project)
}

func requireSafetyGateGranted(
	t *testing.T,
	gate *scheduler.GlobalDispatchGate,
	project scheduler.ProjectCandidate,
	now time.Time,
) scheduler.Slot {
	t.Helper()
	slot, granted, decision, err := gate.TryAcquireWithDecision(
		t.Context(),
		project,
		scheduler.SlotRequest{State: "Todo"},
		now,
	)
	if err != nil {
		t.Fatalf("TryAcquireWithDecision(%s): %v", project.ID, err)
	}
	if !granted || decision.Reason != scheduler.DispatchGateReasonGranted {
		t.Fatalf("TryAcquireWithDecision(%s) decision = %#v, want granted", project.ID, decision)
	}
	return slot
}

func requireSafetyGateReserved(
	t *testing.T,
	gate *scheduler.GlobalDispatchGate,
	project scheduler.ProjectCandidate,
	now time.Time,
) {
	t.Helper()
	_, granted, decision, err := gate.TryAcquireWithDecision(
		t.Context(),
		project,
		scheduler.SlotRequest{State: "Todo"},
		now,
	)
	if err != nil {
		t.Fatalf("TryAcquireWithDecision(%s): %v", project.ID, err)
	}
	if granted || decision.Reason != scheduler.DispatchGateReasonReservedForHigherPriorityProject {
		t.Fatalf("TryAcquireWithDecision(%s) decision = %#v, want priority reservation", project.ID, decision)
	}
}
