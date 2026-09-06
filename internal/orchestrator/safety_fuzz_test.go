package orchestrator

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type releaseErrorSafetyScheduler struct {
	scheduler.GlobalScheduler
	err error
}

// ReleaseSlot fails BEFORE delegating, so a failed release leaves the underlying
// semaphore genuinely occupied. Releasing first and then returning an error made
// the slot actually free, which meant the old priority reservation -- not real
// capacity accounting -- was the only thing refusing the next request.
func (s *releaseErrorSafetyScheduler) ReleaseSlot(slot scheduler.Slot) error {
	if s.err != nil {
		err := s.err
		s.err = nil
		return err
	}
	return s.GlobalScheduler.ReleaseSlot(slot)
}

func FuzzSafetyCriticalOrchestratorBoundaries(f *testing.F) {
	f.Add(0, 0, 0, "", "blocked", int64(1), "same-head", "p2", int64(1), "same-head", "completed_rework_gate_wait", int64(0), int64(0), false, int64(0), int64(0), int64(0), false, true, uint8(17))
	f.Add(0, 0, 0, "", "```detent-human\nschema: 1\n```", int64(1), "head", "Depends on: owner/repo#1 #2", int64(1), "head", "", int64(0), int64(0), false, int64(0), int64(0), int64(0), false, false, uint8(0))
	f.Add(0, 0, 0, "", "```detent-human\nschema: invalid\n", int64(1), "head", "Depends on: #1bad https://github.com/owner/repo/issues/0", int64(1), "head", "", int64(0), int64(0), false, int64(0), int64(0), int64(0), false, false, uint8(0))
	f.Add(0, 0, 0, "", "clean", int64(1), " head ", "lint,test", int64(1), "head", "test,lint", int64(60), int64(0), true, int64(0), int64(1), int64(2), true, false, uint8(0))
	f.Add(1, 2, 3, "fingerprint", "dirty", int64(1), "head-a", "test", int64(2), "head-b", "lint", int64(-60), int64(0), true, int64(10), int64(-10), int64(0), false, false, uint8(1))
	f.Add(0, 0, 0, "clean", "dirty", int64(1), "same-head", "fail", int64(1), "same-head", "pass", int64(0), int64(0), false, int64(0), int64(0), int64(0), false, false, uint8(2))
	f.Add(0, 0, 0, "", "", int64(0), "", "", int64(0), "", "", int64(0), int64(0), false, int64(0), int64(10), int64(0), false, false, uint8(3))
	f.Add(1, 0, 0, "old-output", "same-deliverable", int64(1), "old-receipt", "recut", int64(1), "new-receipt", "pending_review", int64(0), int64(0), false, int64(0), int64(0), int64(0), false, false, uint8(0))
	f.Add(2, 0, 0, "new-output", "new-deliverable", int64(1), "same-receipt", "recut", int64(2), "same-receipt", "pending_review", int64(0), int64(0), false, int64(0), int64(0), int64(0), false, false, uint8(1))
	f.Add(1, 0, 0, "tracked-output", "changed", int64(1), "same-head", "recut", int64(1), "same-head", "pending_review", int64(0), int64(0), false, int64(0), int64(0), int64(0), false, true, uint8(1))
	for _, scenario := range []uint8{4, 5, 6, 7} {
		f.Add(0, 0, 0, "", "clean", int64(0), "", "", int64(0), "", "", int64(0), int64(0), false, int64(0), int64(0), int64(0), false, false, scenario)
	}

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
		tracked bool,
		gateScenario uint8,
	) {
		assertReworkGateWaitSafety(t, gateScenario%4, gateScenario/4%3, gateScenario/12%4, tracked)
		assertHumanDependencyBoundary(t, status, leftChecks)
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
		if tracked {
			unpushed.TrackedPaths = []string{"tracked.go"}
		}
		gotStranded, gotDeferred := implementProgressUnpushedClassification(unpushed, &connector.PullRequest{HeadSHA: rightHead})
		wantStranded := false
		wantDeferred := ""
		switch {
		case len(unpushed.TrackedPaths) > 0:
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
		case rightArtifact.Status != "" && !strings.EqualFold(strings.TrimSpace(rightArtifact.Status), strings.TrimSpace(leftArtifact.Status)):
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

		scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
		controller := backendCapacityTestController{scope: scope, hasStatus: true, status: runpkg.CapacityStatus{Available: true, Exhausted: hasReset}}
		capacityOrchestrator := &Orchestrator{capacityController: controller, capacityStatus: controller}
		capacityState := newState(normalizeConfig(Config{}))
		kind := []string{"http_404", "http_500", "http_503", "http_599"}[gateScenario%4]
		capacityState.BackendOutages[scope.Key()] = BackendOutage{Scope: scope, Kind: kind, ProbeAttempts: int(gateScenario % 8)}
		outage := capacityOrchestrator.registerBackendOutage(&capacityState, &backendcapacity.Error{Scope: scope, Details: backendcapacity.Details{Type: backendcapacity.ErrorTypeProviderOutage, Kind: kind}}, now, true)
		if want := now.Add(backendCapacityProbeDelayForAttempt(outage.ProbeAttempts)); !outage.NextProbeAt.Equal(want) {
			t.Fatalf("provider outage next probe = %s, want %s", outage.NextProbeAt, want)
		}
		capacityOrchestrator.recoverBackendCapacityFromStatus(&capacityState, Running{CapacityScope: scope}, &telemetry.RateLimits{}, now)
		if len(capacityState.BackendOutages) != 1 {
			t.Fatal("quota telemetry cleared a provider HTTP outage")
		}
		if _, _, paused := capacityOrchestrator.backendCapacityDispatch(&capacityState, runpkg.RunRequest{}, now); !paused {
			t.Fatal("provider outage did not pause dispatch")
		}
		if terminalAttemptRetryableFailure(telemetry.WorkAttempt{TerminalState: string(store.WorkAttemptTerminalCapacity), ErrorClass: backendcapacity.ErrorClass}) {
			t.Fatal("provider capacity counted toward retry parking")
		}
		capacityOrchestrator.recoverBackendCapacity(&capacityState, Running{CapacityScope: scope, CapacityProbe: true}, outage.NextProbeAt)
		if _, _, paused := capacityOrchestrator.backendCapacityDispatch(&capacityState, runpkg.RunRequest{}, outage.NextProbeAt); paused {
			t.Fatal("successful capacity probe did not resume dispatch")
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
		// Priority never holds idle capacity. A higher-priority project that is
		// mid-cycle, merely ready, or idle must not refuse a free slot to a
		// lower-priority project; only a slot it actually holds may.
		gate.BeginProjectCycle(higher)
		requireSafetyGateGrantedAndReleased(t, gate, lower, now)
		gate.EndProjectCycle(higher.ID)
		requireSafetyGateGrantedAndReleased(t, gate, lower, now.Add(time.Second))

		gate.MarkReady(higher)
		requireSafetyGateGrantedAndReleased(t, gate, lower, now.Add(2*time.Second))
		gate.MarkIdle(higher)
		requireSafetyGateGrantedAndReleased(t, gate, lower, now.Add(3*time.Second))

		gate.BeginProjectCycle(higher)
		slot := requireSafetyGateGranted(t, gate, higher, now.Add(4*time.Second))
		requireSafetyGateReserved(t, gate, lower, now.Add(5*time.Second))
		gate.EndProjectCycle(higher.ID)
		if err := gate.Release(slot); err != nil {
			t.Fatalf("release higher-priority candidate: %v", err)
		}
		requireSafetyGateGrantedAndReleased(t, gate, lower, now.Add(6*time.Second))
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
