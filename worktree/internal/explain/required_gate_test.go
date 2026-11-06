package explain

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRequiredGateSeparatesCIFromAggregateEvidence(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		required     *telemetry.RequiredGate
		mergeability string
		want         GateState
	}{
		{name: "exact head audit failure", required: &telemetry.RequiredGate{PRNumber: 11, HeadSHA: "head", BaseSHA: "base", State: "failed", Reason: "security_audit_findings", AuditRunID: 13, AuditReason: "unresolved_findings"}, want: GateFailed},
		{name: "cached pass with current conflict", required: &telemetry.RequiredGate{PRNumber: 11, HeadSHA: "head", BaseSHA: "base", State: "passed", Reason: "ready"}, mergeability: "dirty", want: GateFailed},
		{name: "conflict", mergeability: "dirty", want: GateFailed},
		{name: "conflicting", mergeability: "conflicting", want: GateFailed},
		{name: "capability blocker", required: &telemetry.RequiredGate{PRNumber: 11, HeadSHA: "head", BaseSHA: "base", State: "failed", Reason: "workpad_blocker", HumanAction: "Restore worker-github-cli-auth."}, want: GateFailed},
		{name: "missing audit", required: &telemetry.RequiredGate{PRNumber: 11, HeadSHA: "head", BaseSHA: "base", State: "pending", Reason: "security_audit_missing"}, want: GatePending},
		{name: "stale audit failure", required: &telemetry.RequiredGate{PRNumber: 11, HeadSHA: "old", BaseSHA: "base", State: "failed", Reason: "security_audit_findings"}, want: GatePending},
		{name: "durable exact-head audit pass", required: &telemetry.RequiredGate{PRNumber: 11, HeadSHA: "head", BaseSHA: "base", State: "passed", Reason: "ready", AuditRunID: 476, AuditReason: "ready"}, want: GatePassed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 5, 19, 18, 8, 0, time.UTC)
			issue := telemetry.Issue{ID: "issue-1", ProjectID: "detent", PullRequest: &telemetry.PullRequest{Number: 11, HeadSHA: "head", BaseSHA: "base", CIStatus: "success", MergeableState: tt.mergeability}, RequiredGate: tt.required}
			reader := &evidenceReader{observation: liveIssueObservation(now, issue)}
			got, err := newTestService(now, reader).Explain(t.Context(), Query{ProjectID: "detent", IssueID: "issue-1"})
			if err != nil {
				t.Fatal(err)
			}
			if got.RequiredGate.State != tt.want || got.RequiredGate.CIState != "success" {
				t.Fatalf("required gate = %#v, want aggregate %s and CI success", got.RequiredGate, tt.want)
			}
			if tt.required != nil && tt.required.HeadSHA == "head" && (got.RequiredGate.AuditRunID != tt.required.AuditRunID || got.RequiredGate.HumanAction != tt.required.HumanAction) {
				t.Fatalf("missing evidence: %#v", got.RequiredGate)
			}
		})
	}
}

