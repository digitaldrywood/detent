package orchestrator

import (
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
)

func FuzzSafetyCriticalOrchestratorBoundaries(f *testing.F) {
	f.Add(0, 0, 0, "", "clean", int64(1), " head ", "lint,test", int64(1), "head", "test,lint", int64(60), int64(0), true, int64(0), int64(1), int64(2), true)
	f.Add(1, 2, 3, "fingerprint", "dirty", int64(1), "head-a", "test", int64(2), "head-b", "lint", int64(-60), int64(0), true, int64(10), int64(-10), int64(0), false)
	f.Add(0, 0, 0, "clean", "dirty", int64(1), "same-head", "failure", int64(1), "same-head", "success", int64(0), int64(0), false, int64(0), int64(0), int64(0), false)
	f.Add(0, 0, 0, "", "", int64(0), "", "", int64(0), "", "", int64(0), int64(0), false, int64(0), int64(0), int64(0), false)

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
		if stageUpdatedAt.After(wantBaseline) {
			wantBaseline = stageUpdatedAt
		}
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
	})
}