func TestNativeRequiredGateSeparatesValidationFromLanding(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 13, 4, 33, 25, 0, time.UTC)
	finding := "Unresolved review: integration actor overwrites the runner actor"
	for _, test := range []struct {
		name       string
		edit       func(*tracker.NativeRuntimeEvidence)
		want       GateState
		historical bool
		refusal    bool
		local      bool
		pending    bool
		wantReason string
	}{
		{name: "current refusal during claimed repair", want: GateFailed, refusal: true, local: true},
		{name: "base movement retains automatic landing retry without Waiting", want: GatePending, pending: true, local: true, edit: func(e *tracker.NativeRuntimeEvidence) {
			e.CurrentVersion.Landing.RefusalKind = "base_moved"
			e.CurrentVersion.Landing.Refusal = "Base branch moved; waiting to retry landing"
		}},
		{name: "batch member conflict retains automatic landing retry without Waiting", want: GatePending, pending: true, local: true, edit: func(e *tracker.NativeRuntimeEvidence) {
			e.CurrentVersion.Landing.Path = "batch_member"
			e.CurrentVersion.Landing.RefusalKind = "conflict"
			e.CurrentVersion.Landing.Refusal = "Batch member conflicts with the current staging tree"
		}},
		{name: "ordinary conflict still requires repair", want: GateFailed, local: true, wantReason: "Source conflicts with the base branch", edit: func(e *tracker.NativeRuntimeEvidence) {
			e.CurrentVersion.Landing.RefusalKind = "conflict"
			e.CurrentVersion.Landing.Refusal = "Source conflicts with the base branch"
			e.LatestTransition = nil
		}},
		{name: "explicit landing wait", want: GatePending, pending: true, local: true, edit: func(e *tracker.NativeRuntimeEvidence) {
			e.CurrentVersion.Landing.Waiting = true
			e.CurrentVersion.Landing.Refusal = "Waiting for landing validation"
		}},
		{name: "pending CI without refusal retains automatic landing retry", want: GatePending, pending: true, local: true, edit: func(e *tracker.NativeRuntimeEvidence) {
			e.CurrentVersion.Landing.RefusalKind = ""
			e.CurrentVersion.Landing.Refusal = "Waiting for required checks"
			e.CurrentVersion.Landing.CI = &tracker.NativeLandingCIReceipt{HeadSHA: "head", State: "pending"}
		}},
		{name: "pending CI does not hide an external review refusal", want: GateFailed, refusal: true, local: true, edit: func(e *tracker.NativeRuntimeEvidence) {
			e.CurrentVersion.Landing.CI = &tracker.NativeLandingCIReceipt{HeadSHA: "head", State: "pending"}
		}},
		{name: "superseding version", want: GateUnknown, historical: true, edit: func(e *tracker.NativeRuntimeEvidence) {
			e.Attempt.Runtime.Landing = e.CurrentVersion.Landing
			e.Change.Change.CurrentVersion = "version_new"
			e.CurrentVersion = &tracker.NativeChangeVersionEvidence{NativeChangeReference: tracker.NativeChangeReference{ChangeID: "change_1", VersionID: "version_new", HeadSHA: "new_head"}, PublishedAt: at.Add(time.Minute)}
		}},
		{name: "native evidence updated after external observation", want: GateUnknown, local: true, edit: func(e *tracker.NativeRuntimeEvidence) { e.Change.Change.UpdatedAt = at.Add(30 * time.Second) }},
		{name: "unrelated transition does not supply a finding", want: GateFailed, local: true, wantReason: "base_protected", edit: func(e *tracker.NativeRuntimeEvidence) { e.LatestTransition.Data.FromState = "Todo" }},
		{name: "different head", want: GateUnknown, historical: true, local: true, edit: func(e *tracker.NativeRuntimeEvidence) { e.CurrentVersion.Landing.HeadSHA = "old_head" }},
		{name: "different change", want: GateUnknown, historical: true, local: true, edit: func(e *tracker.NativeRuntimeEvidence) { e.CurrentVersion.Landing.ChangeID = "change_old" }},
		{name: "observation predates publication", want: GateUnknown, historical: true, local: true, edit: func(e *tracker.NativeRuntimeEvidence) { e.CurrentVersion.Landing.ObservedAt = at.Add(-time.Hour) }},
		{name: "missing observation time", want: GateUnknown, local: true, edit: func(e *tracker.NativeRuntimeEvidence) { e.CurrentVersion.Landing.ObservedAt = time.Time{} }},
		{name: "reviewed with passed local receipt and no external observation", want: GateUnknown, local: true, edit: func(e *tracker.NativeRuntimeEvidence) {
			e.CurrentVersion.Landing = nil
			e.Attempt.Runtime.Landing = nil
		}},
		{name: "landed current version", want: GatePassed, local: true, edit: func(e *tracker.NativeRuntimeEvidence) {
			e.Change.Summary.Status = "landed"
			e.Change.Change.Landed = &tracker.ChangeLanding{VersionID: "version_1", HeadSHA: "head", LandedAt: at.Add(time.Minute)}
		}},
		{name: "failed native check without external observation", want: GateFailed, local: true, edit: func(e *tracker.NativeRuntimeEvidence) {
			e.CurrentVersion.Landing = nil
			e.Change.Summary.Checks = "failure"
		}},
		{name: "unknown source", want: GateUnavailable, edit: func(e *tracker.NativeRuntimeEvidence) {
			e.Change = nil
			e.CurrentVersion = nil
			e.Attempt.Runtime.Landing = nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			local := &gate.CommandResult{ReceiptID: "receipt_1", Command: "make check-landing", HeadSHA: "head", TreeSHA: "tree", FinishedAt: at.Add(-time.Second)}
			landing := &tracker.NativeLandingReceipt{ChangeID: "change_1", VersionID: "version_1", HeadSHA: "head", RefusalKind: "base_protected", Refusal: "base_protected", FromState: "Merging", TargetState: "Rework", ObservedAt: at, Gate: local}
			e := tracker.NativeRuntimeEvidence{
				ObservedAt: at.Add(time.Minute), Selection: "active",
				Issue:            tracker.NativeIssue{NativeReference: tracker.NativeReference{ProjectID: "prj_example", WorkItemID: "wi_example"}, State: "Rework"},
				Change:           &tracker.ChangeDetail{Change: tracker.ChangeRequest{ID: "change_1", CurrentVersion: "version_1", UpdatedAt: at.Add(-time.Minute)}, Summary: tracker.ChangeSummary{Status: "reviewed", Checks: "success"}},
				CurrentVersion:   &tracker.NativeChangeVersionEvidence{NativeChangeReference: tracker.NativeChangeReference{ChangeID: "change_1", VersionID: "version_1", HeadSHA: "head"}, PublishedAt: at.Add(-time.Minute), Validation: local, Landing: landing},
				Attempt:          &tracker.NativeAttempt{NativeRunData: tracker.NativeRunData{AttemptID: "attempt_repair", Runtime: &tracker.NativeRuntimeObservation{}}, Status: "running", Current: true, RuntimeFreshness: "live"},
				CurrentLease:     &tracker.NativeLease{ID: "lease_repair"},
				Scheduling:       tracker.NativeSchedulerDecision{Outcome: "claimed", Reason: "A current fenced lease owns this item", At: at.Add(4 * time.Second)},
				LatestTransition: &tracker.CollaborationEvent{ID: "event_refusal", RecordedAt: at.Add(time.Second), Data: tracker.CollaborationData{FromState: "Merging", ToState: "Rework", Reason: "worker_progress", ReasonDetail: finding}},
			}
			if test.edit != nil {
				test.edit(&e)
			}
			if test.pending {
				e.Issue.State = "Merging"
				e.LatestTransition = nil
			}
			got := FromNativeEvidence(e)
			if got.RequiredGate.State != test.want {
				t.Fatalf("gate = %#v, want %s", got.RequiredGate, test.want)
			}
			if test.local && (got.RequiredGate.LocalValidation == nil || got.RequiredGate.LocalValidation.State != GatePassed || got.RequiredGate.LocalValidation.ReceiptID != "receipt_1") {
				t.Fatalf("lost genuine local receipt: %#v", got.RequiredGate)
			}
			if test.refusal {
				merge := got.RequiredGate.ExternalMerge
				if merge == nil || merge.Reason != "base_protected" || merge.Detail != finding || merge.SourceState != SourceLastKnown || merge.Historical || merge.ObservedAt == nil || !merge.ObservedAt.Equal(at) || got.RequiredGate.Reason != finding || len(got.Reasons) != 1 || got.Reasons[0].Detail != finding {
					t.Fatalf("lost actionable refusal: %#v reasons=%#v", got.RequiredGate, got.Reasons)
				}
			}
			if test.pending {
				merge := got.RequiredGate.ExternalMerge
				if merge == nil || merge.State != GatePending || merge.SourceState != SourceLastKnown || merge.Historical || merge.ObservedAt == nil || !merge.ObservedAt.Equal(at) || got.RequiredGate.Reason != landing.Refusal || len(got.RequiredGate.Failures) != 0 || len(got.Reasons) != 0 || got.CurrentLane.Name != "Merging" {
					t.Fatalf("automatic retry described as a blocker: gate=%#v merge=%#v reasons=%#v", got.RequiredGate, merge, got.Reasons)
				}
			}
			if test.want == GateUnknown && (got.RequiredGate.ExternalMerge.State != GateUnknown || len(got.Reasons) != 0) {
				t.Fatalf("stale observation became current: %#v", got.RequiredGate)
			}
			if test.wantReason != "" && got.RequiredGate.Reason != test.wantReason {
				t.Fatalf("unrelated finding attributed to refusal: %#v", got.RequiredGate)
			}
			if test.historical && (got.RequiredGate.ExternalMerge == nil || !got.RequiredGate.ExternalMerge.Historical || len(got.Reasons) != 0) {
				t.Fatalf("old refusal is not historical: %#v reasons=%#v", got.RequiredGate, got.Reasons)
			}
			if got.NativeRuntime.CurrentLease.ID != "lease_repair" || got.Attempt.NativeID != "attempt_repair" || got.Attempt.Status != "running" || got.Eligibility.Current.Outcome != "claimed" {
				t.Fatalf("repair ownership lost: %#v", got)
			}
		})
	}
}
